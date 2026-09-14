package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

type ToolProvider interface {
	Tools(id string) []mcp.Tool
	// ToolOwners returns every connected MCP exposing the name, sorted, so
	// resolution happens inside the caller's grant rather than against
	// whichever connection a map iteration reached first — see
	// resolveToolOwner.
	ToolOwners(name string) []string
	CallTool(ctx context.Context, id, name string, args, meta json.RawMessage) (json.RawMessage, error)
	// McpSurfaceFor is the LIVE declaration: the context schema an MCP
	// published at its last handshake, that schema's version, and the tools
	// it exposes now. Read at call time rather than from the stored grant --
	// the only defence that catches an MCP which grew a scope field after a
	// grant was validated is one that asks the running server (ADR-011
	// decision 4).
	McpSurfaceFor(id string) project.McpSurface
}

type ToolManager interface {
	ToolProvider
	Reconcile(ctx context.Context, mcps []config.ExternalMcp)
	Reload(ctx context.Context, id string, cfg *config.ExternalMcp) error
}

type ServiceReloader interface {
	Reload(id string, cfg *config.ServiceConfig) error
}

// checkToolAccess applies the layers in this order deliberately: which MCP,
// then which tools, then which operations, then which side of the host, then
// the denylist. Each later layer can only SUBTRACT from what the earlier
// ones admitted, so a refusal always names the grant that was never given
// rather than a switch an operator flipped further down.
//
// Pass empty toolName for an MCP-level check only. tool is the live
// definition, needed by the mode and outbound-grant checks below to read its
// annotations; pass nil for an MCP-level check. A nil tool with a non-empty
// toolName means relay could not find the definition, which both of those
// checks treat as a denial (see readOnlyHintTrue and toolIsOpenWorld).
func checkToolAccess(tok *config.StoredToken, mcpID, toolName string, tool *mcp.Tool) error {
	if perm, ok := tok.Permissions[mcpID]; ok && perm == config.PermOff {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: MCP '%s' is disabled for this token", mcpID))
	}
	if toolName == "" {
		return nil
	}
	// Which tools (ADR-011 decision 2b), checked before the mode and the
	// denylist: a tool this grant does not name is refused whatever its
	// annotations say, which is what keeps a profile named for one mailbox
	// out of capture_screenshot, shortcuts_run, web_fetch and the address
	// book -- all honestly read-only, so the mode check below would not have
	// stopped any of them.
	if !tok.ToolAllowed(mcpID, toolName) {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is not in the allowed tools for MCP '%s'", toolName, mcpID))
	}
	// Which operations (ADR-011 decision 2). Relay verifies the MCP's own
	// readOnlyHint, not the tool's actual behavior -- an MCP that mislabels a
	// mutating tool defeats the mode. Still stronger than the resource layer:
	// a false hint is a lie in a published tool list an operator can read and
	// diff, where an ignored _meta leaves no trace anywhere.
	if tok.AccessMode(mcpID) != config.AccessWrite {
		if !readOnlyHintTrue(tool) {
			return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is not annotated read-only and this grant is read-only for MCP '%s'", toolName, mcpID))
		}
	}
	// Which side of the host (ADR-011 decision 2c). Orthogonal to the mode
	// above, not a value of it: a tool can be read-only and open-world
	// (web_fetch), or mutating and local (mail_create_draft).
	if !tok.ExternalAllowed(mcpID) {
		if toolIsOpenWorld(tool) {
			return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' reaches outside this host and this grant does not allow external access for MCP '%s'", toolName, mcpID))
		}
	}
	// Applied to every token kind, not only local ones: project.ValidateShape
	// refuses disabled_tools on a remote-kind record, but a record that
	// acquired one by a route validation didn't cover (a hand-edited
	// settings.json) must still have it honoured -- ignoring a denylist is
	// the one direction that widens.
	if tok.DisabledTools != nil && slices.Contains(tok.DisabledTools[mcpID], toolName) {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("access denied: tool '%s' is disabled for this token", toolName))
	}
	return nil
}

// readOnlyHintTrue reports whether a tool's annotations declare
// readOnlyHint: true, EXPLICITLY, as a boolean, under that exact spelling.
// Everything else -- absent, null, malformed JSON, a string "true", a number
// 1, or false -- is "mutating" (ADR-011 finding 9): a tool added to an MCP
// after a grant was written is denied to every read-only grant until
// annotated truthfully, rather than silently granted the way a denylist
// would grant it.
//
// The spelling is read from a map rather than decoded into a struct on
// purpose: encoding/json matches struct fields CASE-INSENSITIVELY, so a
// `ReadOnlyHint *bool` field would admit {"ReadOnlyHint":true} and
// {"readonlyhint":true} -- neither the key the MCP specification defines --
// to every read-only grant. A map lookup is exact.
//
// Must never panic on a malformed blob: annotations are server-supplied
// bytes, so unmarshalling into a *bool gives all three answers -- error,
// nil, value -- without a type switch that could miss a case.
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

const mcpReadOnlyHintKey = "readOnlyHint"

// toolIsOpenWorld reports whether a tool must be treated as reaching outside
// this host.
//
// THE POLARITY IS INVERTED FROM readOnlyHintTrue, AND THAT IS NOT A BUG: the
// two hints have opposite defaults in the MCP specification, and each
// function answers the question that DENIES when the hint is missing.
// readOnlyHint defaults to false (absent means "mutating", so that function
// asks "explicitly true?"); openWorldHint defaults to TRUE (absent means
// "open-world", so this one asks "explicitly false?", and absent answers no
// to that -- i.e. yes to open-world). A later reader who "tidies" this into
// one shared helper with readOnlyHintTrue admits every unannotated tool to
// every grant.
func toolIsOpenWorld(tool *mcp.Tool) bool {
	if tool == nil || len(tool.Annotations) == 0 {
		return true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(tool.Annotations, &fields); err != nil {
		return true
	}
	raw, ok := fields[mcpOpenWorldHintKey]
	if !ok {
		return true
	}
	var hint *bool
	if err := json.Unmarshal(raw, &hint); err != nil {
		return true
	}
	// A null is not a declaration. Absent, null and malformed are one answer.
	return hint == nil || *hint
}

const mcpOpenWorldHintKey = "openWorldHint"

func findTool(tools []mcp.Tool, name string) *mcp.Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

type appRouter struct {
	store    config.SettingsStore
	tools    ToolManager
	services ServiceReloader
	enhanced *EnhancedServiceRegistry
	onChange func()
	// Nil disables auditing entirely -- every call site goes through
	// nil-safe helpers, so nothing branches on it.
	audit *audit.AuditRecorder
	// budgets enforces each enrolment's rolling call-rate and result-volume
	// caps for remote callers (ADR-010 decision 7). The zero value enforces
	// (see enrolment.Budgets), so there is no way to end up with an
	// unbudgeted router by omission.
	budgets enrolment.Budgets
	// launches is the table the service registry records launches in and
	// Hello binds (docs/launch-identity.md). Nil means no caller can hold a
	// launch identity: every Hello is refused and every service operation
	// with it.
	launches *service.Launches

	// The six S5 op cores admin_op dispatches into (ADR-017 implementation
	// spec §7.2). These are the SAME instances the IPC and HTTP doors hold
	// (trayapp.go constructs each once and wires it here too), so a mutation
	// brokered over admin_op carries the same Gate, the same nonce table and
	// the same OnChange as one made from curl or the Settings window — never
	// a second, parallel copy. A nil field here refuses by name
	// (admin_ops.go's requireXxxOps) rather than panicking three calls deep
	// inside a core, which is what a test appRouter that forgot to wire one
	// gets instead of a crash.
	credentialOps *CredentialOps
	enrolmentOps  *EnrolmentOps
	loginOps      *LoginOps
	mcpOps        *McpOps
	serviceOps    *ServiceOps

	// eveEnrolmentOps backs `relay eve enrol` (docs/eve-passkey-enrolment.md).
	// Not one of the six S5 cores above (it predates none of ADR-017's
	// history and has no IPC tab of its own), but wired the same way: the
	// SAME instance trayapp.go's tray menu item and RegisterEveEnrolmentRoutes
	// hold, so opening the window from the CLI, the tray, and answering
	// eve's own status/consume routes can never disagree about whether one
	// is open.
	eveEnrolmentOps *EveEnrolmentOps

	// evePasskeyOps backs `relay eve list|revoke` and eve's own PUT/GET
	// mirror routes (docs/eve-passkey-enrolment.md). The SAME instance the
	// Passkeys tab's eve section and RegisterEvePasskeyRoutes hold, so a
	// revoke from a terminal, the tab, and eve's own report can never
	// disagree about what is pending.
	evePasskeyOps *EvePasskeyOps
}

// serviceIdentityName is the Name of the synthetic StoredToken a launch
// identity holding the projects capability resolves to for ListTools and
// CallTool. It holds every MCP unfiltered; nothing but resolveServiceIdentity
// ever builds one.
const serviceIdentityName = "service"

// identityAllowed returns the launch identity bound to this request's peer
// if that identity may perform op.
func (r *appRouter) identityAllowed(ctx context.Context, op service.Operation) (service.Identity, bool) {
	id, ok := r.launches.Lookup(bridge.CallerPeerFromContext(ctx))
	if !ok || !id.Allows(op) {
		return service.Identity{}, false
	}
	return id, true
}

func (r *appRouter) resolveServiceIdentity(ctx context.Context) *config.StoredToken {
	if _, ok := r.identityAllowed(ctx, service.OpServiceTools); !ok {
		return nil
	}
	return &config.StoredToken{Name: serviceIdentityName}
}

var (
	_ bridge.ToolRouter = (*appRouter)(nil)
	_ ToolManager       = (*mcpbroker.Manager)(nil)
	_ ServiceReloader   = (*service.Registry)(nil)
)

// resolveAuth, in order: a present token is a project token or a hard
// failure; no token from a peer whose identity holds the projects capability
// is that service;
// any other tokenless caller falls back to directory auth (resolveCwdAuth),
// opt-in per project. The fallback must never rescue a bad credential, only
// the absence of one.
func (r *appRouter) resolveAuth(ctx context.Context, token string) (*config.StoredToken, *config.Settings, error) {
	if token == "" {
		if stored := r.resolveServiceIdentity(ctx); stored != nil {
			return stored, r.store.Get(), nil
		}
		return r.resolveCwdAuth(ctx)
	}

	s := r.store.Get()

	hash := config.HashToken(token)
	if stored := s.AuthenticateProjectByHash(hash); stored != nil {
		return stored, s, nil
	}

	return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, config.ErrInvalidToken)
}

// resolveCwdAuth authenticates a tokenless caller by the working directory it
// asserted over the bridge. The resulting scope is exactly the project's
// token scope -- this identifies a caller, it does not widen one. Grants are
// logged: directory auth has no deliberate hand-off to point at afterwards,
// so the log is the audit trail.
func (r *appRouter) resolveCwdAuth(ctx context.Context) (*config.StoredToken, *config.Settings, error) {
	cwd := bridge.CallerCwdFromContext(ctx)
	if cwd == "" {
		return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, config.ErrNoToken)
	}

	s := r.store.Get()
	stored := project.AuthenticateByPath(s, cwd)
	if stored == nil {
		slog.Debug("cwd auth rejected", "cwd", cwd)
		return nil, nil, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized,
			fmt.Errorf("no token supplied and working directory %q is not inside a project with directory auth enabled", cwd))
	}
	slog.Info("cwd auth granted", "cwd", cwd, "project", stored.ProjectID, "name", stored.Name)
	return stored, s, nil
}

// ambiguousToolNames returns the tool names this grant admits on more than
// one connected MCP -- the names CallTool refuses outright, because a bare
// name cannot say which MCP it meant. The listing must agree with dispatch,
// or ListTools would advertise a tool that can never be called and
// ListSkillBuckets would write it into a SKILL.md for an agent to spend
// calls on. Logged rather than only withheld, so a name that vanishes from a
// listing isn't the silent half of the failure; the log fires only for a
// configuration that is already broken.
func (r *appRouter) ambiguousToolNames(stored *config.StoredToken, s *config.Settings, isService bool) map[string]bool {
	owners := map[string]int{}
	for _, ext := range s.ExternalMcps {
		for _, t := range r.tools.Tools(ext.ID) {
			if isService || grantRoutesToolTo(stored, ext.ID, t.Name) {
				owners[t.Name]++
			}
		}
	}
	ambiguous := map[string]bool{}
	names := []string{}
	for name, n := range owners {
		if n > 1 {
			ambiguous[name] = true
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		slog.Warn("withholding tool names this grant admits on more than one MCP; every call to them is refused",
			"project", stored.ProjectID, "tools", names)
	}
	return ambiguous
}

func (r *appRouter) ListTools(ctx context.Context, token string) (json.RawMessage, error) {
	au := r.beginAudit(ctx, audit.AuditEventListTools)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(audit.AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	tools := make([]mcp.Tool, 0)
	for _, listing := range r.listableToolsByMcp(stored, settings) {
		tools = append(tools, listing.tools...)
	}

	au.setToolCount(len(tools))
	au.done(audit.AuditOutcomeOK, nil)
	return json.Marshal(tools)
}

// ListSkillBuckets groups the token's visible tools into skill buckets.
// Membership matches ListTools exactly. Bucket key is the server-supplied
// tool category if present, else the owning MCP's display name -- the
// name-prefix fallback in toolCategory is deliberately NOT used for keys, as
// it produces noise like "Generate" from generate_image.
func (r *appRouter) ListSkillBuckets(ctx context.Context, token string) ([]SkillBucket, error) {
	au := r.beginAudit(ctx, audit.AuditEventListSkills)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(audit.AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	isService := stored.Name == serviceIdentityName
	groups := map[string][]mcp.Tool{}
	ambiguous := r.ambiguousToolNames(stored, settings, isService)
	for _, ext := range settings.ExternalMcps {
		if !isService && checkToolAccess(stored, ext.ID, "", nil) != nil {
			continue
		}
		view := newScopeView(r, stored, ext.ID, isService)
		for _, t := range r.tools.Tools(ext.ID) {
			if !isService && checkToolAccess(stored, ext.ID, t.Name, &t) != nil {
				continue
			}
			// project.AppendScopeNote is idempotent, so the two listing paths cannot
			// double-append a scope note if they ever converge.
			if !view.listable(t.Name) {
				continue
			}
			if ambiguous[t.Name] {
				continue
			}
			view.annotate(&t)
			key := t.Category
			if key == "" {
				key = ext.DisplayName
			}
			groups[key] = append(groups[key], t)
		}
	}

	// Sorted so slug-collision merges are deterministic: the
	// alphabetically-first key wins as the bucket's display Key.
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
	au.done(audit.AuditOutcomeOK, nil)
	return buckets, nil
}

// grantRoutesToolTo reports whether the OPERATOR's own layers admit toolName
// on mcpID -- the predicate that decides which owners of a bare tool name
// this grant could have meant.
//
// It deliberately stops short of the two layers checkToolAccess applies
// after these -- the access mode and the outbound grant -- even though
// either can also refuse the call. Those two are decided from the MCP's OWN
// annotations (readOnlyHint, openWorldHint), and a route must never be a
// function of a value the MCP controls: an MCP that declares
// readOnlyHint: true would otherwise be able to make itself the sole
// candidate for a name a read-only grant admits on nobody else, and capture
// a call the operator meant for another server. The three layers here are
// all things a human typed into settings.json.
func grantRoutesToolTo(tok *config.StoredToken, mcpID, toolName string) bool {
	if checkToolAccess(tok, mcpID, "", nil) != nil {
		return false
	}
	if !tok.ToolAllowed(mcpID, toolName) {
		return false
	}
	return !slices.Contains(tok.DisabledTools[mcpID], toolName)
}

// resolveToolOwner picks which of a tool name's owners this grant means.
//
// A tool name is not unique across MCPs, and the id chosen here also selects
// the `_meta` resource scope, the disabled-tools list, the live schema its
// scope is checked against, and the mcp_id the audit records -- resolving
// the name globally would let Go's map seed decide which confinement
// governed a call. So the grant answers the question: the owners on which
// the OPERATOR's own layers admit this tool are the candidates (see
// grantRoutesToolTo).
//
// More than one candidate is REFUSED rather than resolved: picking one would
// silently apply one MCP's scope to a call the operator may have meant for
// the other's. Service tokens follow the same rule -- they admit every MCP,
// so "pick one at random" is never more correct for them than for anyone
// else.
//
// A zero-candidate name falls through to owners[0] ONLY when that owner is
// granted at the MCP level (just refused at the tool/mode/disabled layer):
// that lets checkToolAccess below write its specific reason (wrong tool,
// wrong mode, hand-disabled) instead of a generic one, without naming an MCP
// outside the grant. When no owner is granted even at the MCP level, the
// call is refused here, in terms of the grant's own MCPs -- never by naming
// an MCP the caller was never granted.
func resolveToolOwner(stored *config.StoredToken, isService bool, toolName string, owners []string, granted []string) (string, error) {
	var candidates []string
	for _, id := range owners {
		if isService || grantRoutesToolTo(stored, id, toolName) {
			candidates = append(candidates, id)
		}
	}
	switch {
	case len(candidates) == 1:
		return candidates[0], nil
	case len(candidates) > 1:
		return "", jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
			"access denied: tool '%s' is exposed by more than one MCP this grant allows (%s); relay will not choose between them — narrow the grant to one of them",
			toolName, strings.Join(candidates, ", ")))
	}
	for _, id := range owners {
		if checkToolAccess(stored, id, "", nil) == nil {
			return id, nil
		}
	}
	return "", noGrantedOwnerError(toolName, granted)
}

// noGrantedOwnerError names the MCPs the caller already knows it holds --
// never the outside MCP that actually publishes the name, which
// resolveToolOwner never even reveals to this function.
func noGrantedOwnerError(toolName string, granted []string) error {
	if len(granted) == 0 {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
			"access denied: no tool named '%s' is available to this grant", toolName))
	}
	return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
		"access denied: no tool named '%s' is available to this grant (granted: %s)",
		toolName, strings.Join(granted, ", ")))
}

// grantedMcpIDsForToken lists, sorted, the MCPs this grant admits at the MCP
// level -- the set a noGrantedOwnerError refusal is allowed to name, since
// the caller already knows it holds them.
func grantedMcpIDsForToken(stored *config.StoredToken, isService bool, s *config.Settings) []string {
	var ids []string
	for _, ext := range s.ExternalMcps {
		if isService || checkToolAccess(stored, ext.ID, "", nil) == nil {
			ids = append(ids, ext.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// CallTool is the one chokepoint every tool call funnels through, including
// refused ones -- a denied or unauthenticated call is precisely what a
// security review is looking for, so it is audited too.
func (r *appRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	au := r.beginAudit(ctx, audit.AuditEventCallTool)
	au.setTool(name, args)

	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		au.setUnauthenticated(ctx, token)
		au.done(audit.AuditOutcomeUnauthorized, err)
		return nil, err
	}
	au.setActor(ctx, stored, settings, token)

	isService := stored.Name == serviceIdentityName

	owners := r.tools.ToolOwners(name)
	// An MCP that is connected but absent from settings.ExternalMcps gets no
	// PermOff entry and would read as GRANTED to every token -- the deny-set
	// a grant resolves into is built by walking settings.ExternalMcps, so a
	// live connection relay holds no configuration for is not a candidate
	// for anything.
	owners = slices.DeleteFunc(owners, func(id string) bool {
		return !slices.ContainsFunc(settings.ExternalMcps, func(m config.ExternalMcp) bool { return m.ID == id })
	})
	if len(owners) == 0 {
		err := fmt.Errorf("unknown tool: %s", name)
		au.done(audit.AuditOutcomeError, err)
		return nil, err
	}
	// Resolved BEFORE au.setMcp and everything below it: the schema read,
	// the _meta assembly, the scope checks and the audit's mcp_id all take a
	// single resolved MCP as given, and an ambiguous name has no such thing
	// to give them.
	extID, err := resolveToolOwner(stored, isService, name, owners, grantedMcpIDsForToken(stored, isService, settings))
	if err != nil {
		au.done(audit.AuditOutcomeDenied, err)
		return nil, err
	}
	au.setMcp(extID)

	// The MCP's LIVE declaration, read now rather than taken from the stored
	// grant: a grant is validated once, at edit time, against the schema an
	// MCP published then, and an MCP that grows a restrict-field afterwards
	// would otherwise keep serving every existing grant with no scope at all
	// (ADR-011 decision 4).
	surface := r.tools.McpSurfaceFor(extID)
	schema := project.ParseContextSchema(surface.Schema, surface.SchemaVersion)

	au.setMcpRoot(surface.Root)

	// project.FilterKnownContextFields drops any stored value under a field name the
	// LIVE schema no longer declares: an MCP can rename or drop a field
	// between when a grant was written and when a call runs, and relay never
	// rewrites settings.json to match, so an unfiltered value stored under
	// an old name could be picked up by unrelated new logic reusing that
	// name. Only reached once extID has resolved to a live connection, so
	// this is never the "MCP is merely down" case that makes pruning stored
	// data unsafe.
	meta := mergeProjectID(project.FilterKnownContextFields(stored.Context[extID], schema), stored.ProjectID)
	meta = mergeArgsSHA256(meta, bridge.ArgsSHA256FromContext(ctx))

	// Audited BEFORE the first thing that can refuse (ADR-011 decision 7),
	// taken from `meta` -- the bytes that would go on the wire -- rather
	// than from the project, so a `denied` or `throttled` record still shows
	// which mode and scope the call was judged against, not only a
	// permitted one.
	if !isService {
		au.setAuthority(stored.AccessMode(extID), stored.ExternalAllowed(extID), scopeFromMeta(schema, meta))

		// A declaration relay could not read. Checked before every other
		// layer: a fragment that would not decode may have been the
		// restrict field governing this very tool, so "nothing governs it"
		// is not a finding, it is the absence of one.
		if !schema.Usable() {
			err := jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
				"access denied: MCP '%s' publishes a context schema relay cannot read, so no grant on it can be enforced (%s)",
				extID, schema.MalformedReason()))
			au.done(audit.AuditOutcomeDenied, err)
			return nil, err
		}

		// A scope the OPERATOR wrote that relay cannot place in the MCP's
		// live schema. Checked immediately after Usable() as the same
		// finding from the other end -- there, relay cannot read what the
		// MCP declared; here, it cannot place what the operator declared --
		// and both get the same answer: nothing is handed over. Ahead of
		// the tool check because this is the layer that decides whether
		// relay is in a position to make statements about this MCP's
		// boundaries at all.
		if unplaced := project.UnplaceableContextFields(schema, project.ContextValues(stored.Context[extID])); len(unplaced) > 0 {
			au.setUnplacedScope(unplaced)
			err := jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
				"access denied: this grant scopes MCP '%s' by %s, which '%s' does not declare in its live context schema — relay cannot enforce a scope it cannot place, so no call to this MCP is dispatched under this grant",
				extID, project.QuoteNames(unplaced), extID))
			au.done(audit.AuditOutcomeDenied, err)
			return nil, err
		}

		if err := checkToolAccess(stored, extID, name, findTool(r.tools.Tools(extID), name)); err != nil {
			au.done(audit.AuditOutcomeDenied, err)
			return nil, err
		}
		// Presence re-check, ahead of the budget check: a call with no
		// scope is not a legitimate call whose pattern of use was refused,
		// it is a call the grant does not cover.
		//
		// NOT remote-only, deliberately: decision 2's asymmetric default
		// (remote reads, local writes) is not extended to scope, because a
		// MODE has a defensible default in each direction and a SCOPE does
		// not -- there is no answer to "which mailbox" relay could pick and
		// be right about. So a local project granted an MCP with an
		// operator-set restrict field must set a value or lose the tools
		// that field governs.
		//
		// A scope this record's KIND can never supply (ADR-011 decision 5)
		// is checked first, as a different finding with a different answer:
		// not "set a value" but "this grant can never hold one". Refusing
		// rather than stripping: for a v1 filesystem-scoped MCP an ABSENT
		// allowed_dirs is what fsMCP reads as unrestricted, so removing the
		// value and letting the call through would turn a forged
		// confinement into no confinement.
		if f, unsatisfiable := project.UnsatisfiableScopeField(schema, stored.IsRemote(), name); unsatisfiable {
			err := jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
				"access denied: MCP '%s' scopes tool '%s' by %q, which relay derives from a project's directory — an access profile has none, so no value for it can be authentic and this tool can never be called under this grant",
				extID, name, f.Name))
			au.done(audit.AuditOutcomeDenied, err)
			return nil, err
		}
		if err := checkScopePresence(schema, project.ContextValues(stored.Context[extID]), extID, name); err != nil {
			au.done(audit.AuditOutcomeDenied, err)
			return nil, err
		}
	}

	// Per-enrolment budgets (ADR-010 decision 7): a local caller carries no
	// remote identity, so it is not accounted at all. The rate check sits
	// here -- after the grant check, before the MCP -- because a throttled
	// call must not invoke the tool; refusing after the tool has run would
	// interdict nothing.
	rc, isRemote := bridge.RemoteCallerFromContext(ctx)
	var budget config.EnrolmentBudget
	if isRemote {
		// Resolved once and reused below, so admission and accounting for
		// one call are always governed by the same numbers even if an
		// operator edits the enrolment mid-call.
		budget = enrolment.BudgetFor(settings, rc)
		if err := r.budgets.Admit(rc, budget); err != nil {
			au.done(audit.AuditOutcomeThrottled, err)
			return nil, err
		}
	}

	// Fail-closed auditing for a remote caller (ADR-010 decision 5),
	// immediately before the MCP is invoked, so a call that cannot be
	// recorded is refused rather than merely regretted. A no-op for local
	// callers.
	if err := au.intent(); err != nil {
		err = fmt.Errorf("audit: refusing tool call that cannot be recorded: %w", err)
		au.done(audit.AuditOutcomeError, err)
		return nil, err
	}

	result, err := r.tools.CallTool(ctx, extID, name, args, meta)
	if isRemote {
		// Charged after the fact because a result's size is not knowable
		// before the MCP answers, and even on error: bytes that came back
		// left the host whether or not the tool called them a success.
		r.budgets.Charge(rc, budget, len(result))
	}
	au.doneResult(result, err)
	return result, err
}

// mergeArgsSHA256 places the client's argument hash on the outgoing _meta,
// verbatim; relay does not check it (see bridge.RemoteRequest.ArgsSHA256).
func mergeArgsSHA256(base json.RawMessage, sum string) json.RawMessage {
	if sum == "" {
		return base
	}
	m := map[string]json.RawMessage{}
	if len(base) > 0 && string(base) != "null" {
		if err := json.Unmarshal(base, &m); err != nil || m == nil {
			m = map[string]json.RawMessage{}
		}
	}
	encoded, err := json.Marshal(sum)
	if err != nil {
		return base
	}
	m["args_sha256"] = encoded
	out, err := json.Marshal(m)
	if err != nil {
		return base
	}
	return out
}

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

// checkScopePresence requires a value in the grant's context for every
// scope: "restrict" field the live schema declares for this tool. Absent is
// a refusal -- "no restriction" is deliberately not expressible by omitting
// the field. A v1 schema is exempt: it declares no scope keywords.
//
// A present, explicit EMPTY value is not a refusal here, since the ADR-011
// addendum ("A star and an empty array"): it is the confirmed-empty grant,
// distinct from the field being unset, and denying it at this gate would
// mean a well-behaved MCP's own correct handling of `.confirmedEmpty` (an
// ordinary, successful empty result) is never reached at all -- the call
// dies here first, under a DIFFERENT refusal (`access denied`, not the
// MCP's own answer). project.HasScopeAssertion is the shared question; see
// it for why this is not the same function `dependencyValues` uses for the
// picker.
func checkScopePresence(cs project.ContextSchema, values map[string]json.RawMessage, mcpID, toolName string) error {
	if !cs.V2() {
		return nil
	}
	for _, f := range cs.GoverningFields(toolName) {
		if project.HasScopeAssertion(values, f.Name) {
			continue
		}
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf(
			"access denied: MCP '%s' scopes tool '%s' by %q and this grant supplies no value for it",
			mcpID, toolName, f.Name))
	}
	return nil
}

// scopeFromMeta extracts the injected scope for the audit record: ONLY the
// fields the MCP declared as scope: "restrict", never the whole context map
// -- _meta is a general channel and a future MCP may pass an API key
// through it, so logging it wholesale would make the audit file a place
// credentials go to be archived.
//
// Returns nil ONLY when the live schema declares no restrict field at all.
// Whenever it declares at least one, this returns a map even if empty,
// because "declared, but this call's grant supplied nothing" is itself the
// finding a `denied` record exists to carry; collapsing that to nil would
// make it indistinguishable from an MCP with no scope concept at all. Fields
// come from project.AuditedScopeFields rather than RestrictFields so a v1 MCP whose
// confinement relay itself derives is still answerable as "was this call
// confined?" rather than recorded as `scope: null`.
func scopeFromMeta(cs project.ContextSchema, meta json.RawMessage) map[string]json.RawMessage {
	fields := project.AuditedScopeFields(cs)
	if len(fields) == 0 {
		return nil
	}
	injected := project.ContextValues(meta)
	out := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if v, ok := injected[f.Name]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// scopeView is what a listing has to know about one MCP's scope to describe
// it the way CallTool will judge it -- built once per MCP per listing so the
// schema is parsed once rather than per tool. It answers both whether a tool
// is listable and what scope note to attach, as one type, so ListTools and
// ListSkillBuckets cannot answer either question differently.
type scopeView struct {
	schema   project.ContextSchema
	values   map[string]json.RawMessage
	isRemote bool
	// scoped is false for a service token, which holds no project context
	// and is not scoped at all: nothing truthful to say about its limits and
	// nothing to withhold from it.
	scoped bool
}

func newScopeView(r *appRouter, stored *config.StoredToken, mcpID string, isService bool) scopeView {
	if isService || stored == nil {
		return scopeView{}
	}
	surface := r.tools.McpSurfaceFor(mcpID)
	return scopeView{
		schema:   project.ParseContextSchema(surface.Schema, surface.SchemaVersion),
		values:   project.ContextValues(stored.Context[mcpID]),
		isRemote: stored.IsRemote(),
		scoped:   true,
	}
}

// listable withholds exactly what CallTool refuses UNCONDITIONALLY, and
// nothing else (ADR-011 decisions 4 and 5): a value that is not set YET
// stays listed, with the loud `denied` naming the missing field more
// diagnostic than silent absence; a value that can NEVER be set -- a
// source: "project_path" field on an access profile, or a v1 filesystem MCP
// granted to one -- is withheld, since no configuration makes that tool
// work and `relayremote skill` would otherwise write it into a SKILL.md the
// agent plans around. A schema relay could not read, or a scope this grant
// sets that relay cannot place in it, withholds every tool on the MCP,
// matching CallTool's unconditional refusal.
func (v scopeView) listable(toolName string) bool {
	if !v.scoped {
		return true
	}
	if !v.schema.Usable() {
		return false
	}
	if len(project.UnplaceableContextFields(v.schema, v.values)) > 0 {
		return false
	}
	_, unsatisfiable := project.UnsatisfiableScopeField(v.schema, v.isRemote, toolName)
	return !unsatisfiable
}

func (v scopeView) annotate(t *mcp.Tool) {
	if !v.scoped || !v.schema.V2() {
		return
	}
	t.Description = project.AppendScopeNote(t.Description, project.ScopeNoteFor(v.schema, v.values, t.Name))
}

func (r *appRouter) ValidateAdmin(token string) error {
	s := r.store.Get()
	// A degraded sealed store (§5.6) has no admin_secret to compare
	// against, so this fails closed exactly like an empty token would.
	adminSecret, ok := s.AdminSecret.Reveal()
	if len(token) == 0 || !ok || subtle.ConstantTimeCompare([]byte(token), []byte(adminSecret)) != 1 {
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

// regenProjectSkills updates SKILL.md for every project with
// GenerateSkill: true. Best-effort: errors are logged, not returned.
// EmitSkills is idempotent -- it skips the write when on-disk content
// already matches -- so a pass that touches no files is normal, not a
// no-op failure.
func (r *appRouter) regenProjectSkills(ctx context.Context, settings *config.Settings) {
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
	svc, _ := config.FindServiceByID(settings, id)
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

// requireServiceIdentity admits only a tokenless caller whose peer's launch
// identity may perform op. It deliberately never consults resolveAuth: a
// present token can only be a project token, and directory auth only ever
// yields a project, so neither may reach a service operation.
func (r *appRouter) requireServiceIdentity(ctx context.Context, token string, op service.Operation) (service.Identity, error) {
	if token != "" {
		return service.Identity{}, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("%s requires the calling service's launch identity, not a token", op))
	}
	id, ok := r.identityAllowed(ctx, op)
	if !ok {
		return service.Identity{}, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("%s requires a launch identity holding that capability", op))
	}
	return id, nil
}

// Hello binds a launch secret to the caller's peer audit token. The result
// recognises the caller and carries no credential.
func (r *appRouter) Hello(ctx context.Context, name, secret string) (bridge.HelloResult, error) {
	if r.launches == nil {
		return bridge.HelloResult{}, fmt.Errorf("%w: no launch table", service.ErrHelloRefused)
	}
	id, err := r.launches.Bind(name, secret, bridge.CallerPeerFromContext(ctx))
	if err != nil {
		return bridge.HelloResult{}, err
	}
	slog.Info("launch identity bound", "kind", id.Kind, "name", id.Name, "pid", id.Process.PID, "capabilities", id.Capabilities)
	return bridge.HelloResult{Kind: string(id.Kind), ServiceID: id.Name, RelayPID: os.Getpid()}, nil
}

// ListProjects and GetProject answer through projectToView/projectsToView —
// the same allow-list the eve-facing HTTP routes project through
// (project_dto.go) — rather than marshalling the raw Project. Marshalling
// Project directly would hand any service holding the projects capability every project's
// plaintext token: ResolvePtyEnv below is the sole plaintext-token egress
// over the bridge, and no other bridge response may carry one.
func (r *appRouter) ListProjects(ctx context.Context, token string) (json.RawMessage, error) {
	if _, err := r.requireServiceIdentity(ctx, token, service.OpListProjects); err != nil {
		return nil, err
	}
	return json.Marshal(projectsToView(r.store.Get().Projects))
}

func (r *appRouter) GetProject(ctx context.Context, id string, token string) (json.RawMessage, error) {
	if _, err := r.requireServiceIdentity(ctx, token, service.OpGetProject); err != nil {
		return nil, err
	}
	proj, _ := config.FindProjectByID(r.store.Get(), id)
	if proj == nil {
		return nil, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: %s", id))
	}
	return json.Marshal(projectToView(*proj))
}

// DescribeProject lets a process launched holding only RELAY_PROJECT_TOKEN
// configure itself from the grant relay enforces, rather than from a second
// copy of it on disk. A tokenless caller is refused rather than resolved by
// directory auth or by launch identity, since a service names no project.
func (r *appRouter) DescribeProject(ctx context.Context, token string) (bridge.ProjectDescription, error) {
	if token == "" {
		return bridge.ProjectDescription{}, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("DescribeProject requires a project token"))
	}
	stored, settings, err := r.resolveAuth(ctx, token)
	if err != nil {
		return bridge.ProjectDescription{}, err
	}
	if stored.Name == serviceIdentityName || stored.ProjectID == "" {
		return bridge.ProjectDescription{}, jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("DescribeProject requires a project token"))
	}
	proj, _ := config.FindProjectByID(settings, stored.ProjectID)
	if proj == nil {
		return bridge.ProjectDescription{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: %s", stored.ProjectID))
	}

	kind := config.ProjectKindLocal
	if proj.IsRemote() {
		kind = config.ProjectKindRemote
	}
	out := bridge.ProjectDescription{
		ID:            proj.ID,
		Name:          proj.Name,
		Kind:          string(kind),
		Path:          proj.Path,
		HostID:        proj.HostID,
		AllowedModels: append([]string{}, proj.AllowedModels...),
		Mcps:          []bridge.ProjectMcpDescription{},
	}
	for _, listing := range r.listableToolsByMcp(stored, settings) {
		names := make([]string, 0, len(listing.tools))
		for _, t := range listing.tools {
			names = append(names, t.Name)
		}
		out.Mcps = append(out.Mcps, bridge.ProjectMcpDescription{
			ID:     listing.mcpID,
			Access: stored.AccessMode(listing.mcpID),
			Root:   r.tools.McpSurfaceFor(listing.mcpID).Root,
			Tools:  names,
		})
	}
	return out, nil
}

type mcpListing struct {
	mcpID string
	tools []mcp.Tool
}

// listableToolsByMcp is ListTools' membership rule grouped by owning MCP, in
// settings order. DescribeProject reads the same grouping, so the tools a
// description names can never differ from the tools ListTools lists.
func (r *appRouter) listableToolsByMcp(stored *config.StoredToken, settings *config.Settings) []mcpListing {
	isService := stored.Name == serviceIdentityName
	ambiguous := r.ambiguousToolNames(stored, settings, isService)

	var out []mcpListing
	for _, ext := range settings.ExternalMcps {
		if !isService && checkToolAccess(stored, ext.ID, "", nil) != nil {
			continue
		}
		view := newScopeView(r, stored, ext.ID, isService)
		listing := mcpListing{mcpID: ext.ID}
		for _, t := range r.tools.Tools(ext.ID) {
			if !isService && checkToolAccess(stored, ext.ID, t.Name, &t) != nil {
				continue
			}
			if !view.listable(t.Name) {
				continue
			}
			if ambiguous[t.Name] {
				continue
			}
			view.annotate(&t)
			listing.tools = append(listing.tools, t)
		}
		out = append(out, listing)
	}
	return out
}

// ResolvePtyEnv returns the env bundle (project-scoped token + working dir)
// for spawning a project-scoped PTY. RelayToken is the project's plaintext
// token; the caller (relayLLM) must inject it as RELAY_PROJECT_TOKEN and
// never expose it in argv, files, or logs. Remote projects are refused
// outright -- see refuseRemotePty.
func (r *appRouter) ResolvePtyEnv(ctx context.Context, req bridge.PtyEnvRequest, token string) (bridge.PtyEnvResponse, error) {
	if _, err := r.requireServiceIdentity(ctx, token, service.OpResolvePtyEnv); err != nil {
		return bridge.PtyEnvResponse{}, err
	}

	s := r.store.Get()
	var proj *config.Project
	if req.ProjectID != "" {
		// Validating that the requested directory belongs to the project
		// matters: without it a service token could bind an arbitrary cwd
		// to another project's token (confused deputy).
		proj, _ = config.FindProjectByID(s, req.ProjectID)
		if proj == nil {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project_id=%q", req.ProjectID))
		}
		if err := refuseRemotePty(proj); err != nil {
			return bridge.PtyEnvResponse{}, err
		}
		if proj.IsHosted() {
			return resolveHostedPtyEnv(s, proj, req.Directory)
		}
		if !project.DirWithin(req.Directory, proj.Path) {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("directory %q is not within project %q", req.Directory, proj.ID))
		}
	} else {
		proj = findProjectForPty(s, req.Project, req.Directory)
		if proj == nil {
			return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project=%q directory=%q", req.Project, req.Directory))
		}
		if err := refuseRemotePty(proj); err != nil {
			return bridge.PtyEnvResponse{}, err
		}
		if proj.IsHosted() {
			return resolveHostedPtyEnv(s, proj, req.Directory)
		}
	}

	relayToken, ok := proj.Token.Reveal()
	if !ok {
		return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInternalError,
			fmt.Errorf("project %q has no token: the sealed store may be unavailable", proj.ID))
	}

	return bridge.PtyEnvResponse{
		RelayToken: relayToken,
		WorkingDir: proj.Path,
	}, nil
}

// ResolveProjectTemplate returns ONLY the template definition fields
// (command/args/env/...), never the project token: ResolvePtyEnv is the
// sole plaintext-token egress over the bridge and this call must not widen
// that surface. Do not be tempted to reuse GetProject here -- that marshals
// the raw Project including its plaintext token.
func (r *appRouter) ResolveProjectTemplate(ctx context.Context, req bridge.ShellTemplateRequest, token string) (bridge.ShellTemplateResponse, error) {
	if _, err := r.requireServiceIdentity(ctx, token, service.OpResolveProjectTemplate); err != nil {
		return bridge.ShellTemplateResponse{}, err
	}

	proj, _ := config.FindProjectByID(r.store.Get(), req.ProjectID)
	if proj == nil {
		return bridge.ShellTemplateResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("project not found: project_id=%q", req.ProjectID))
	}
	// Refused explicitly rather than relying on ShellTemplates being empty on
	// a remote project: a Project constructed directly (a migration, a
	// hand-edited settings.json) could carry templates from a former life as
	// a local project, and resolving one would hand a host launch command to
	// a caller acting for another machine.
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

// resolveHostedPtyEnv is ResolvePtyEnv's host-project branch (docs/ssh-hosts.md):
// no project token at all (decision 6 — a host session gets no relay-brokered
// tools to hold one for), the project's path as WorkingDir (it is real, it
// just isn't on this machine), and a HostSpec carrying the ssh argv prefix
// and the absolute tool paths the last probe discovered.
func resolveHostedPtyEnv(s *config.Settings, proj *config.Project, directory string) (bridge.PtyEnvResponse, error) {
	if !hostDirWithin(directory, proj.Path) {
		return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("directory %q is not within project %q", directory, proj.ID))
	}
	host, _ := config.FindHostByID(s, proj.HostID)
	if host == nil {
		return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInternalError, fmt.Errorf("project %q names host %q, which no longer exists", proj.ID, proj.HostID))
	}
	if host.Probe == nil || host.Probe.ClaudePath == "" {
		return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, fmt.Errorf("host %q has no claude: run a probe", host.Name))
	}
	controlDir, err := sshhost.ControlDir()
	if err != nil {
		return bridge.PtyEnvResponse{}, jsonrpc.NewCodedError(jsonrpc.CodeInternalError, fmt.Errorf("ssh control dir: %w", err))
	}
	return bridge.PtyEnvResponse{
		RelayToken: "",
		WorkingDir: proj.Path,
		Host: &bridge.HostSpec{
			ID:         host.ID,
			Name:       host.Name,
			SSHArgv:    sshhost.SSHArgv(*host, controlDir),
			NodePath:   host.Probe.NodePath,
			ClaudePath: host.Probe.ClaudePath,
			Shell:      host.Probe.Shell,
			OS:         host.Probe.OS,
		},
	}, nil
}

// hostDirWithin is project.DirWithin's lexical-only cousin for a host
// project (docs/ssh-hosts.md): the directory names a path on the HOST, so
// relay cannot os.Stat, EvalSymlinks or os.SameFile it the way DirWithin
// does for a console path — path.Clean and a prefix compare is the only
// check that makes sense for a filesystem this process cannot see. An empty
// dir means "no directory to validate", matching DirWithin.
func hostDirWithin(dir, projectPath string) bool {
	if dir == "" {
		return true
	}
	if projectPath == "" {
		return false
	}
	clean := path.Clean(dir)
	cleanProj := path.Clean(projectPath)
	return clean == cleanProj || strings.HasPrefix(clean, cleanProj+"/")
}

// refuseRemotePty rejects a PTY launch bound to a remote project. Without
// it the request would succeed: project.DirWithin("", "") returns true (the
// empty-dir branch short-circuits before the empty-project-path branch), so
// the caller would receive the project's plaintext token with
// WorkingDir: "", and Go's exec.Cmd treats an empty Dir as the PARENT
// process's working directory -- a host shell coming up holding a remote
// project's credential. Refusing here rather than teaching project.DirWithin
// about kinds keeps that helper a pure containment predicate.
func refuseRemotePty(proj *config.Project) error {
	if !proj.IsRemote() {
		return nil
	}
	return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams,
		fmt.Errorf("project %q is a remote project: it has no host directory to launch a terminal in", proj.ID))
}

// findProjectForPty accepts either an explicit project identifier (ID or
// name) or a directory match against Project.Path, since a terminal_create
// request may carry only the working directory.
func findProjectForPty(s *config.Settings, projectRef, directory string) *config.Project {
	for i := range s.Projects {
		p := &s.Projects[i]
		if projectRef != "" && (p.ID == projectRef || p.Name == projectRef) {
			return p
		}
		if projectRef == "" && directory != "" && p.Path == directory {
			return p
		}
	}
	return nil
}

func (r *appRouter) ReloadExternalMcp(ctx context.Context, id string) error {
	settings := r.store.Reload()
	mcpCfg, _ := config.FindExternalMcpByID(settings, id)
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

// RegisterManifest authenticates the caller's launch identity, which must
// hold the manifest capability, then forwards the full record to the
// enhanced-services registry. The registry
// handles conflict detection and triggers an onChange notification so the
// front-door dispatcher rebuilds its routing table.
//
// A service registers only under its own launch name. The registry forgets a
// manifest by the id of the launch that exited, so a manifest under any other
// id would outlive the process that serves it.
func (r *appRouter) RegisterManifest(ctx context.Context, req bridge.RegisterManifestRequest, token string) error {
	id, err := r.requireServiceIdentity(ctx, token, service.OpRegisterManifest)
	if err != nil {
		return err
	}
	if req.ServiceID != id.Name {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("%s: service %q may not register a manifest for %q", bridge.ReqRegisterManifest, id.Name, req.ServiceID))
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

// AdminOp resolves name against adminOps and runs it. An op absent from the
// table is refused the same way an unknown bridge request type is — there is
// no default handler to fall back to, by construction, since the table's
// entire point is to name only what a later step has deliberately wired in.
func (r *appRouter) AdminOp(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	op, ok := adminOps[name]
	if !ok {
		return nil, jsonrpc.NewCodedError(jsonrpc.CodeMethodNotFound, fmt.Errorf("unknown admin operation: %q", name))
	}
	return op(ctx, r, args)
}
