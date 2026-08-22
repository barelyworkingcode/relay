package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
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
	// McpSurfaceFor is the LIVE declaration: the context schema an MCP
	// published at its last handshake, that schema's version, and the tools it
	// exposes now. Read at call time rather than from the stored grant because
	// the only defence that catches an MCP which grew a scope field AFTER a
	// grant was validated is one that asks the running server (ADR-011
	// decision 4).
	McpSurfaceFor(id string) McpSurface
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
//
// tool is the live definition of toolName, needed for the access-mode check
// below; pass nil for an MCP-level check. A nil tool with a non-empty toolName
// means relay could not find the definition, which under a read grant is a
// denial — see the fail-closed reasoning there.
func checkToolAccess(tok *StoredToken, mcpID, toolName string, tool *mcp.Tool) error {
	// Check MCP-level permission.
	if perm, ok := tok.Permissions[mcpID]; ok && perm == PermOff {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: MCP '%s' is disabled for this token", mcpID))
	}
	if toolName == "" {
		return nil
	}
	// Which tools (ADR-011 decision 2b). An allowlist, checked before the mode
	// and before the denylist: a tool this grant does not name is refused
	// whatever its annotations say and whatever any denylist omits. This is
	// what keeps a profile named for one mailbox out of capture_screenshot,
	// shortcuts_run, web_fetch and the address book — all of which the live
	// "Hermes Mail" enrolment was measured to reach, and none of which the
	// access mode below would have stopped, because they are honestly
	// read-only.
	if !tok.ToolAllowed(mcpID, toolName) {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is not in the allowed tools for MCP '%s'", toolName, mcpID))
	}
	// Which operations (ADR-011 decision 2). Relay applies this rule itself, at
	// its own chokepoint, and what it decided is visible in the audit log and
	// in what ListTools returns. What relay does NOT verify is the input: the
	// classification of a tool as read-only is the MCP's own readOnlyHint, and
	// an MCP that mislabels a mutating tool defeats the mode. That is still
	// meaningfully stronger than the resource layer — a false hint is a lie
	// told in a published tool list an operator can read and diff, whereas an
	// ignored _meta leaves no trace anywhere.
	if tok.AccessMode(mcpID) != AccessWrite {
		if !readOnlyHintTrue(tool) {
			return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is not annotated read-only and this grant is read-only for MCP '%s'", toolName, mcpID))
		}
	}
	// Tool-level disabling. Last, because it can only ever SUBTRACT from what
	// the three allowlists already admitted, and because that is the order in
	// which the layers are easiest to reason about: which MCP, which tools,
	// which operations, then what an operator switched off by hand.
	//
	// Applied to every token kind, not only local ones. validateProjectShape
	// refuses disabled_tools on a remote-kind record — an inert control is
	// worse than none — but a record that acquired one by a route validation
	// did not cover (a hand-edited settings.json) must still have it honoured:
	// ignoring a denylist is the one direction that widens.
	if tok.DisabledTools != nil && slices.Contains(tok.DisabledTools[mcpID], toolName) {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is disabled for this token", toolName))
	}
	return nil
}

// readOnlyHintTrue reports whether a tool's annotations declare
// readOnlyHint: true, EXPLICITLY, as a boolean, and under that exact spelling.
//
// Everything else is "mutating": absent, null, malformed JSON, a string
// "true", a number 1, or false. That is the whole rule and it is what makes
// ADR-011 finding 9 safe — a tool added to an MCP after a grant was written is
// denied to every read-only grant until someone annotates it truthfully,
// rather than silently granted the way a denylist would grant it.
//
// The spelling is read from a map rather than decoded into a struct, and that
// is the point rather than a style choice. encoding/json matches struct fields
// CASE-INSENSITIVELY, so a `ReadOnlyHint *bool` field admitted
// {"ReadOnlyHint":true} and {"readonlyhint":true} — neither of which is the
// key the MCP specification defines — to every read-only grant, while the
// comment above said such blobs were treated as mutating. A map lookup is
// exact, which is what decision 2 asks for: relay's whole claim here is that a
// mode is decided from a declaration an operator can read and diff, and a
// buggy or hostile MCP must not be able to widen a grant with a near-miss key
// that no reviewer reading the spec would recognise as one.
//
// It must never panic on a malformed blob: annotations are server-supplied
// bytes relay has carried unread since the type was written, so this is the
// first code to trust them with anything, and the first thing an MCP could get
// wrong. Unmarshalling the one value into a *bool gives all three answers —
// error, nil, value — without a type switch that could miss a case.
func readOnlyHintTrue(tool *mcp.Tool) bool {
	if tool == nil || len(tool.Annotations) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(tool.Annotations, &fields); err != nil {
		return false
	}
	raw, ok := fields[mcpReadOnlyHintKey]
	if !ok {
		return false
	}
	var hint *bool
	if err := json.Unmarshal(raw, &hint); err != nil {
		return false
	}
	return hint != nil && *hint
}

// mcpReadOnlyHintKey is the annotation key the MCP specification defines, in
// the specification's spelling. It is a constant so the exactness above is one
// value rather than a string literal someone later "tidies".
const mcpReadOnlyHintKey = "readOnlyHint"

// findTool locates a tool definition by name in a list.
func findTool(tools []mcp.Tool, name string) *mcp.Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ToolRouter implementation
// ---------------------------------------------------------------------------

type appRouter struct {
	store    SettingsStore
	tools    ToolManager
	services ServiceReloader
	enhanced *EnhancedServiceRegistry
	onChange func()
	// audit records every tool call, denial, and auth failure that passes
	// through this router. Nil disables auditing entirely — every call site
	// goes through nil-safe helpers, so nothing branches on it.
	audit *AuditRecorder
	// budgets enforces each enrolment's rolling call-rate and result-volume
	// caps for remote callers (ADR-010 decision 7). The zero value enforces —
	// see enrolmentBudgets — so there is nothing to wire up and no way to end
	// up with an unbudgeted router by omission.
	budgets       enrolmentBudgets
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
	_ bridge.ToolRouter = (*appRouter)(nil)
	_ ToolManager       = (*ExternalMcpManager)(nil)
	_ ServiceReloader   = (*ServiceRegistry)(nil)
)

// resolveAuth loads settings and authenticates the given token.
// Checks in-memory service tokens first (full access, no per-MCP permissions),
// then project tokens (inline permissions), then external tokens in settings.
//
// With no token, falls back to directory auth (resolveCwdAuth) — opt-in per
// project. A token that is present but wrong is always a hard failure: the
// fallback must never rescue a bad credential, only the absence of one.
func (r *appRouter) resolveAuth(ctx context.Context, token string) (*StoredToken, *Settings, error) {
	if token == "" {
		return r.resolveCwdAuth(ctx)
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

// resolveCwdAuth authenticates a tokenless caller by the working directory it
// asserted over the bridge. Only projects with AllowCwdAuth participate, and the
// resulting scope is exactly the project's token scope — this identifies a
// caller, it does not widen one. Grants are logged: directory auth has no
// deliberate hand-off to point at afterwards, so the log is the audit trail.
func (r *appRouter) resolveCwdAuth(ctx context.Context) (*StoredToken, *Settings, error) {
	cwd := bridge.CallerCwdFromContext(ctx)
	if cwd == "" {
		return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, ErrNoToken)
	}

	s := r.store.Get()
	stored := s.AuthenticateProjectByPath(cwd)
	if stored == nil {
		slog.Debug("cwd auth rejected", "cwd", cwd)
		return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("no token supplied and working directory %q is not inside a project with directory auth enabled", cwd))
	}
	slog.Info("cwd auth granted", "cwd", cwd, "project", stored.ProjectID, "name", stored.Name)
	return stored, s, nil
}

func (r *appRouter) ListTools(ctx context.Context, token string) (json.RawMessage, error) {
	au := r.beginAudit(ctx, AuditEventListTools)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	isServiceToken := stored.Name == serviceTokenName
	tools := make([]mcp.Tool, 0)

	// External MCP tools.
	for _, ext := range settings.ExternalMcps {
		if !isServiceToken && checkToolAccess(stored, ext.ID, "", nil) != nil {
			continue
		}
		note := newScopeNoter(r, stored, ext.ID, isServiceToken)
		for _, t := range r.tools.Tools(ext.ID) {
			if !isServiceToken && checkToolAccess(stored, ext.ID, t.Name, &t) != nil {
				continue
			}
			note.annotate(&t)
			tools = append(tools, t)
		}
	}

	au.setToolCount(len(tools))
	au.done(AuditOutcomeOK, nil)
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
func (r *appRouter) ListSkillBuckets(ctx context.Context, token string) ([]SkillBucket, error) {
	au := r.beginAudit(ctx, AuditEventListSkills)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	isServiceToken := stored.Name == serviceTokenName
	groups := map[string][]mcp.Tool{}
	for _, ext := range settings.ExternalMcps {
		if !isServiceToken && checkToolAccess(stored, ext.ID, "", nil) != nil {
			continue
		}
		note := newScopeNoter(r, stored, ext.ID, isServiceToken)
		for _, t := range r.tools.Tools(ext.ID) {
			if !isServiceToken && checkToolAccess(stored, ext.ID, t.Name, &t) != nil {
				continue
			}
			// Membership already mirrors ListTools; the scope note has to
			// mirror it too, or the skill renderer would describe a tool
			// surface as unconfined that ListTools describes as confined.
			// appendScopeNote is idempotent, which is what keeps the two
			// paths from double-appending if they ever meet.
			note.annotate(&t)
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
	total := 0
	for _, slug := range order {
		buckets = append(buckets, *bySlug[slug])
		total += len(bySlug[slug].Tools)
	}
	au.setToolCount(total)
	au.done(AuditOutcomeOK, nil)
	return buckets, nil
}

func (r *appRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	// Every tool call in the ecosystem funnels through here, which makes this
	// the one place auditing has to be correct. Note that the refusals are
	// audited too: a denied or unauthenticated call is precisely what a
	// security review is looking for.
	au := r.beginAudit(ctx, AuditEventCallTool)
	au.setTool(name, args)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	isServiceToken := stored.Name == serviceTokenName

	// Check external MCPs.
	extID, extMcp := r.tools.FindToolOwner(name)
	if extMcp == nil {
		err := fmt.Errorf("unknown tool: %s", name)
		au.done(AuditOutcomeError, err)
		return nil, err
	}
	au.setMcp(extID)

	// The MCP's LIVE declaration, read now rather than taken from the stored
	// grant. That is the whole point of the third defence below: a grant is
	// validated once, at edit time, against the schema an MCP published then,
	// and an MCP that grows a restrict-field afterwards would otherwise keep
	// serving every existing grant with no scope at all (ADR-011 decision 4).
	surface := r.tools.McpSurfaceFor(extID)
	schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)

	if !isServiceToken {
		if err := checkToolAccess(stored, extID, name, findTool(r.tools.Tools(extID), name)); err != nil {
			au.done(AuditOutcomeDenied, err)
			return nil, err
		}
		// Presence re-check. `denied` is the right outcome because RELAY made
		// this decision — no MCP was reached, nothing was probed, and there is
		// no result to relay (ADR-011 decision 7). It sits ahead of the budget
		// check because a call with no scope is not a legitimate call whose
		// pattern of use was refused; it is a call the grant does not cover.
		//
		// NOT remote-only, deliberately. Decision 2's asymmetric default
		// (remote reads, local writes) is not extended to scope, because the
		// two are different in kind: a MODE has a defensible default in each
		// direction, and a SCOPE has none — there is no answer to "which
		// mailbox" relay could pick and be right about. One has a safe wrong
		// answer; the other does not. So a local project granted an MCP with an
		// operator-set restrict field must set a value or lose the tools that
		// field governs. A source: "project_path" field is unaffected, because
		// SyncProjectToken derives it for a local project and it is therefore
		// always present.
		if err := checkScopePresence(schema, contextValues(stored.Context[extID]), extID, name); err != nil {
			au.done(AuditOutcomeDenied, err)
			return nil, err
		}
	}

	// Per-enrolment budgets (ADR-010 decision 7). One context lookup decides
	// whether any of this applies: a local caller carries no remote identity,
	// so it takes no lock, keeps no ledger, and is not accounted at all.
	//
	// The rate check sits here — after the grant check, before the intent
	// record and before the MCP — because a throttled call must not invoke the
	// tool. Refusing after the mailbox has been read would interdict nothing.
	// It stays a single audit record rather than an intent/completion pair for
	// the same reason a denial does: no MCP was reached, so there is no
	// side effect for a pre-call record to bracket.
	//
	// `throttled` is distinct from `denied` (a tool the grant never included)
	// and `tool_error` (a boundary inside the MCP) because it is the only one
	// of the three that says the grant was legitimate and the pattern of use
	// was not — which is what exfiltration looks like from the host's side.
	rc, isRemote := bridge.RemoteCallerFromContext(ctx)
	var budget EnrolmentBudget
	if isRemote {
		// Resolved once and reused below, so admission and accounting for one
		// call are always governed by the same numbers even if an operator
		// edits the enrolment mid-call.
		budget = settings.enrolmentBudget(rc)
		if err := r.budgets.admit(rc, budget); err != nil {
			au.done(AuditOutcomeThrottled, err)
			return nil, err
		}
	}

	// Inject per-token context as _meta for this MCP, plus the authenticated
	// project id so an MCP can attribute the call to a project without
	// trusting LLM-supplied values. Relay is the project authority here.
	meta := mergeProjectID(stored.Context[extID], stored.ProjectID)

	// Audit the authority actually in force (ADR-011 decision 7). The grant
	// alone does not answer "was this call confined?" once an operator has
	// since edited the profile, and re-reading settings.json at query time
	// answers a different question. Taken from `meta` — the bytes about to go
	// on the wire — rather than from the project, so what is recorded is what
	// was injected. This runs BEFORE au.intent() so a remote call's intent
	// record carries it.
	if !isServiceToken {
		au.setAuthority(stored.AccessMode(extID), scopeFromMeta(schema, meta))
	}

	// Fail-closed auditing for a remote caller (ADR-010 decision 5). This sits
	// after auth resolution, so the actor is known and the record is
	// attributable, and immediately before the MCP is invoked, so a call that
	// cannot be recorded is refused rather than merely regretted. Local callers
	// are untouched: intent() is a no-op for them and the single fail-open
	// record below is exactly what ADR-008 specified.
	if err := au.intent(); err != nil {
		err = fmt.Errorf("audit: refusing tool call that cannot be recorded: %w", err)
		// Best-effort, over the ordinary fail-open queue: if the sink is broken
		// this will be dropped too, but a transient failure should still leave
		// the refusal visible rather than silent.
		au.done(AuditOutcomeError, err)
		return nil, err
	}

	result, err := r.tools.CallTool(ctx, extID, name, args, meta)
	if isRemote {
		// Volume is charged after the fact because a result's size is not
		// knowable before the MCP answers: this call completes and its bytes
		// count, and the NEXT one is refused once the window is spent. Charged
		// even on error, because bytes that came back left the host whether or
		// not the tool called them a success.
		r.budgets.charge(rc, budget, len(result))
	}
	au.doneResult(result, err)
	return result, err
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

// checkScopePresence is ADR-011 decision 4's third defence: for every
// scope: "restrict" field in the MCP's live schema that governs this tool,
// require a non-empty value in the grant's context.
//
// Absent and empty are both refusals, and that is the whole rule — relay
// writes a non-empty value or it refuses the operation, never a placeholder,
// never [], never null, never the field omitted while the grant stands.
// "No restriction" is deliberately not expressible as emptiness.
//
// A v1 schema is exempt: it declared no scope keywords, so there is nothing
// here to be present.
func checkScopePresence(cs ContextSchema, values map[string]json.RawMessage, mcpID, toolName string) error {
	if !cs.V2() {
		return nil
	}
	for _, f := range cs.GoverningFields(toolName) {
		if hasScopeValue(values, f.Name) {
			continue
		}
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
			"access denied: MCP '%s' scopes tool '%s' by %q and this grant supplies no value for it",
			mcpID, toolName, f.Name))
	}
	return nil
}

// scopeFromMeta extracts the injected scope for the audit record: ONLY the
// fields the MCP declared as scope: "restrict", never the whole context map.
//
// _meta is a general channel and a future MCP may pass an API key through it.
// Logging the map wholesale would make the audit file the place credentials go
// to be archived. Filtering to declared restrict-fields is both safer and
// domain-blind — relay is not deciding which keys look sensitive, it is
// recording only the ones something declared as permissions.
func scopeFromMeta(cs ContextSchema, meta json.RawMessage) map[string]json.RawMessage {
	if !cs.V2() {
		return nil
	}
	injected := contextValues(meta)
	if len(injected) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage)
	for _, f := range cs.RestrictFields() {
		if v, ok := injected[f.Name]; ok {
			out[f.Name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scopeNoter appends ADR-011 decision 8's scope note to the tools in a
// listing. Built once per MCP per listing so the schema is parsed once rather
// than per tool.
//
// A client is told its own limits here because renderBucketSkillMd is the
// wrong ONLY place: access profiles have no skills (validateProjectShape
// refuses GenerateSkill), so the agent this feature exists for would never see
// it. One implementation reaches the remote listener's ListTools,
// `relay mcp call --list`, and ListSkillBuckets.
type scopeNoter struct {
	schema ContextSchema
	values map[string]json.RawMessage
}

func newScopeNoter(r *appRouter, stored *StoredToken, mcpID string, isServiceToken bool) scopeNoter {
	// A service token holds no project context and is not scoped, so there is
	// nothing truthful to say about its limits.
	if isServiceToken || stored == nil {
		return scopeNoter{}
	}
	surface := r.tools.McpSurfaceFor(mcpID)
	cs := ParseContextSchema(surface.Schema, surface.SchemaVersion)
	if !cs.V2() {
		return scopeNoter{}
	}
	return scopeNoter{schema: cs, values: contextValues(stored.Context[mcpID])}
}

func (n scopeNoter) annotate(t *mcp.Tool) {
	if !n.schema.V2() {
		return
	}
	t.Description = appendScopeNote(t.Description, scopeNoteFor(n.schema, n.values, t.Name))
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
// Best-effort: errors are logged, not returned. Called on relay startup and
// after MCP reconcile so generated skills reflect the current tool surface.
// EmitSkills is idempotent — it skips the write when on-disk content already
// matches — so a pass that touches no files is normal, not a no-op failure.
// If the underlying MCP processes have not yet fully initialized, the skill
// picks up the new tools on the next regen trigger (next project save, next
// reconcile, next startup).
func (r *appRouter) regenProjectSkills(ctx context.Context, settings *Settings) {
	processed := 0
	for _, proj := range settings.Projects {
		if !proj.GenerateSkill {
			continue
		}
		dir := projectSkillDir(proj)
		if dir == "" {
			continue
		}
		if _, err := EmitSkills(ctx, r, proj, dir, RegenAlways); err != nil {
			slog.Warn("project skill regen failed", "project", proj.Name, "error", err)
			continue
		}
		processed++
	}
	slog.Info("project skill regen pass", "generate_skill_projects", processed)
}

func (r *appRouter) ReloadService(id string) error {
	settings := r.store.Reload()
	svc, _ := settings.findServiceByID(id)
	if svc == nil {
		slog.Warn("reload: no service found", "id", id)
		return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("no service registered with id %q", id))
	}
	if err := r.services.Reload(id, svc); err != nil {
		slog.Error("failed to reload service", "id", id, "error", err)
		return jsonrpc.NewCodedError(jsonrpc.CodeInternalError, fmt.Errorf("reload service %q: %w", id, err))
	}
	r.onChange()
	return nil
}

// requireServiceToken authenticates a token and rejects anything that isn't
// a service token. Returns CodeUnauthorized on failure with op named in the
// error for caller-friendly logging.
//
// Deliberately resolves against a bare context: service-token operations
// (ResolvePtyEnv, RegisterManifest, project reads) must never be reachable by
// directory auth, which only ever yields a project-scoped token.
func (r *appRouter) requireServiceToken(token, op string) error {
	stored, _, err := r.resolveAuth(context.Background(), token)
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

// ResolvePtyEnv returns the env bundle (project-scoped token + working dir) for
// spawning a project-scoped PTY. Service-token authentication required. Skill
// generation is owned by relay (startup, project save, MCP reconcile, manual
// regen) and is not driven by this call.
//
// RelayToken in the response is the project's plaintext token; the caller
// (relayLLM) must inject it as the project-token env (RELAY_PROJECT_TOKEN) in
// the spawned process and never expose it in argv, files, or logs.
//
// Remote projects are refused outright — see refuseRemotePty.
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
		if err := refuseRemotePty(proj); err != nil {
			return bridge.PtyEnvResponse{}, err
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
		if err := refuseRemotePty(proj); err != nil {
			return bridge.PtyEnvResponse{}, err
		}
	}

	return bridge.PtyEnvResponse{
		RelayToken: proj.Token,
		WorkingDir: proj.Path,
	}, nil
}

// ResolveProjectTemplate returns a project-scoped shell (terminal) launch
// template by (ProjectID, TemplateID). Service-token authentication required.
//
// It returns ONLY the template definition fields (command/args/env/…), never
// the project token: ResolvePtyEnv is the sole plaintext-token egress over the
// bridge and this call must not widen that surface. Do not be tempted to reuse
// GetProject here — that marshals the raw Project including its plaintext token.
//
// relayLLM calls this to spawn a private, project-only shell whose command lives
// in relay's project record rather than relayLLM's global pty map. The launch's
// project token + working dir are still resolved separately via ResolvePtyEnv,
// where the directory-within-project confused-deputy check lives; this call is
// keyed purely on ids and binds no cwd.
func (r *appRouter) ResolveProjectTemplate(ctx context.Context, req bridge.ShellTemplateRequest, token string) (bridge.ShellTemplateResponse, error) {
	if err := r.requireServiceToken(token, "ResolveProjectTemplate"); err != nil {
		return bridge.ShellTemplateResponse{}, err
	}

	proj, _ := r.store.Get().findProjectByID(req.ProjectID)
	if proj == nil {
		return bridge.ShellTemplateResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project_id=%q", req.ProjectID))
	}
	// Validation already keeps ShellTemplates empty on a remote project, so the
	// loop below would fall through to "not found" anyway. Refuse explicitly
	// regardless: a Project constructed directly (a migration, a hand-edited
	// settings.json) could carry templates from a former life as a local
	// project, and resolving one would hand a host launch command to a caller
	// acting for another machine.
	if proj.IsRemote() {
		return bridge.ShellTemplateResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
			fmt.Errorf("project %q is a remote project: shell templates launch a host terminal", proj.ID))
	}
	for _, t := range proj.ShellTemplates {
		if t.ID == req.TemplateID {
			return bridge.ShellTemplateResponse{
				ID:          t.ID,
				Name:        t.Name,
				Command:     t.Command,
				Args:        t.Args,
				Env:         t.Env,
				Description: t.Description,
				Icon:        t.Icon,
			}, nil
		}
	}
	return bridge.ShellTemplateResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("shell template not found: project_id=%q template_id=%q", req.ProjectID, req.TemplateID))
}

// refuseRemotePty rejects a PTY launch bound to a remote project.
//
// This is not merely "a remote project has nothing sensible to run in". Without
// it the request succeeds: dirWithinProject("", "") returns true, because the
// empty-dir branch ("no directory to validate") is checked before the empty-
// project-path branch and short-circuits it. The caller would then receive the
// project's plaintext token with WorkingDir: "", and Go's exec.Cmd treats an
// empty Dir as the PARENT process's working directory — so a host shell would
// come up holding a remote project's credential, rooted wherever relay happens
// to be running. That is exactly the confused-deputy binding the directory
// check exists to prevent, arrived at by a different route.
//
// Refusing here rather than teaching dirWithinProject about kinds keeps that
// helper a pure containment predicate, and keeps the reason visible at the
// place where the token is about to be handed out.
func refuseRemotePty(proj *Project) error {
	if !proj.IsRemote() {
		return nil
	}
	return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
		fmt.Errorf("project %q is a remote project: it has no host directory to launch a terminal in", proj.ID))
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
	// Prefer filesystem identity when both paths exist: os.SameFile compares
	// device + inode, so it sees through case-insensitive volumes (a stored
	// "/users/Jonathan/x" really is the on-disk "/Users/jonathan/x") and any
	// aliasing that string comparison would reject. Only ever adds matches for
	// directories that genuinely ARE the project directory. Falls through to the
	// textual check when either side can't be stat'd — paths that don't exist
	// yet are legitimate here.
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
	cur := filepath.Clean(dir)
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

func (r *appRouter) ReloadExternalMcp(ctx context.Context, id string) error {
	settings := r.store.Reload()
	mcpCfg, _ := settings.findMcpByID(id)
	if mcpCfg == nil {
		slog.Warn("reload: no external MCP found", "id", id)
		return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("no external MCP registered with id %q", id))
	}
	if err := r.tools.Reload(ctx, id, mcpCfg); err != nil {
		slog.Error("failed to reload external MCP", "id", id, "error", err)
		return jsonrpc.NewCodedError(jsonrpc.CodeInternalError, fmt.Errorf("reload external MCP %q: %w", id, err))
	}
	r.onChange()
	return nil
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
