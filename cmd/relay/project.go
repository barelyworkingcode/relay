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

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/google/uuid"
)

// createProjectWithToken is a thin wrapper over createProjectWithTokenKind,
// kept with its original signature so no existing caller or test has to
// change. Call within store.With.
func createProjectWithToken(s *config.Settings, name, path string, mcpIDs, models []string, templates []config.ChatTemplate, surfaces McpSurfaces) (config.Project, error) {
	return createProjectWithTokenKind(s, config.ProjectKindLocal, name, path, mcpIDs, models, templates, surfaces)
}

// surfaces maps MCP IDs to their runtime schema + tool surface (from
// ExternalMcpManager) for scope derivation. Call within store.With.
func createProjectWithTokenKind(s *config.Settings, kind config.ProjectKind, name, path string, mcpIDs, models []string, templates []config.ChatTemplate, surfaces McpSurfaces) (config.Project, error) {
	kind = config.NormalizeProjectKind(kind)
	if name == "" {
		return config.Project{}, fmt.Errorf("project name is required")
	}
	if mcpIDs == nil {
		mcpIDs = []string{}
	}
	if models == nil {
		models = []string{}
	}
	// GenerateSkill/AllowCwdAuth/ShellTemplates aren't parameters here — they
	// are applied by later mutators in applyProjectCreate — so this candidate
	// only carries what this function actually knows about; a direct caller
	// relying solely on this function (as every pre-remote test does) still
	// gets full path/MCP/model validation.
	candidate := config.Project{Kind: kind, Path: path, AllowedMcpIDs: mcpIDs, AllowedModels: models, ChatTemplates: templates}
	if err := validateProjectShape(&candidate); err != nil {
		return config.Project{}, err
	}
	if err := validateProjectGrants(&candidate, surfaces); err != nil {
		return config.Project{}, err
	}

	plaintext, hash, err := generateProjectToken()
	if err != nil {
		return config.Project{}, err
	}

	proj := config.Project{
		ID:            uuid.New().String(),
		Kind:          kind,
		Name:          name,
		Path:          path,
		AllowedMcpIDs: mcpIDs,
		AllowedModels: models,
		ChatTemplates: templates,
		Token:         config.NewSecret(plaintext),
		TokenHash:     hash,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	s.Projects = append(s.Projects, proj)
	syncProjectToken(s, &s.Projects[len(s.Projects)-1], surfaces)

	return proj, nil
}

// generateProjectToken errors rather than falling back to a weak token if
// the system CSPRNG fails.
func generateProjectToken() (string, string, error) {
	plaintext, err := generateRandomHex(32)
	if err != nil {
		return "", "", err
	}
	return plaintext, config.HashToken(plaintext), nil
}

// validateProjectPath rejects a relative path (interpreted against relay's
// CWD) or one with ".." segments, either of which could escape the
// project's fsMCP allowed_dirs root. Shared by the create and update paths
// (HTTP + IPC) so the rule is enforced identically everywhere.
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

// validateProjectShape is the single point that decides whether a given
// combination of Kind, Path, AllowCwdAuth, GenerateSkill, ShellTemplates,
// AllowedMcpIDs and AllowedModels is coherent — called from both the create
// and update paths so a project can never reach settings.json in a
// self-contradictory shape.
//
// A remote project is a capability grant to a client on another machine,
// not a host directory, so every host-directory-flavored feature below must
// be absent.
func validateProjectShape(proj *config.Project) error {
	// Kind-independent, and checked first: an over-broad allowed_tools entry
	// grants every tool of the MCP for a profile, and is a no-op for a local
	// project — refusing both keeps validation and enforcement the same rule
	// (see validateToolPattern).
	if err := validateAllowedToolPatterns(proj); err != nil {
		return err
	}
	if !proj.IsRemote() {
		return validateProjectPath(proj.Path)
	}
	if proj.Path != "" {
		return fmt.Errorf("remote project must not have a path: %q", proj.Path)
	}
	// A remote caller's cwd is on a different machine; relay cannot compare
	// it against a host path, and a collision would grant a remote client
	// the tool surface of an unrelated local project via a directory guess.
	if proj.AllowCwdAuth {
		return fmt.Errorf("remote project must not enable allow_cwd_auth: directory auth compares a caller's cwd against Path, which a remote project doesn't have")
	}
	// regenProjectSkills silently skips pathless projects, so leaving this
	// flag on would make it an inert toggle that lies about what it does.
	if proj.GenerateSkill {
		return fmt.Errorf("remote project must not enable generate_skill: skills are written under <path>/.claude/skills, and a remote project has no path")
	}
	if len(proj.ShellTemplates) > 0 {
		return fmt.Errorf("remote project must not have shell templates: shell templates launch a terminal on the project's host directory, which a remote project doesn't have")
	}
	// On a local project "*" means every MCP relay currently knows about; on
	// a remote grant it would let registering a new MCP silently widen what
	// the client can reach with no diff to review. An empty list is fine —
	// zero grants is the expected resting state before widening deliberately.
	if config.IsWildcard(proj.AllowedMcpIDs) {
		return fmt.Errorf(`remote project must not use the "*" wildcard for allowed_mcp_ids: it would let a future MCP registration silently widen what the remote client can reach; list MCP IDs explicitly`)
	}
	// A denylist cannot bound a client, and an inert control is worse than
	// none: it reads as a boundary while allowed_tools has already decided
	// everything. Only refused for an MCP this record still grants — a
	// project mid-conversion can carry a leftover entry that
	// SyncProjectToken prunes moments later. (checkToolAccess still honours
	// a denylist that reaches it another way — ignoring one is the only
	// direction that widens.)
	for mcpID, tools := range proj.DisabledTools {
		if len(tools) == 0 {
			continue
		}
		if !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		return fmt.Errorf(`remote project must not set disabled_tools for %q: a denylist grants every tool an MCP gains in future, which is the fail-open shape a grant to another machine must not have — enumerate what it may call in allowed_tools instead`, mcpID)
	}
	// Inert on a record that can hold no session: a permission policy gates
	// a Claude CLI session, and an access profile launches none. Refusing it
	// at the door is more honest than shipping a toggle that quietly no-ops.
	if p := proj.PermissionPolicy; p != nil && !permissionPolicyIsEmpty(p) {
		return fmt.Errorf("remote project must not set permission_policy: those are Claude CLI gates on a session, and an access profile launches none — what bounds a remote client is access, allowed_tools and context")
	}
	// Same argument: a chat template is a preset for starting a chat, and an
	// access profile has no sessions to start.
	if len(proj.ChatTemplates) > 0 {
		return fmt.Errorf("remote project must not have chat templates: a template is a preset for starting a chat session, and an access profile has no sessions to start")
	}
	// modelAllowedForProject treats both len==0 and ["*"] as unrestricted,
	// and remote projects have no model-scoping story yet, so the only safe
	// value is empty.
	if len(proj.AllowedModels) > 0 {
		return fmt.Errorf("remote project must not set allowed_models: an allowlist here would either be misread as unrestricted (see modelAllowedForProject) or need a model-scoping story remote projects don't have yet — leave it empty")
	}
	return nil
}

// validateAllowedToolPatterns walks entries in MCP-name order so a record
// with two bad patterns names the same one every time.
func validateAllowedToolPatterns(proj *config.Project) error {
	for _, mcpID := range sortedKeys(proj.AllowedTools) {
		for _, pattern := range proj.AllowedTools[mcpID] {
			if err := validateToolPattern(mcpID, pattern); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateToolPattern refuses a pattern on two independent grounds.
//
// It does not compile: path.Match rejects an unterminated character class,
// and toolAllowedByPatterns answers "no" for a pattern it cannot compile,
// so an allowlist of nothing but a broken pattern grants nothing — safe,
// but a bad thing to discover only when an agent stops working. (The same
// broken pattern in a context field's applies_to instead governs EVERY
// tool, since fail-closed for a restriction points the other way — which
// is why this refusal lives here and not beside the shared matcher.)
//
// It is too broad: ADR-011 decision 2b requires that registering a tool
// tomorrow not widen a grant made today, which a literal compare against
// "*" does not guarantee — path.Match wildcards like "**" or "*e*" each
// match every tool of an MCP without being the string "*" (a read-only
// "mail" profile written with ["**"] was measured holding 26 tools across
// 11 of macMCP's domains). The wildcard is not replaced by a narrower one:
// "mail_*" is admitted, but "everything this MCP has" is a request an
// allowlist deliberately cannot express.
func validateToolPattern(mcpID, pattern string) error {
	if _, err := matchToolPattern(pattern, toolPatternProbes[0]); err != nil {
		return fmt.Errorf("allowed_tools for %q: pattern %q is not a valid tool pattern (%v); an entry that will not compile matches no tool, so this allowlist would grant less than it reads as", mcpID, pattern, err)
	}
	if reason, over := overBroadToolPattern(pattern); over {
		return fmt.Errorf("allowed_tools for %q: pattern %q is too broad — %s. A tool this MCP gains tomorrow would join the grant with nobody reviewing it, which is the fail-open shape an allowlist exists to close; name the tools, or use a pattern with a real prefix such as %q", mcpID, pattern, reason, "mail_*")
	}
	return nil
}

// permissionPolicyIsEmpty must agree with the update path's rule that an
// emptied policy is stored as nil (applyProjectUpdate) — otherwise
// converting a local project to a profile by clearing its policy would be
// refused for still having one.
func permissionPolicyIsEmpty(p *config.PermissionPolicy) bool {
	return p == nil || (p.DefaultMode == "" && len(p.AllowedTools) == 0 && len(p.DeniedTools) == 0)
}

// validateProjectPermissions refuses an invalid access mode, an uncompilable
// tool pattern, and a context value the MCP's own schema will not stand
// behind. Called from applyProjectCreate and applyProjectUpdate against the
// fully-merged candidate, so every surface is refused identically.
//
// It is separate from validateProjectShape because it needs something that
// function does not have: what the MCP declared at runtime. Shape is
// answerable from the record alone; whether "mail_accounts" is a field
// macMCP has is answerable only from the live surface.
func validateProjectPermissions(proj *config.Project, surfaces McpSurfaces) error {
	// AccessMode already reads anything but exactly "write" as read (fail
	// closed), but a typo like "wrIte" silently narrowing was never
	// surfaced to the operator until here.
	for _, mcpID := range sortedKeys(proj.Access) {
		mode := proj.Access[mcpID]
		if mode != config.AccessRead && mode != config.AccessWrite {
			return fmt.Errorf("access for %q must be %q or %q, not %q", mcpID, config.AccessRead, config.AccessWrite, mode)
		}
	}

	// Not redundant with validateProjectShape's identical check: this runs
	// against the fully-merged candidate reached by both HTTP and IPC.
	if err := validateAllowedToolPatterns(proj); err != nil {
		return err
	}

	for _, mcpID := range sortedKeys(proj.Context) {
		if err := validateProjectContextForMcp(mcpID, proj.Context[mcpID], surfaces); err != nil {
			return err
		}
	}
	return nil
}

// validateProjectContextForMcp checks one MCP's context blob against what
// that MCP declared. Which of three cases applies is decided by the MCP's
// own declaration:
//
//   - v2 schema: every field name must be declared, every value must
//     conform, and a source: "project_path" field is refused outright since
//     relay derives those from the project's path and SyncProjectToken
//     would overwrite any hand-set value on the next resync anyway.
//   - v1 declaration: refused entirely — the v1 branch of SyncProjectToken
//     REPLACES the whole blob with the derived allowed_dirs, so anything
//     stored here would vanish at the next path or MCP edit.
//   - No declaration (relay has never connected to the MCP, or it publishes
//     no contextSchema): permitted with only an emptiness check. Refusing on
//     missing information would make a merely-not-running MCP
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
		// Presence is the only thing checkable for an unknown MCP, and the
		// one that matters: an empty value is how a restrict field refuses
		// everything it governs.
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

// declaredFieldList lists an MCP's fields so a "no such field" refusal
// doesn't leave the operator guessing which names are valid.
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

// sortedKeys orders a map's keys so a refusal naming one of several
// offending entries names the same one every time — Go's map iteration is
// randomised per range.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// authenticateProjectByPath returns nil when dir is empty, matches nothing,
// or matches only projects that have NOT opted into AllowCwdAuth — every
// failure mode is "no access", never "all access". The scope granted is
// identical to the project's token: opting in changes how a caller is
// *identified*, never what the project is allowed to reach.
//
// Nested projects resolve to the most specific match (longest project path
// containing dir), so a project nested inside another wins for its own
// subtree.
func authenticateProjectByPath(s *config.Settings, dir string) *config.StoredToken {
	if dir == "" {
		return nil
	}
	var best *config.Project
	bestLen := -1
	for i := range s.Projects {
		p := &s.Projects[i]
		// Check the opt-in first: a project that hasn't enabled directory
		// auth must not even participate in the longest-match race, or it
		// could shadow an opted-in parent and turn a valid grant into a
		// denial.
		if !p.AllowCwdAuth || p.Path == "" {
			continue
		}
		if !dirWithinProject(dir, p.Path) {
			continue
		}
		if n := len(realpathBestEffort(p.Path)); n > bestLen {
			best, bestLen = p, n
		}
	}
	if best == nil {
		return nil
	}
	return config.StoredTokenForProject(s, best, best.TokenHash)
}
