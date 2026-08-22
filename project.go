package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CreateProjectWithToken generates a new LOCAL project with an inline scoped
// token. Kept with its original signature so no existing caller or test has
// to change; it's a thin wrapper over CreateProjectWithTokenKind. Call within
// store.With.
func (s *Settings) CreateProjectWithToken(name, path string, mcpIDs, models []string, templates []ChatTemplate, surfaces McpSurfaces) (Project, error) {
	return s.CreateProjectWithTokenKind(ProjectKindLocal, name, path, mcpIDs, models, templates, surfaces)
}

// CreateProjectWithTokenKind is CreateProjectWithToken's kind-aware variant.
// The token's permissions, disabled tools, and context are configured based on
// the project's allowedMcpIDs and path. surfaces maps MCP IDs to their runtime
// schema + tool surface (from ExternalMcpManager) for scope derivation.
// Call within store.With.
func (s *Settings) CreateProjectWithTokenKind(kind ProjectKind, name, path string, mcpIDs, models []string, templates []ChatTemplate, surfaces McpSurfaces) (Project, error) {
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
	candidate := Project{Kind: kind, Path: path, AllowedMcpIDs: mcpIDs, AllowedModels: models, ChatTemplates: templates}
	if err := validateProjectShape(&candidate); err != nil {
		return Project{}, err
	}
	if err := s.ValidateProjectGrants(&candidate, surfaces); err != nil {
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
	s.SyncProjectToken(&s.Projects[len(s.Projects)-1], surfaces)

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
	// The same argument one level down (ADR-011 decision 2b). Registering a new
	// TOOL is the same event as registering a new MCP at finer grain, and it
	// happens far more often — macmcp went from 46 tools to 47 during ADR-010's
	// own testing. A pattern is fine ("mail_*"), because the layers compose: a
	// future mail_delete_everything is still refused by a read mode and still
	// confined by mail_accounts, whose applies_to is the same "mail_*". A bare
	// "*" composes with nothing; it is the wildcard again.
	for mcpID, patterns := range proj.AllowedTools {
		for _, pattern := range patterns {
			if pattern == "*" {
				return fmt.Errorf(`remote project must not use the "*" wildcard in allowed_tools for %q: it would let a future tool registration silently widen what the remote client can reach; list tool names or patterns explicitly`, mcpID)
			}
		}
	}
	// A denylist cannot bound a client, and an inert control is worse than no
	// control: it reads on the screen as a boundary and enforces nothing that
	// allowed_tools has not already decided. disabled_tools stays for local
	// projects, where the caller is the same user on the same machine and
	// subtracting from everything is coherent; a profile that sets one is
	// refused here, naming the mechanism that does bound it. (checkToolAccess
	// still honours a denylist that reaches it by some other route — ignoring
	// one is the only direction that widens.)
	//
	// Only for an MCP this record still grants. A local project that had
	// fs_bash auto-disabled and is being converted to remote in the same
	// request that drops the fsmcp grant carries a leftover entry that
	// SyncProjectToken prunes moments later; refusing on that would block a
	// legal conversion on the strength of a map key about to be deleted.
	for mcpID, tools := range proj.DisabledTools {
		if len(tools) == 0 {
			continue
		}
		if !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		return fmt.Errorf(`remote project must not set disabled_tools for %q: a denylist grants every tool an MCP gains in future, which is the fail-open shape a grant to another machine must not have — enumerate what it may call in allowed_tools instead`, mcpID)
	}
	// Both of the next two are inert on a record that can hold no session, and
	// ADR-009 decision 2's rule is that refusing an inert control at the door
	// is more honest than shipping one that quietly no-ops. They were the last
	// two fields on a profile that read on screen as a capability and were not
	// one — the same argument that already removes the path, the skill toggle,
	// the shell templates, the model allowlist and directory auth.
	//
	// A permission policy is a set of gates the CLAUDE CLI applies to a
	// session it launches. An access profile launches none: it has no path to
	// launch in, resolvePtyEnv and resolveProjectTemplate both refuse a remote
	// record outright, and what actually bounds a remote client is the mode,
	// the tool allowlist and the scope. A default_mode of "bypassPermissions"
	// sitting on a profile is the worst of it — it reads as a widening that
	// never happens and cannot be reasoned about from the record alone.
	if p := proj.PermissionPolicy; p != nil && !permissionPolicyIsEmpty(p) {
		return fmt.Errorf("remote project must not set permission_policy: those are Claude CLI gates on a session, and an access profile launches none — what bounds a remote client is access, allowed_tools and context")
	}
	// A chat template is a preset for starting a chat in that project. Same
	// argument, one step further along: relay stores them and eve edits them,
	// and eve has nowhere to offer them for a record with no sessions.
	if len(proj.ChatTemplates) > 0 {
		return fmt.Errorf("remote project must not have chat templates: a template is a preset for starting a chat session, and an access profile has no sessions to start")
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

// permissionPolicyIsEmpty reports whether a policy says nothing at all.
//
// It exists because "empty means clear it" is already the update path's rule
// (applyProjectUpdate stores nil for a policy with no fields set), and a
// refusal that used a DIFFERENT reading of empty would refuse the very request
// that clears one — an operator converting a local project to a profile sends
// the emptied form, and being told "must not set permission_policy" about a
// policy they just emptied is a wall with no door in it.
func permissionPolicyIsEmpty(p *PermissionPolicy) bool {
	return p == nil || (p.DefaultMode == "" && len(p.AllowedTools) == 0 && len(p.DeniedTools) == 0)
}

// ---------------------------------------------------------------------------
// The permission set an operator can now type (ADR-011 decisions 2, 2b, 4, 6)
// ---------------------------------------------------------------------------

// validateProjectPermissions refuses an invalid access mode, an uncompilable
// tool pattern, and a context value the MCP's own schema will not stand behind.
// It is called from applyProjectCreate and applyProjectUpdate against the
// fully-merged candidate, so every surface — the HTTP routes, the IPC handlers
// the tray uses, and any future one — is refused identically.
//
// It exists because of the constraint ADR-011 states as its second: an editor
// whose easiest failure is a confinement that does not confine is operability
// DEFEATING security rather than trading against it. A scope value is typed
// text today, a typo now fails closed (decision 4), and a closed failure is
// silent from the agent's side — so the moment to catch a value that means
// nothing is the moment it is written, not the call that later returns nothing.
//
// It is separate from validateProjectShape because it needs something that
// function does not have: what the MCP declared. Shape is answerable from the
// record alone; whether "mail_accounts" is a field macMCP has, and whether it
// is one an operator may set, is answerable only from the live surface.
func validateProjectPermissions(proj *Project, surfaces McpSurfaces) error {
	// Which operations. AccessMode reads anything that is not exactly "write"
	// as read, so a stored typo is already fail-closed — but a mode of "wrIte"
	// that silently means read is a confinement the operator did not choose,
	// and this is the one place it can still be said out loud.
	for _, mcpID := range sortedKeys(proj.Access) {
		mode := proj.Access[mcpID]
		if mode != AccessRead && mode != AccessWrite {
			return fmt.Errorf("access for %q must be %q or %q, not %q", mcpID, AccessRead, AccessWrite, mode)
		}
	}

	// Which tools. matchToolPattern is path.Match, which rejects an
	// unterminated character class — and toolAllowedByPatterns answers "no" for
	// a pattern it cannot compile, so an allowlist of nothing but a broken
	// pattern grants nothing at all. That is the safe direction and a terrible
	// thing to discover from an agent that stopped working.
	for _, mcpID := range sortedKeys(proj.AllowedTools) {
		for _, pattern := range proj.AllowedTools[mcpID] {
			if _, err := matchToolPattern(pattern, "probe_tool_name"); err != nil {
				return fmt.Errorf("allowed_tools for %q: pattern %q is not a valid tool pattern (%v); it would match no tool at all", mcpID, pattern, err)
			}
		}
	}

	// Which resources.
	for _, mcpID := range sortedKeys(proj.Context) {
		if err := validateProjectContextForMcp(mcpID, proj.Context[mcpID], surfaces); err != nil {
			return err
		}
	}
	return nil
}

// validateProjectContextForMcp checks one MCP's context blob against what that
// MCP declared.
//
// Three cases, and which one applies is decided by the MCP's own declaration:
//
//   - A v2 schema is the case this was written for. Every field name must be
//     one the MCP declares, every value must conform to the declared fragment,
//     and a source: "project_path" field is refused outright — relay derives
//     those from the project's path, so an operator setting one is either
//     confused about what the field is or is trying to widen a bound relay
//     controls. SyncProjectToken would overwrite it on the next resync either
//     way, and a value that silently disappears is worse than a refusal.
//
//   - A v1 declaration is refused entirely, because relay knows enough to know
//     nothing here is operator-set: the v1 branch of SyncProjectToken REPLACES
//     the whole blob with the derived allowed_dirs. Storing a value there means
//     storing one that vanishes at the next path or MCP edit.
//
//   - No declaration at all — an MCP relay has never connected to, or one that
//     publishes no contextSchema — cannot be checked, and is permitted with
//     nothing but an emptiness check. That is the same stance
//     ValidateProjectGrants takes and for the same reason: this is a coherence
//     check an operator sees at edit time, not the boundary. Refusing on
//     missing information would make an MCP that is merely not running
//     unconfigurable, and the call-time presence re-check still denies.
func validateProjectContextForMcp(mcpID string, blob json.RawMessage, surfaces McpSurfaces) error {
	trimmed := strings.TrimSpace(string(blob))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(blob, &values); err != nil {
		return fmt.Errorf("context for %q must be an object of field values", mcpID)
	}
	if len(values) == 0 {
		return nil
	}

	surface := surfaces[mcpID]
	schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)
	names := sortedKeys(values)

	if !schema.V2() {
		if len(surface.Schema) > 0 {
			return fmt.Errorf("context for %q cannot be set here: it declares a v1 context schema, whose only field relay derives from the project's path — a value written here would be replaced on the next resync", mcpID)
		}
		// Unknown MCP. Presence is the only thing that can be checked, and it
		// is the one that matters: an empty value is how a restrict field says
		// "refuse everything I govern".
		for _, name := range names {
			if !hasScopeValue(values, name) {
				return fmt.Errorf("context %q for %q: a non-empty value is required", name, mcpID)
			}
		}
		return nil
	}

	for _, name := range names {
		f, ok := schema.Field(name)
		if !ok {
			return fmt.Errorf("MCP %q declares no context field named %q (it declares: %s)", mcpID, name, declaredFieldList(schema))
		}
		if f.FromProjectPath() {
			return fmt.Errorf("context %q for %q is derived by relay from the project's path and cannot be set by hand", name, mcpID)
		}
		if err := f.ValidateValue(values[name]); err != nil {
			return fmt.Errorf("context for %q: %w", mcpID, err)
		}
	}
	return nil
}

// declaredFieldList names an MCP's declared fields for a refusal. A refusal
// that says a name is wrong without saying which are right is a refusal an
// operator answers by guessing.
func declaredFieldList(cs ContextSchema) string {
	if len(cs.Fields) == 0 {
		return "no fields"
	}
	names := make([]string, 0, len(cs.Fields))
	for _, f := range cs.Fields {
		names = append(names, strconv.Quote(f.Name))
	}
	return strings.Join(names, ", ")
}

// sortedKeys returns a map's keys in name order, so a refusal naming one of
// several offending entries names the same one every time. Go's map iteration
// is randomised per range, and an error message that varies between identical
// requests is one nobody can write a test against.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
