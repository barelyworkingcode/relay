package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"relaygo/bridge"
	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// Interfaces for dependency injection
// ---------------------------------------------------------------------------

// ToolProvider abstracts read-only access to external MCP tool data and invocation.
type ToolProvider interface {
	Tools(id string) []mcp.Tool
	FindToolOwner(name string) (string, *ExternalMcp)
	CallTool(ctx context.Context, id, name string, args, meta json.RawMessage) (json.RawMessage, error)
}

// ToolManager extends ToolProvider with lifecycle operations for reconciling
// and reloading MCP connections.
type ToolManager interface {
	ToolProvider
	Reconcile(ctx context.Context, mcps []ExternalMcp)
	Reload(ctx context.Context, id string, cfg *ExternalMcp) error
}

// ServiceReloader abstracts service restart operations.
type ServiceReloader interface {
	Reload(id string, cfg *ServiceConfig) error
}

// checkToolAccess verifies that the resolved token has permission to access
// the specified MCP and (optionally) tool. Pass empty toolName to check
// only the MCP-level permission. Operates on the StoredToken directly so it
// works for both external tokens (from Tokens[]) and project tokens (inline).
func checkToolAccess(tok *StoredToken, mcpID, toolName string) error {
	// Check MCP-level permission.
	if perm, ok := tok.Permissions[mcpID]; ok && perm == PermOff {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: MCP '%s' is disabled for this token", mcpID))
	}
	// Check tool-level disabling.
	if toolName != "" && tok.DisabledTools != nil {
		if slices.Contains(tok.DisabledTools[mcpID], toolName) {
			return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is disabled for this token", toolName))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ToolRouter implementation
// ---------------------------------------------------------------------------

type appRouter struct {
	store         SettingsStore
	tools         ToolManager
	services      ServiceReloader
	enhanced      *EnhancedServiceRegistry
	onChange      func()
	serviceTokens serviceTokenStore
}

// serviceTokenName identifies service tokens in the Name field.
const serviceTokenName = "service"

// serviceTokenStore holds ephemeral in-memory tokens for managed services.
// Tokens are never persisted — if Relay crashes, both the tokens and the
// services that use them disappear together.
type serviceTokenStore struct {
	mu     sync.Mutex
	hashes map[string]*StoredToken // hash → synthetic StoredToken with full access
}

// Register adds an in-memory service token.
func (s *serviceTokenStore) Register(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashes == nil {
		s.hashes = make(map[string]*StoredToken)
	}
	s.hashes[hash] = &StoredToken{
		Name: serviceTokenName,
		Hash: hash,
	}
}

// Remove deletes an in-memory service token.
func (s *serviceTokenStore) Remove(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hashes, hash)
}

// Lookup checks if a hash matches an in-memory service token.
func (s *serviceTokenStore) Lookup(hash string) *StoredToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hashes[hash]
}

// Len returns the number of registered service tokens. Provides a synchronized
// read so callers (e.g. tests) don't touch the map directly and race the
// reaper's Remove on process exit.
func (s *serviceTokenStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hashes)
}

// Compile-time interface assertions.
var (
	_ bridge.ToolRouter  = (*appRouter)(nil)
	_ ToolManager        = (*ExternalMcpManager)(nil)
	_ ServiceReloader    = (*ServiceRegistry)(nil)
)

// resolveAuth loads settings and authenticates the given token.
// Checks in-memory service tokens first (full access, no per-MCP permissions),
// then project tokens (inline permissions), then external tokens in settings.
func (r *appRouter) resolveAuth(token string) (*StoredToken, *Settings, error) {
	if token == "" {
		return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, ErrNoToken)
	}

	s := r.store.Get()

	// Check ephemeral service tokens first.
	hash := hashToken(token)
	if tok := r.serviceTokens.Lookup(hash); tok != nil {
		return tok, s, nil
	}

	// Check project tokens (inline permissions). Reuse hash from above.
	if stored := s.AuthenticateProjectByHash(hash); stored != nil {
		return stored, s, nil
	}

	return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, ErrInvalidToken)
}

func (r *appRouter) ListTools(_ context.Context, token string) (json.RawMessage, error) {
	stored, settings, err := r.resolveAuth(token)
	if err != nil {
		return nil, err
	}

	isServiceToken := stored.Name == serviceTokenName
	tools := make([]mcp.Tool, 0)

	// External MCP tools.
	for _, ext := range settings.ExternalMcps {
		if !isServiceToken && checkToolAccess(stored, ext.ID, "") != nil {
			continue
		}
		for _, t := range r.tools.Tools(ext.ID) {
			if !isServiceToken && checkToolAccess(stored, ext.ID, t.Name) != nil {
				continue
			}
			tools = append(tools, t)
		}
	}

	return json.Marshal(tools)
}

// ListSkillBuckets groups the token's visible tools into skill buckets for
// skill generation. Membership matches ListTools exactly (same auth + per-MCP
// and per-tool access filtering); the only difference is that this keeps the
// owning MCP in scope so it can group. Bucket key = server-supplied tool
// category if present, else the owning MCP's display name (the name-prefix
// fallback in toolCategory is intentionally NOT used for keys — it produces
// noise like "Generate" from generate_image; uncategorized tools route by
// their MCP instead). Buckets are returned in a deterministic order.
func (r *appRouter) ListSkillBuckets(_ context.Context, token string) ([]SkillBucket, error) {
	stored, settings, err := r.resolveAuth(token)
	if err != nil {
		return nil, err
	}

	isServiceToken := stored.Name == serviceTokenName
	groups := map[string][]mcp.Tool{}
	for _, ext := range settings.ExternalMcps {
		if !isServiceToken && checkToolAccess(stored, ext.ID, "") != nil {
			continue
		}
		for _, t := range r.tools.Tools(ext.ID) {
			if !isServiceToken && checkToolAccess(stored, ext.ID, t.Name) != nil {
				continue
			}
			key := t.Category
			if key == "" {
				key = ext.DisplayName
			}
			groups[key] = append(groups[key], t)
		}
	}

	// Iterate keys in sorted order so slug-collision merges are deterministic
	// (the alphabetically-first key wins as the bucket's display Key).
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	bySlug := map[string]*SkillBucket{}
	order := make([]string, 0, len(keys))
	for _, key := range keys {
		slug := skillSlug(key)
		if b, ok := bySlug[slug]; ok {
			b.Tools = append(b.Tools, groups[key]...)
			continue
		}
		bySlug[slug] = &SkillBucket{Key: key, Slug: slug, Tools: append([]mcp.Tool{}, groups[key]...)}
		order = append(order, slug)
	}

	buckets := make([]SkillBucket, 0, len(order))
	for _, slug := range order {
		buckets = append(buckets, *bySlug[slug])
	}
	return buckets, nil
}

func (r *appRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	stored, _, err := r.resolveAuth(token)
	if err != nil {
		return nil, err
	}

	isServiceToken := stored.Name == serviceTokenName

	// Check external MCPs.
	extID, extMcp := r.tools.FindToolOwner(name)
	if extMcp != nil {
		if !isServiceToken {
			if err := checkToolAccess(stored, extID, name); err != nil {
				return nil, err
			}
		}

		// Inject per-token context as _meta for this MCP, plus the authenticated
		// project id so an MCP can attribute the call to a project without
		// trusting LLM-supplied values. Relay is the project authority here.
		meta := mergeProjectID(stored.Context[extID], stored.ProjectID)

		return r.tools.CallTool(ctx, extID, name, args, meta)
	}

	return nil, fmt.Errorf("unknown tool: %s", name)
}

// mergeProjectID returns base with a top-level "project_id" added when
// projectID is non-empty. base is the per-token _meta context (may be nil). When
// projectID is empty it returns base unchanged, preserving prior behavior for
// service/external tokens. Falls back gracefully if base isn't a JSON object.
func mergeProjectID(base json.RawMessage, projectID string) json.RawMessage {
	if projectID == "" {
		return base
	}
	m := map[string]json.RawMessage{}
	if len(base) > 0 && string(base) != "null" {
		if err := json.Unmarshal(base, &m); err != nil || m == nil {
			m = map[string]json.RawMessage{}
		}
	}
	pid, _ := json.Marshal(projectID)
	m["project_id"] = pid
	out, err := json.Marshal(m)
	if err != nil {
		return base
	}
	return out
}

func (r *appRouter) ValidateAdmin(token string) error {
	s := r.store.Get()
	if len(token) == 0 || subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminSecret)) != 1 {
		return fmt.Errorf("admin authentication failed")
	}
	return nil
}

func (r *appRouter) ReconcileExternalMcps(ctx context.Context) {
	settings := r.store.Reload()
	r.tools.Reconcile(ctx, settings.ExternalMcps)
	r.regenProjectSkills(ctx, settings)
	r.onChange()
}

// regenProjectSkills updates SKILL.md for every project with GenerateSkill: true.
// Best-effort: errors are logged, not returned. Called after MCP reconcile so
// generated skills reflect the new tool surface. If the underlying MCP processes
// have not yet fully initialized, the skill picks up the new tools on the
// next regen trigger (next PTY launch, next project save, next reconcile).
func (r *appRouter) regenProjectSkills(ctx context.Context, settings *Settings) {
	for _, proj := range settings.Projects {
		if !proj.GenerateSkill {
			continue
		}
		dir := projectSkillDir(proj)
		if dir == "" {
			continue
		}
		if _, err := EmitSkills(ctx, r, proj, dir, RegenAlways); err != nil {
			slog.Warn("post-reconcile skill regen failed", "project", proj.Name, "error", err)
		}
	}
}

func (r *appRouter) ReloadService(id string) {
	settings := r.store.Reload()
	svc, _ := settings.findServiceByID(id)
	if svc == nil {
		slog.Warn("reload: no service found", "id", id)
		return
	}
	if err := r.services.Reload(id, svc); err != nil {
		slog.Error("failed to reload service", "id", id, "error", err)
		return
	}
	r.onChange()
}

// requireServiceToken authenticates a token and rejects anything that isn't
// a service token. Returns CodeUnauthorized on failure with op named in the
// error for caller-friendly logging.
func (r *appRouter) requireServiceToken(token, op string) error {
	stored, _, err := r.resolveAuth(token)
	if err != nil {
		return err
	}
	if stored.Name != serviceTokenName {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("%s requires a service token", op))
	}
	return nil
}

// ListProjects returns all projects. Requires a valid service token.
func (r *appRouter) ListProjects(token string) (json.RawMessage, error) {
	if err := r.requireServiceToken(token, "ListProjects"); err != nil {
		return nil, err
	}
	return json.Marshal(r.store.Get().Projects)
}

// GetProject returns a single project by ID. Requires a valid service token.
func (r *appRouter) GetProject(id string, token string) (json.RawMessage, error) {
	if err := r.requireServiceToken(token, "GetProject"); err != nil {
		return nil, err
	}
	proj, _ := r.store.Get().findProjectByID(id)
	if proj == nil {
		return nil, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: %s", id))
	}
	return json.Marshal(proj)
}

// ResolvePtyEnv returns the env bundle (token, working dir, skill path) for
// spawning a project-scoped PTY. As a side effect, regenerates SKILL.md if
// the caller requests it. Service-token authentication required.
//
// RelayToken in the response is the project's plaintext token; the caller
// (relayLLM) must inject it as the project-token env (RELAY_PROJECT_TOKEN) in
// the spawned process and never expose it in argv, files, or logs.
func (r *appRouter) ResolvePtyEnv(ctx context.Context, req bridge.PtyEnvRequest, token string) (bridge.PtyEnvResponse, error) {
	if err := r.requireServiceToken(token, "ResolvePtyEnv"); err != nil {
		return bridge.PtyEnvResponse{}, err
	}

	s := r.store.Get()
	var proj *Project
	if req.ProjectID != "" {
		// Authoritative path: resolve by project id, then validate the requested
		// directory belongs to the project. Without this check a service token
		// could bind an arbitrary cwd to another project's token (confused
		// deputy). Relay is the project authority.
		proj, _ = s.findProjectByID(req.ProjectID)
		if proj == nil {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project_id=%q", req.ProjectID))
		}
		if !dirWithinProject(req.Directory, proj.Path) {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("directory %q is not within project %q", req.Directory, proj.ID))
		}
	} else {
		// Legacy path: resolve by project id/name, or directory match.
		proj = findProjectForPty(s, req.Project, req.Directory)
		if proj == nil {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project=%q directory=%q", req.Project, req.Directory))
		}
	}

	mode := RegenMode(req.RegenSkills)
	if mode == "" {
		mode = RegenNever
	}
	// SkillPath is the skills root (.claude/skills). Tolerate an external PTY
	// template that still points at the legacy per-bucket dir (.../skills/relay
	// or a relay-* dir) by walking up to its parent — relay now manages a set
	// of relay-* dirs under the root, not a single dir.
	skillsRoot := skillsRootFromPath(req.SkillPath)
	if mode != RegenNever {
		if skillsRoot == "" {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("skill_path required when regen_skills != never"))
		}
		if _, err := EmitSkills(ctx, r, *proj, skillsRoot, mode); err != nil {
			return bridge.PtyEnvResponse{}, fmt.Errorf("regen skills: %w", err)
		}
	}

	// Echo the request's SkillPath back unchanged. Consumers (relayLLM's pty
	// spawn) derive the --skill directory as filepath.Dir(SkillPath), expecting
	// the per-bucket path they sent (".../skills/relay"); returning the
	// walked-up root here would make them over-walk to ".../.claude". The
	// walked-up skillsRoot is for our own EmitSkills only.
	return bridge.PtyEnvResponse{
		RelayToken: proj.Token,
		WorkingDir: proj.Path,
		SkillPath:  req.SkillPath,
	}, nil
}

// skillsRootFromPath normalizes an inbound skill_path to the skills root. If
// the path's last element is a relay-managed dir name (legacy "relay" or a
// "relay-*" bucket dir), it returns the parent; otherwise the path itself.
func skillsRootFromPath(p string) string {
	if p == "" {
		return ""
	}
	if isRelayManagedSkillDir(filepath.Base(p)) {
		return filepath.Dir(p)
	}
	return p
}

// findProjectForPty resolves the project for a PTY launch. Eve's terminal_create
// only carries the working directory, so we accept either an explicit project
// identifier (ID or name) or a directory match against Project.Path, in a
// single pass over the project list.
func findProjectForPty(s *Settings, project, directory string) *Project {
	for i := range s.Projects {
		p := &s.Projects[i]
		if project != "" && (p.ID == project || p.Name == project) {
			return p
		}
		if project == "" && directory != "" && p.Path == directory {
			return p
		}
	}
	return nil
}

// dirWithinProject reports whether dir is equal to or nested under projectPath.
// Both are cleaned before comparison. An empty dir means "no directory to
// validate" and returns true — the LLM-provider path may send a project id with
// no cwd. Used to stop a service token from binding an arbitrary working
// directory to a project's token.
func dirWithinProject(dir, projectPath string) bool {
	if dir == "" {
		return true
	}
	if projectPath == "" {
		return false
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

// realpathBestEffort cleans p and resolves symlinks. The path may not exist yet
// (only an ancestor might), so it EvalSymlinks the longest existing prefix and
// re-appends the non-existent tail. This makes a directory and its project
// parent resolve to the same symlink-canonical form regardless of which
// segments exist, so the containment check in dirWithinProject is reliable.
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

func (r *appRouter) ReloadExternalMcp(ctx context.Context, id string) {
	settings := r.store.Reload()
	mcpCfg, _ := settings.findMcpByID(id)
	if mcpCfg == nil {
		slog.Warn("reload: no external MCP found", "id", id)
		return
	}
	if err := r.tools.Reload(ctx, id, mcpCfg); err != nil {
		slog.Error("failed to reload external MCP", "id", id, "error", err)
		return
	}
	r.onChange()
}

// RegisterManifest authenticates the service token then forwards the full
// record to the enhanced-services registry. The registry handles conflict
// detection and triggers an onChange notification so the front-door
// dispatcher rebuilds its routing table.
func (r *appRouter) RegisterManifest(_ context.Context, req bridge.RegisterManifestRequest, token string) error {
	if err := r.requireServiceToken(token, bridge.ReqRegisterManifest); err != nil {
		return err
	}
	if err := r.enhanced.RegisterManifest(req.ServiceID, req.InternalSocket, req.InternalToken, req.Manifest); err != nil {
		return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, err)
	}
	slog.Info("manifest registered",
		"service", req.ServiceID,
		"socket", req.InternalSocket,
		"routes", req.Manifest.Routes,
		"actions", len(req.Manifest.Actions))
	return nil
}
