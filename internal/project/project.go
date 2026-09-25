package project

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/google/uuid"
)

// CreateWithToken is a thin wrapper over CreateWithTokenKind,
// kept with its original signature so no existing caller or test has to
// change. Call within store.With.
func CreateWithToken(s *config.Settings, name, path string, mcpIDs, models []string, templates []config.ChatTemplate, surfaces McpSurfaces) (config.Project, error) {
	return CreateWithTokenKind(s, config.ProjectKindLocal, name, path, mcpIDs, models, templates, surfaces)
}

// surfaces maps MCP IDs to their runtime schema + tool surface (from
// mcpbroker.Manager) for scope derivation. Call within store.With.
func CreateWithTokenKind(s *config.Settings, kind config.ProjectKind, name, path string, mcpIDs, models []string, templates []config.ChatTemplate, surfaces McpSurfaces) (config.Project, error) {
	return createWithTokenKind(s, kind, "", name, path, mcpIDs, models, templates, surfaces)
}

// createWithTokenKind is CreateWithTokenKind plus the host the project will
// live on. hostID only informs validation (a host project's path is judged
// by the host's rules, e.g. a Windows drive path); ApplyCreate sets the
// project's HostID itself afterwards.
func createWithTokenKind(s *config.Settings, kind config.ProjectKind, hostID, name, path string, mcpIDs, models []string, templates []config.ChatTemplate, surfaces McpSurfaces) (config.Project, error) {
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
	// GenerateSkill/AllowedTemplates aren't parameters here — they
	// are applied by later mutators in ApplyCreate — so this candidate
	// only carries what this function actually knows about; a direct caller
	// relying solely on this function (as every pre-remote test does) still
	// gets full path/MCP/model validation.
	candidate := config.Project{Kind: kind, HostID: hostID, Path: path, AllowedMcpIDs: mcpIDs, AllowedModels: models, ChatTemplates: templates}
	if err := ValidateShape(&candidate); err != nil {
		return config.Project{}, err
	}
	if err := ValidateGrants(&candidate, surfaces); err != nil {
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
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	plaintext := hex.EncodeToString(b[:])
	return plaintext, config.HashToken(plaintext), nil
}

// validateProjectPath rejects a relative path (interpreted against relay's
// CWD) or one with ".." segments, either of which could escape the
// project's fsMCP allowed_dirs root. Shared by the create and update paths
// (HTTP + IPC) so the rule is enforced identically everywhere. A host
// project's path lives on the host, so a Windows host's drive-letter path
// (C:/… or C:\…) is absolute there even though it is not on the console.
func validateProjectPath(path string, hosted bool) error {
	if path == "" {
		return fmt.Errorf("project path is required")
	}
	if !filepath.IsAbs(path) && !(hosted && isWindowsAbsPath(path)) {
		return fmt.Errorf("project path must be an absolute path: %q", path)
	}
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fmt.Errorf("project path must not contain '..': %q", path)
		}
	}
	return nil
}

// isWindowsAbsPath reports whether path is a drive-letter absolute path:
// a letter, a colon, then / or \.
func isWindowsAbsPath(path string) bool {
	if len(path) < 3 || path[1] != ':' || (path[2] != '/' && path[2] != '\\') {
		return false
	}
	c := path[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// validateAllowedTemplates refuses what could not be placed: a blank entry, and
// a "*" alongside other ids, which would read as a list but mean everything.
func validateAllowedTemplates(ids []string) error {
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("allowed_templates must not hold an empty entry")
		}
		if id == "*" && len(ids) > 1 {
			return fmt.Errorf(`allowed_templates: "*" must be the only entry`)
		}
	}
	return nil
}

// ValidateShape is the single point that decides whether a given
// combination of Kind, Path, GenerateSkill, AllowedTemplates,
// AllowedMcpIDs and AllowedModels is coherent — called from both the create
// and update paths so a project can never reach settings.json in a
// self-contradictory shape.
//
// A remote project is a capability grant to a client on another machine,
// not a host directory, so every host-directory-flavored feature below must
// be absent.
func ValidateShape(proj *config.Project) error {
	// Kind-independent, and checked first: an over-broad allowed_tools entry
	// grants every tool of the MCP for a profile, and is a no-op for a local
	// project — refusing both keeps validation and enforcement the same rule
	// (see validateToolPattern).
	if err := validateAllowedToolPatterns(proj); err != nil {
		return err
	}
	if err := validateAllowedTemplates(proj.AllowedTemplates); err != nil {
		return err
	}
	// Kind-independent like validateAllowedToolPatterns: a local project with
	// non-empty Mounts must be refused, and that refusal lives inside
	// ValidateMounts, not here.
	if err := ValidateMounts(proj); err != nil {
		return err
	}
	// Kind-independent: a host project is kind: local in shape (it is a
	// directory, just not one on the console) and a remote project is a
	// capability grant with no directory at all — the two answer different
	// questions and a record naming both is unrepresentable, not merely
	// unusual.
	if proj.IsHosted() && proj.IsRemote() {
		return fmt.Errorf(`project cannot set both host_id and kind: "remote": a host project is kind: local with its directory on another machine; a remote project is a capability grant with no directory`)
	}
	if !proj.IsRemote() {
		if err := validateProjectPath(proj.Path, proj.IsHosted()); err != nil {
			return err
		}
		return validateHostShape(proj)
	}
	if proj.Path != "" {
		return fmt.Errorf("remote project must not have a path: %q", proj.Path)
	}
	// regenProjectSkills silently skips pathless projects, so leaving this
	// flag on would make it an inert toggle that lies about what it does.
	if proj.GenerateSkill {
		return fmt.Errorf("remote project must not enable generate_skill: skills are written under <path>/.claude/skills, and a remote project has no path")
	}
	if len(proj.AllowedTemplates) > 0 {
		return fmt.Errorf("remote project must not allow templates: templates launch a terminal on the project's host directory, which a remote project doesn't have")
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
		return fmt.Errorf("allowed_tools for %q: pattern %q is not a valid tool pattern (%w); an entry that will not compile matches no tool, so this allowlist would grant less than it reads as", mcpID, pattern, err)
	}
	if reason, over := overBroadToolPattern(pattern); over {
		return fmt.Errorf("allowed_tools for %q: pattern %q is too broad — %s. A tool this MCP gains tomorrow would join the grant with nobody reviewing it, which is the fail-open shape an allowlist exists to close; name the tools, or use a pattern with a real prefix such as %q", mcpID, pattern, reason, "mail_*")
	}
	return nil
}

// permissionPolicyIsEmpty must agree with the update path's rule that an
// emptied policy is stored as nil (ApplyUpdate) — otherwise
// converting a local project to a profile by clearing its policy would be
// refused for still having one.
func permissionPolicyIsEmpty(p *config.PermissionPolicy) bool {
	return p == nil || (p.DefaultMode == "" && len(p.AllowedTools) == 0 && len(p.DeniedTools) == 0)
}

// validateProjectPermissions refuses an invalid access mode, an uncompilable
// tool pattern, and a context value the MCP's own schema will not stand
// behind. Called from ApplyCreate and ApplyUpdate against the
// fully-merged candidate, so every surface is refused identically.
//
// It is separate from ValidateShape because it needs something that
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

	// Not redundant with ValidateShape's identical check: this runs
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

// findDuplicateKey reports the first object key repeated within one object,
// at any depth. Malformed JSON reports none; decoding refuses it elsewhere.
func findDuplicateKey(raw json.RawMessage) (string, bool) {
	type container struct {
		keys          map[string]bool // nil for an array
		awaitingValue bool
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []*container
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		var top *container
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.keys != nil && !top.awaitingValue {
			if key, ok := tok.(string); ok {
				if top.keys[key] {
					return key, true
				}
				top.keys[key] = true
				top.awaitingValue = true
				continue
			}
		}
		if top != nil {
			top.awaitingValue = false
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &container{keys: map[string]bool{}})
		case json.Delim('['):
			stack = append(stack, &container{})
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
	}
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
	// The gate compares Go's decode, which keeps the last of a repeated key,
	// but relay forwards the raw bytes and other parsers keep the first. A
	// repeated key would let the MCP see a value the gate never compared.
	if key, dup := findDuplicateKey(blob); dup {
		return fmt.Errorf("context for %q repeats the key %q; each field may appear once", mcpID, key)
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
			if !HasScopeValue(values, name) {
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

// DirWithin reports whether dir is equal to or nested under projectPath,
// seeing through symlinks and case-insensitive volumes.
//
// This is subtle, and it fails OPEN: an empty dir means "no directory to
// validate" and returns TRUE. A caller that might not have a directory must
// decide what an absent one means BEFORE asking — this function answers a
// containment question, and "nothing to contain" is not a refusal it can
// make on the caller's behalf.
func DirWithin(dir, projectPath string) bool {
	if dir == "" {
		return true
	}
	if projectPath == "" {
		return false
	}
	// Prefer filesystem identity when both paths exist: os.SameFile compares
	// device + inode, so it sees through case-insensitive volumes (a stored
	// "/users/Me/x" really is the on-disk "/Users/me/x"). Falls
	// through to the textual check when either side can't be stat'd -- paths
	// that don't exist yet are legitimate here.
	if within, decided := dirWithinProjectByIdentity(dir, projectPath); decided {
		return within
	}
	// Resolve symlinks on both sides so e.g. macOS /var vs /private/var (or
	// /tmp) don't false-reject a directory that really is inside the project.
	dir = realpathBestEffort(dir)
	projectPath = realpathBestEffort(projectPath)
	if dir == projectPath {
		return true
	}
	rel, err := filepath.Rel(projectPath, dir)
	if err != nil {
		return false
	}
	// rel must stay inside the project: not "..", not "../...", not absolute.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// dirWithinProjectByIdentity walks from dir up to the filesystem root looking
// for the directory that IS projectPath, comparing by device+inode. Returns
// (result, true) once it can answer from the filesystem, or (false, false) when
// the project path can't be stat'd and the caller should fall back to comparing
// text. The walk is bounded by path depth and each step is a single stat.
func dirWithinProjectByIdentity(dir, projectPath string) (within, decided bool) {
	projInfo, err := os.Stat(projectPath)
	if err != nil || !projInfo.IsDir() {
		return false, false
	}
	// This is subtle: dir is resolved through realpathBestEffort BEFORE the
	// walk starts, not just stat'd as it climbs. os.Stat below follows a
	// symlink to decide identity at each step, but filepath.Dir climbs the
	// UNRESOLVED literal path — so a dir whose own leaf component is a
	// symlink pointing outside projectPath would stat to somewhere else
	// (correctly not projInfo), then climb via its literal parent straight
	// back into projectPath's own ancestry on the next iteration, and read
	// as contained. Resolving dir first means every stat in the walk below
	// already reflects where symlinks actually point, so the climb can
	// never re-enter the project through a leaf that only pointed there
	// syntactically.
	cur := realpathBestEffort(dir)
	for {
		info, err := os.Stat(cur)
		if err == nil {
			if os.SameFile(info, projInfo) {
				return true, true
			}
		} else if !os.IsNotExist(err) {
			// Permission trouble or worse: don't claim an answer we can't back up.
			return false, false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without meeting the project directory. The project
			// exists and dir's whole chain was walkable, so this is a real "no".
			return false, true
		}
		cur = parent
	}
}

// realpathBestEffort cleans p and resolves symlinks. The path may not exist yet
// (only an ancestor might), so it EvalSymlinks the longest existing prefix and
// re-appends the non-existent tail. This makes a directory and its project
// parent resolve to the same symlink-canonical form regardless of which
// segments exist, so the containment check in DirWithin is reliable.
func realpathBestEffort(p string) string {
	p = filepath.Clean(p)
	suffix := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return resolved
			}
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root with nothing resolvable
		}
		if suffix == "" {
			suffix = filepath.Base(cur)
		} else {
			suffix = filepath.Join(filepath.Base(cur), suffix)
		}
		cur = parent
	}
}
