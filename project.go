package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CreateProjectWithToken generates a new LOCAL project with an inline scoped
// token. Kept with its original signature so no existing caller or test has
// to change; it's a thin wrapper over CreateProjectWithTokenKind. Call within
// store.With.
func (s *Settings) CreateProjectWithToken(name, path string, mcpIDs, models []string, templates []ChatTemplate, schemas map[string]json.RawMessage) (Project, error) {
	return s.CreateProjectWithTokenKind(ProjectKindLocal, name, path, mcpIDs, models, templates, schemas)
}

// CreateProjectWithTokenKind is CreateProjectWithToken's kind-aware variant.
// The token's permissions, disabled tools, and context are configured based on
// the project's allowedMcpIDs and path. schemas maps MCP IDs to their runtime
// context schemas (from ExternalMcpManager) for filesystem auto-detection.
// Call within store.With.
func (s *Settings) CreateProjectWithTokenKind(kind ProjectKind, name, path string, mcpIDs, models []string, templates []ChatTemplate, schemas map[string]json.RawMessage) (Project, error) {
	// See normalizeProjectKind: a local project's stored Kind is always "",
	// never the literal "local" string, so every local project — however it
	// was created — serializes identically with no "kind" key.
	kind = normalizeProjectKind(kind)
	if name == "" {
		return Project{}, fmt.Errorf("project name is required")
	}
	if mcpIDs == nil {
		mcpIDs = []string{}
	}
	if models == nil {
		models = []string{}
	}
	// Validate the shape (kind-specific invariants) and the grant list
	// (filesystem-scoped MCPs need a path to scope) before anything is
	// persisted. GenerateSkill/AllowCwdAuth/ShellTemplates aren't parameters
	// here — they're applied by later mutators in applyProjectCreate — so this
	// candidate only carries what this function actually knows about; a
	// direct caller relying solely on this function (as every pre-remote test
	// does) still gets full path/MCP/model validation.
	candidate := Project{Kind: kind, Path: path, AllowedMcpIDs: mcpIDs, AllowedModels: models}
	if err := validateProjectShape(&candidate); err != nil {
		return Project{}, err
	}
	if err := s.ValidateProjectGrants(&candidate, schemas); err != nil {
		return Project{}, err
	}

	plaintext, hash, err := generateProjectToken()
	if err != nil {
		return Project{}, err
	}

	proj := Project{
		ID:            uuid.New().String(),
		Kind:          kind,
		Name:          name,
		Path:          path,
		AllowedMcpIDs: mcpIDs,
		AllowedModels: models,
		ChatTemplates: templates,
		Token:         plaintext,
		TokenHash:     hash,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	s.Projects = append(s.Projects, proj)
	s.SyncProjectToken(&s.Projects[len(s.Projects)-1], schemas)

	return proj, nil
}

// generateProjectToken creates a random token and returns plaintext + hash,
// or an error if the system CSPRNG fails (never returns a weak token).
func generateProjectToken() (string, string, error) {
	plaintext, err := generateRandomHex(32)
	if err != nil {
		return "", "", err
	}
	return plaintext, hashToken(plaintext), nil
}

// validateProjectPath rejects project paths that aren't safe to use as a
// filesystem scope. A project's path becomes the fsMCP allowed_dirs root and
// the parent of its relay-managed skills dir, so a relative path (interpreted
// against relay's CWD) or one with ".." traversal segments could escape the
// intended location. Shared by the create and update paths (HTTP + IPC) so the
// rule is enforced identically everywhere.
func validateProjectPath(path string) error {
	if path == "" {
		return fmt.Errorf("project path is required")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("project path must be an absolute path: %q", path)
	}
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".." {
			return fmt.Errorf("project path must not contain '..': %q", path)
		}
	}
	return nil
}

// validateProjectShape enforces the invariants specific to a project's Kind.
// It is the single point that decides whether a given combination of Kind,
// Path, AllowCwdAuth, GenerateSkill, ShellTemplates, AllowedMcpIDs, and
// AllowedModels is coherent — called from both the create and update paths
// (project.go, project_apply.go) so a project can never reach settings.json
// in a self-contradictory shape.
//
// A LOCAL project (the zero value — see the Kind field comment) keeps
// today's rules exactly: validateProjectPath's absolute-path, no-".."
// checks, nothing else.
//
// A REMOTE project is a capability grant to a client on another machine, not
// a host directory, so every host-directory-flavored feature must be absent:
func validateProjectShape(proj *Project) error {
	if !proj.IsRemote() {
		return validateProjectPath(proj.Path)
	}
	// There is no filesystem root to validate — and none to silently invent.
	if proj.Path != "" {
		return fmt.Errorf("remote project must not have a path: %q", proj.Path)
	}
	// A remote caller's cwd is a path on a DIFFERENT machine; relay has no way
	// to compare it against a host project path, and if a host path happened
	// to collide, this would grant a remote client's own directory guess the
	// tool surface of an unrelated local project. Directory auth is
	// meaningless without a directory.
	if proj.AllowCwdAuth {
		return fmt.Errorf("remote project must not enable allow_cwd_auth: directory auth compares a caller's cwd against Path, which a remote project doesn't have")
	}
	// Skill emission writes to <Path>/.claude/skills; regenProjectSkills
	// already silently skips pathless projects, so leaving this flag on would
	// make it an inert toggle that lies about what it does — refuse instead.
	if proj.GenerateSkill {
		return fmt.Errorf("remote project must not enable generate_skill: skills are written under <path>/.claude/skills, and a remote project has no path")
	}
	// Shell templates launch host terminals; there is no host to launch one on.
	if len(proj.ShellTemplates) > 0 {
		return fmt.Errorf("remote project must not have shell templates: shell templates launch a terminal on the project's host directory, which a remote project doesn't have")
	}
	// On a local project "*" is a convenience that always means "every MCP
	// relay currently knows about." On a remote grant it would mean
	// registering a new MCP on the host silently widens what the remote
	// machine can reach — no action taken against the project, no diff to
	// review. A remote grant must be an enumeration someone typed by hand.
	// (An EMPTY list is fine: enrolling a client with zero grants and
	// widening it deliberately later is the expected resting state.)
	if isWildcard(proj.AllowedMcpIDs) {
		return fmt.Errorf(`remote project must not use the "*" wildcard for allowed_mcp_ids: it would let a future MCP registration silently widen what the remote client can reach; list MCP IDs explicitly`)
	}
	// modelAllowedForProject (frontend_model_guard.go) treats both len==0 and
	// ["*"] as "unrestricted" — there is no representation of "no models
	// listed" that means anything other than "every model is allowed" today.
	// A remote project has no scoping story for models yet, so the only safe
	// value is the one that's unambiguous: empty.
	if len(proj.AllowedModels) > 0 {
		return fmt.Errorf("remote project must not set allowed_models: an allowlist here would either be misread as unrestricted (see modelAllowedForProject) or need a model-scoping story remote projects don't have yet — leave it empty")
	}
	return nil
}
