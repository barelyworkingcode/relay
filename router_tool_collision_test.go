package main

// Issue #35: CallTool resolved a tool name to an MCP GLOBALLY, before the
// caller's grant was consulted, by iterating a Go map. Two consequences, and
// the second is the one that matters:
//
//   - a token granted only B, calling a tool A and B both expose, was refused
//     with "MCP 'A' is disabled for this token";
//   - a token granted BOTH dispatched to a different MCP call to call, so
//     stored.Context[extID] (the _meta resource scope), DisabledTools[extID]
//     and the audit's mcp_id were all chosen by Go's map seed.
//
// The behaviour pinned here is resolution WITHIN the calling grant's MCP-level
// allowance:
//
//  1. grant admits exactly one owner  -> that owner serves, in BOTH orders;
//  2. grant admits several owners     -> a DETERMINISTIC refusal naming the
//     tool and every colliding id, never an arbitrary pick;
//  3. the audit names the MCP that ACTUALLY served;
//  4. grant admits no owner           -> a denial, and the same one every time;
//  5. single-MCP behaviour unchanged.
//
// These tests are written to compile against BOTH the pre-fix interface
// (ToolProvider.FindToolOwner) and the post-fix one (ToolProvider.ToolOwners).
// collidingProvider below implements both methods; a Go interface only
// requires the methods it names, so the same file builds either way and the
// only thing that changes is whether the assertions hold.
//
// The assertions are on OUTCOMES, never on error prose: that it is an error,
// that it carries CodeUnauthorized, that the audit outcome is `denied`, and
// that the message CONTAINS the colliding ids. The sentence is the
// implementer's to write.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"relaygo/jsonrpc"
	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// A tool provider whose connection order is DECLARED, not map-derived
// ---------------------------------------------------------------------------

// dispatchedCall is one invocation that actually reached an MCP. Recording the
// id is the whole point: "refused" and "silently did nothing" are
// indistinguishable from the caller's return value and mean opposite things,
// so every test here asks which MCP was reached rather than only whether an
// error came back. The meta is recorded too, because the resource scope in
// force (allowed_dirs) is keyed off the same id and is the layer issue #35
// says goes non-deterministic.
type dispatchedCall struct {
	mcpID string
	tool  string
	meta  string
}

// collidingProvider is a ToolManager backed by an ORDERED slice rather than a
// map, so a test can state the connection order explicitly instead of hoping a
// map reproduces one. It deliberately implements BOTH resolution methods.
type collidingProvider struct {
	// order is the connection order this provider reports. Pre-fix,
	// FindToolOwner returns order[0] among the owners, which is exactly the
	// arbitrary global pick the map made — made reproducible.
	order []string
	tools map[string][]mcp.Tool

	mu    sync.Mutex
	calls []dispatchedCall
}

func newCollidingProvider(order []string, tools map[string][]mcp.Tool) *collidingProvider {
	return &collidingProvider{order: slices.Clone(order), tools: tools}
}

func (p *collidingProvider) exposes(id, toolName string) bool {
	for _, t := range p.tools[id] {
		if t.Name == toolName {
			return true
		}
	}
	return false
}

func (p *collidingProvider) Tools(id string) []mcp.Tool { return p.tools[id] }

// FindToolOwner is the PRE-FIX ToolProvider method. It answers with the first
// owner in the declared order — a deterministic stand-in for "whichever one
// the map enumerated first" — plus that MCP's config, matching the real
// signature exactly.
//
// Post-fix this method is simply unused by the router; it stays so this file
// compiles against the interface as it is TODAY as well as as it will be.
func (p *collidingProvider) FindToolOwner(name string) (string, *ExternalMcp) {
	for _, id := range p.order {
		if p.exposes(id, name) {
			cfg := ExternalMcp{ID: id, DisplayName: id}
			return id, &cfg
		}
	}
	return "", nil
}

// ToolOwners is the POST-FIX ToolProvider method: every CONNECTED MCP exposing
// the tool, sorted, so the router can intersect that set with the grant.
// Sorted output is returned even though the router is the thing that must be
// deterministic — an unsorted answer here would let a correct router still
// produce an order-dependent refusal.
func (p *collidingProvider) ToolOwners(name string) []string {
	var out []string
	for _, id := range p.order {
		if p.exposes(id, name) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (p *collidingProvider) CallTool(_ context.Context, id, name string, _ json.RawMessage, meta json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.calls = append(p.calls, dispatchedCall{mcpID: id, tool: name, meta: string(meta)})
	p.mu.Unlock()
	return json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, "served by "+id)), nil
}

// McpSurfaceFor answers with the zero surface: no schema, version 0. That is a
// v1 MCP, which exempts these tests from the scope-presence and
// schema-usability layers and leaves the per-MCP context blob passed through
// to _meta verbatim — so what CallTool records as `meta` is exactly the
// resource scope that was in force.
func (p *collidingProvider) McpSurfaceFor(string) McpSurface { return McpSurface{} }

func (p *collidingProvider) Reconcile(context.Context, []ExternalMcp)           {}
func (p *collidingProvider) Reload(context.Context, string, *ExternalMcp) error { return nil }

// dispatches returns a snapshot of everything that reached an MCP.
func (p *collidingProvider) dispatches() []dispatchedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// dispatchedIDs is the dispatch log reduced to the ids, for fingerprinting.
func (p *collidingProvider) dispatchedIDs() []string {
	var out []string
	for _, c := range p.dispatches() {
		out = append(out, c.mcpID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	collisionMcpA = "fs-a"
	collisionMcpB = "fs-b"
	// collisionMcpC exposes nothing that collides. It exists so a grant can
	// name a real, connected, permitted MCP while admitting NEITHER owner of
	// the colliding tool — case 4.
	collisionMcpC = "notes"

	// collidingTool is exposed by BOTH fs-a and fs-b. Two filesystem MCPs
	// exposing one read tool is the ordinary case issue #35 is about.
	collidingTool = "fs_read"
)

// collisionTools is the surface of all three MCPs. Unannotated on purpose: the
// project below is LOCAL, which defaults to write and to allowed-external, so
// an unannotated tool is callable and none of the ADR-011 annotation layers
// can be what refuses. Anything that refuses in these tests refuses because of
// the grant's MCP-level allowance, which is what is under test.
func collisionTools() map[string][]mcp.Tool {
	return map[string][]mcp.Tool{
		collisionMcpA: simpleTools(collidingTool, "fs_write", "a_only"),
		collisionMcpB: simpleTools(collidingTool, "fs_write", "b_only"),
		collisionMcpC: simpleTools("note_read"),
	}
}

// collisionScopes is the per-MCP _meta context. The two values are different
// on purpose: which one goes on the wire is the resource-scope layer, and
// issue #35's real finding is that the map seed was choosing it.
func collisionScopes() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		collisionMcpA: json.RawMessage(`{"allowed_dirs":["/scope/only-a"]}`),
		collisionMcpB: json.RawMessage(`{"allowed_dirs":["/scope/only-b"]}`),
		collisionMcpC: json.RawMessage(`{"allowed_dirs":["/scope/only-c"]}`),
	}
}

// newCollisionRouter builds a router over collidingProvider whose single local
// project grants exactly allowedMcpIDs, with the MCPs connected in the given
// order. rec may be shared across many routers so a determinism loop writes one
// audit log; pass nil for no auditing.
//
// It does not go through newTestRouter because that helper takes a concrete
// *ExternalMcpManager and this needs an injected ToolManager. Nothing existing
// is modified.
func newCollisionRouter(t *testing.T, rec *AuditRecorder, dir string, order, allowedMcpIDs []string, disabled map[string][]string) (*appRouter, *collidingProvider) {
	t.Helper()
	tp := newCollidingProvider(order, collisionTools())

	mcps := make([]ExternalMcp, 0, 3)
	for _, id := range []string{collisionMcpA, collisionMcpB, collisionMcpC} {
		mcps = append(mcps, ExternalMcp{ID: id, DisplayName: id})
	}
	s := &Settings{
		Version:      1,
		ExternalMcps: mcps,
		Projects: []Project{{
			ID:            "collision-project",
			Name:          "collision",
			Path:          "/tmp/collision",
			AllowedMcpIDs: slices.Clone(allowedMcpIDs),
			Token:         testToken,
			TokenHash:     hashToken(testToken),
			DisabledTools: disabled,
			Context:       collisionScopes(),
		}},
		AdminSecret: "supersecretadmin",
	}
	return &appRouter{
		store:    &FileSettingsStore{cache: s, dir: dir},
		tools:    tp,
		services: NewServiceRegistry(),
		onChange: func() {},
		audit:    rec,
	}, tp
}

// bothOrders is the pair of connection orders every resolution assertion has
// to hold under. A map cannot be relied on to reproduce one order, so a
// single-order test proves nothing about a router that resolves by order.
func bothOrders() map[string][]string {
	return map[string][]string{
		"a-then-b": {collisionMcpA, collisionMcpB, collisionMcpC},
		"b-then-a": {collisionMcpB, collisionMcpA, collisionMcpC},
	}
}

// outcomeFingerprint reduces one call to the facts a caller and an auditor can
// observe. Two calls with the same fingerprint are the same outcome; a
// determinism assertion is then string equality, which cannot be satisfied by
// "it refused both times, differently".
//
// The error's TEXT is included deliberately even though no test pins its
// wording: determinism is a property OF the wording as much as of the code, and
// a refusal that names fs-a on Monday and fs-b on Tuesday is precisely the bug.
func outcomeFingerprint(err error, ev *AuditEvent, dispatched []string) string {
	errText := "<nil>"
	if err != nil {
		errText = err.Error()
	}
	audit := "<none>"
	if ev != nil {
		audit = fmt.Sprintf("outcome=%s mcp_id=%s tool=%s err=%s", ev.Outcome, ev.McpID, ev.Tool, ev.Error)
	}
	return fmt.Sprintf("err=%q code=%d dispatched=%v | %s", errText, codeOf(err), dispatched, audit)
}

// ---------------------------------------------------------------------------
// 1. The grant admits exactly one owner — that owner serves, in BOTH orders
// ---------------------------------------------------------------------------

// This is the case observed live in issue #35: a token granted only B, calling
// a tool A and B both expose, refused with "MCP 'A' is disabled for this
// token". B owns the tool name, B is granted, B must serve it.
//
// The assertion is on WHICH MCP was reached, not on the absence of an error.
// A router that returned nil and called nothing would pass an error check and
// is the opposite of correct.
func TestCallTool_GrantAdmittingOneOwnerReachesIt_InBothConnectionOrders(t *testing.T) {
	for _, granted := range []string{collisionMcpA, collisionMcpB} {
		for orderName, order := range bothOrders() {
			t.Run(granted+"/"+orderName, func(t *testing.T) {
				rec := newTestAudit(t, nil)
				r, tp := newCollisionRouter(t, rec, t.TempDir(), order, []string{granted}, nil)

				res, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
				if err != nil {
					t.Fatalf("grant names only %s, which owns %q, but the call was refused: %v",
						granted, collidingTool, err)
				}
				if len(res) == 0 {
					t.Error("call returned no result")
				}

				calls := tp.dispatches()
				if len(calls) != 1 {
					t.Fatalf("expected exactly 1 MCP invocation, got %d (%v) — a call that reached nothing is not a call that was served",
						len(calls), tp.dispatchedIDs())
				}
				if calls[0].mcpID != granted {
					t.Errorf("call was dispatched to %q, want %q — the grant, not the connection order, decides",
						calls[0].mcpID, granted)
				}
				if calls[0].tool != collidingTool {
					t.Errorf("dispatched tool = %q, want %q", calls[0].tool, collidingTool)
				}

				// The resource scope that went on the wire must be the granted
				// MCP's, not the other owner's. This is the layer issue #35
				// says the map seed was choosing.
				wantDir := "/scope/only-" + strings.TrimPrefix(granted, "fs-")
				if !strings.Contains(calls[0].meta, wantDir) {
					t.Errorf("_meta = %s, want it to carry %s — the scope in force must be the served MCP's",
						calls[0].meta, wantDir)
				}
				if otherDir := "/scope/only-" + map[string]string{collisionMcpA: "b", collisionMcpB: "a"}[granted]; strings.Contains(calls[0].meta, otherDir) {
					t.Errorf("_meta = %s, but it carries the OTHER MCP's scope %s", calls[0].meta, otherDir)
				}

				events := readLoggedEvents(t, rec)
				if len(events) != 1 {
					t.Fatalf("expected 1 audit record, got %d", len(events))
				}
				if events[0].Outcome != AuditOutcomeOK {
					t.Errorf("audit outcome = %q, want %q", events[0].Outcome, AuditOutcomeOK)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 2. The grant admits several owners — a DETERMINISTIC refusal naming both
// ---------------------------------------------------------------------------

const collisionDeterminismRuns = 50

// A single call cannot distinguish "refuses" from "happened to refuse this
// time", so the identical call is made many times, across both connection
// orders, and every outcome must be byte-identical — the returned error, its
// code, the audit record, and the set of MCPs reached.
//
// Both orders belong in the SAME identity set on purpose. "Deterministic" here
// does not mean "stable given a fixed enumeration order"; it means the answer
// does not depend on enumeration order at all, which is the only reading under
// which the resource-scope layer stops being a function of the map seed.
func TestCallTool_GrantAdmittingSeveralOwnersRefusesDeterministically_NamingBoth(t *testing.T) {
	rec := newTestAudit(t, nil)
	dir := t.TempDir()

	var fingerprints []string
	var lastErr error
	reached := map[string]int{}

	for orderName, order := range bothOrders() {
		for i := 0; i < collisionDeterminismRuns; i++ {
			r, tp := newCollisionRouter(t, rec, dir, order, []string{collisionMcpA, collisionMcpB}, nil)
			_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
			lastErr = err

			events := readLoggedEvents(t, rec)
			ev := &events[len(events)-1]
			fingerprints = append(fingerprints,
				orderName+" #"+fmt.Sprint(i)+" -> "+outcomeFingerprint(err, ev, tp.dispatchedIDs()))

			// Nothing may be reached. An ambiguity resolved by calling one of
			// the candidates is the bug, whatever it returns afterwards.
			// Tallied rather than fatal so the determinism comparison below
			// still runs and the failure shows BOTH halves of the finding.
			for _, id := range tp.dispatchedIDs() {
				reached[id]++
			}
		}
	}

	// Determinism first: it is the property, and it is what a single call
	// cannot show.
	first := fingerprintBody(fingerprints[0])
	for _, fp := range fingerprints[1:] {
		if body := fingerprintBody(fp); body != first {
			t.Errorf("the same call produced two different outcomes.\n  %s\n  %s", fingerprints[0], fp)
			break
		}
	}
	if len(reached) != 0 {
		t.Fatalf("an ambiguous call reached %v — a collision the caller cannot disambiguate must be refused, not picked", reached)
	}

	// Then the shape of the one outcome.
	if lastErr == nil {
		t.Fatal("a grant admitting both fs-a and fs-b served a colliding tool instead of refusing the ambiguity")
	}
	if code := codeOf(lastErr); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
	}
	// The message must let an operator learn WHICH grant is ambiguous, which
	// means the tool and every colliding id. The sentence is not pinned.
	for _, want := range []string{collidingTool, collisionMcpA, collisionMcpB} {
		if !strings.Contains(lastErr.Error(), want) {
			t.Errorf("refusal %q does not mention %q — an operator cannot learn which MCPs collided", lastErr.Error(), want)
		}
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2*collisionDeterminismRuns {
		t.Fatalf("expected %d audit records, got %d", 2*collisionDeterminismRuns, len(events))
	}
	for i, ev := range events {
		if ev.Outcome != AuditOutcomeDenied {
			t.Fatalf("audit record %d: outcome = %q, want %q — relay made this decision, no MCP was reached",
				i, ev.Outcome, AuditOutcomeDenied)
		}
		if ev.Tool != collidingTool {
			t.Errorf("audit record %d: tool = %q, want %q", i, ev.Tool, collidingTool)
		}
	}
}

// fingerprintBody strips the run label a fingerprint is prefixed with, so the
// label makes a failure readable without entering the comparison itself.
func fingerprintBody(labelled string) string {
	if _, body, ok := strings.Cut(labelled, " -> "); ok {
		return body
	}
	return labelled
}

// ---------------------------------------------------------------------------
// 3. The audit names the MCP that ACTUALLY served
// ---------------------------------------------------------------------------

// The audit's mcp_id came from the same global lookup as the dispatch, so it
// inherited the same nondeterminism: a record could name an MCP that did not
// serve the call. Stated as an invariant over every grant and both orders —
// whenever a call was served, exactly one MCP was reached and the record names
// that one.
func TestCallTool_AuditRecordNamesTheMcpThatServedTheCall(t *testing.T) {
	for _, granted := range []string{collisionMcpA, collisionMcpB} {
		for orderName, order := range bothOrders() {
			t.Run(granted+"/"+orderName, func(t *testing.T) {
				rec := newTestAudit(t, nil)
				r, tp := newCollisionRouter(t, rec, t.TempDir(), order, []string{granted}, nil)

				if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
					t.Fatalf("call refused: %v", err)
				}

				calls := tp.dispatches()
				if len(calls) != 1 {
					t.Fatalf("expected 1 MCP invocation, got %d (%v)", len(calls), tp.dispatchedIDs())
				}
				events := readLoggedEvents(t, rec)
				if len(events) != 1 {
					t.Fatalf("expected 1 audit record, got %d", len(events))
				}
				if events[0].McpID != calls[0].mcpID {
					t.Errorf("audit names mcp_id=%q but the call was served by %q — a record that names the wrong MCP is worse than no record",
						events[0].McpID, calls[0].mcpID)
				}
				if events[0].McpID != granted {
					t.Errorf("audit names mcp_id=%q, want the granted MCP %q", events[0].McpID, granted)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 4. The grant admits NO owner — still a denial, and the same one every time
// ---------------------------------------------------------------------------

// The grant names a real, connected, permitted MCP that does not own the tool.
// Both owners are outside the grant, so the call must be refused — and refused
// identically every time, naming the same MCP whichever order the owners were
// connected in. Pre-fix the named MCP was whichever one the map reached first,
// so an operator diagnosing a denial was shown a different culprit run to run.
func TestCallTool_GrantAdmittingNoOwnerDeniesDeterministically(t *testing.T) {
	rec := newTestAudit(t, nil)
	dir := t.TempDir()

	var fingerprints []string
	var lastErr error

	for orderName, order := range bothOrders() {
		for i := 0; i < collisionDeterminismRuns; i++ {
			r, tp := newCollisionRouter(t, rec, dir, order, []string{collisionMcpC}, nil)
			_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
			lastErr = err

			if err == nil {
				t.Fatalf("%s run %d: a grant naming only %s reached %q, which it does not own",
					orderName, i, collisionMcpC, collidingTool)
			}
			if got := tp.dispatchedIDs(); len(got) != 0 {
				t.Fatalf("%s run %d: a refused call still reached %v", orderName, i, got)
			}

			events := readLoggedEvents(t, rec)
			ev := &events[len(events)-1]
			fingerprints = append(fingerprints,
				orderName+" #"+fmt.Sprint(i)+" -> "+outcomeFingerprint(err, ev, tp.dispatchedIDs()))
		}
	}

	first := fingerprintBody(fingerprints[0])
	for _, fp := range fingerprints[1:] {
		if body := fingerprintBody(fp); body != first {
			t.Fatalf("the same denial was reported two different ways.\n  %s\n  %s", fingerprints[0], fp)
		}
	}
	// The spec for this case reads "still a denial ... the same MCP named every
	// time", which is the access-denied shape rather than the unknown-tool one.
	// If the fix instead decides that a tool no GRANTED MCP owns is simply not
	// in this caller's surface — a defensible and arguably tighter reading,
	// since it names no MCP the grant does not hold — this is the one line to
	// revisit. The determinism assertion above is not negotiable either way.
	if code := codeOf(lastErr); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d); the refusal was %v",
			code, jsonrpc.CodeUnauthorized, lastErr)
	}
}

// ---------------------------------------------------------------------------
// 5. Single-MCP behaviour is unchanged
// ---------------------------------------------------------------------------

// The regression guard. With one connected MCP there is no collision to
// resolve, so every existing answer must survive verbatim: a granted call is
// served and audited ok, an ungranted MCP is refused with CodeUnauthorized and
// audited denied, a tool nobody exposes is an error, and a hand-disabled tool
// is still refused.
func TestCallTool_SingleMcpBehaviourIsUnchanged(t *testing.T) {
	// Only one MCP is connected, so nothing collides.
	soloTools := map[string][]mcp.Tool{collisionMcpA: simpleTools(collidingTool, "fs_write")}
	soloOrder := []string{collisionMcpA}

	newSolo := func(t *testing.T, allowed []string, disabled map[string][]string) (*appRouter, *collidingProvider, *AuditRecorder) {
		t.Helper()
		rec := newTestAudit(t, nil)
		tp := newCollidingProvider(soloOrder, soloTools)
		s := &Settings{
			Version:      1,
			ExternalMcps: []ExternalMcp{{ID: collisionMcpA, DisplayName: collisionMcpA}, {ID: collisionMcpC, DisplayName: collisionMcpC}},
			Projects: []Project{{
				ID:            "solo-project",
				Name:          "solo",
				Path:          "/tmp/solo",
				AllowedMcpIDs: allowed,
				Token:         testToken,
				TokenHash:     hashToken(testToken),
				DisabledTools: disabled,
				Context:       collisionScopes(),
			}},
			AdminSecret: "supersecretadmin",
		}
		r := &appRouter{
			store:    &FileSettingsStore{cache: s, dir: t.TempDir()},
			tools:    tp,
			services: NewServiceRegistry(),
			onChange: func() {},
			audit:    rec,
		}
		return r, tp, rec
	}

	t.Run("granted call is served and audited ok", func(t *testing.T) {
		r, tp, rec := newSolo(t, []string{collisionMcpA}, nil)
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
			t.Fatalf("granted single-MCP call refused: %v", err)
		}
		if got := tp.dispatchedIDs(); len(got) != 1 || got[0] != collisionMcpA {
			t.Fatalf("dispatched to %v, want [%s]", got, collisionMcpA)
		}
		events := readLoggedEvents(t, rec)
		if len(events) != 1 || events[0].Outcome != AuditOutcomeOK || events[0].McpID != collisionMcpA {
			t.Errorf("audit = %+v, want one ok record naming %s", events, collisionMcpA)
		}
	})

	t.Run("ungranted MCP is still refused", func(t *testing.T) {
		r, tp, rec := newSolo(t, []string{collisionMcpC}, nil)
		_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatal("a project not granted fs-a called its tool")
		}
		if code := codeOf(err); code != jsonrpc.CodeUnauthorized {
			t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
		}
		if got := tp.dispatchedIDs(); len(got) != 0 {
			t.Errorf("a refused call still reached %v", got)
		}
		events := readLoggedEvents(t, rec)
		if len(events) != 1 || events[0].Outcome != AuditOutcomeDenied {
			t.Errorf("audit = %+v, want one denied record", events)
		}
	})

	t.Run("a tool nobody exposes is an error", func(t *testing.T) {
		r, tp, _ := newSolo(t, []string{collisionMcpA}, nil)
		if _, err := r.CallTool(context.Background(), "no_such_tool", json.RawMessage(`{}`), testToken); err == nil {
			t.Fatal("an unknown tool was accepted")
		}
		if got := tp.dispatchedIDs(); len(got) != 0 {
			t.Errorf("an unknown tool still reached %v", got)
		}
	})

	t.Run("a hand-disabled tool is still refused", func(t *testing.T) {
		r, tp, rec := newSolo(t, []string{collisionMcpA}, map[string][]string{collisionMcpA: {collidingTool}})
		_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatal("a disabled tool was called")
		}
		if code := codeOf(err); code != jsonrpc.CodeUnauthorized {
			t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
		}
		if got := tp.dispatchedIDs(); len(got) != 0 {
			t.Errorf("a disabled tool still reached %v", got)
		}
		events := readLoggedEvents(t, rec)
		if len(events) != 1 || events[0].Outcome != AuditOutcomeDenied {
			t.Errorf("audit = %+v, want one denied record", events)
		}
	})
}

// ---------------------------------------------------------------------------
// The same properties through the REAL manager, whose conns IS a Go map
// ---------------------------------------------------------------------------

// collisionCounter is a call counter that can be installed as a mockMcpConn's
// sendRequestFunc, so a test can tell which of two live connections a call
// actually landed on.
type collisionCounter struct {
	mu sync.Mutex
	n  int
}

func (c *collisionCounter) handler(id string) func(context.Context, string, interface{}) (json.RawMessage, error) {
	return func(context.Context, string, interface{}) (json.RawMessage, error) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
		return json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, "served by "+id)), nil
	}
}

func (c *collisionCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// newRealManagerCollisionRouter registers fs-a and fs-b as live connections in
// a real ExternalMcpManager — whose conns field is the map issue #35 is about
// — and grants the project exactly allowedMcpIDs.
func newRealManagerCollisionRouter(t *testing.T, allowedMcpIDs []string) (*appRouter, *collisionCounter, *collisionCounter) {
	t.Helper()
	a, b := &collisionCounter{}, &collisionCounter{}
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, collisionMcpA, newMockConn(collisionMcpA, simpleTools(collidingTool), a.handler(collisionMcpA)))
	addMockConn(mgr, collisionMcpB, newMockConn(collisionMcpB, simpleTools(collidingTool), b.handler(collisionMcpB)))

	s := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{
			{ID: collisionMcpA, DisplayName: collisionMcpA},
			{ID: collisionMcpB, DisplayName: collisionMcpB},
		},
		Projects: []Project{{
			ID:            "collision-project",
			Name:          "collision",
			Path:          "/tmp/collision",
			AllowedMcpIDs: slices.Clone(allowedMcpIDs),
			Token:         testToken,
			TokenHash:     hashToken(testToken),
			Context:       collisionScopes(),
		}},
		AdminSecret: "supersecretadmin",
	}
	return newTestRouter(t, s, mgr), a, b
}

// The live reproduction of the reported symptom, with no mock standing in for
// the map. Go randomises map iteration, so over many identical calls a router
// that resolves globally will resolve to fs-a about half the time and refuse —
// while fs-b, which is granted and owns the tool, sits there able to serve.
func TestCallTool_RealManagerMapOrderDoesNotDecideWhetherAGrantedCallSucceeds(t *testing.T) {
	const runs = 200
	r, a, b := newRealManagerCollisionRouter(t, []string{collisionMcpB})

	var refusals int
	var firstErr error
	for i := 0; i < runs; i++ {
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
			refusals++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if refusals != 0 {
		t.Errorf("%d of %d identical calls were refused although %s is granted and owns %q; first refusal: %v",
			refusals, runs, collisionMcpB, collidingTool, firstErr)
	}
	if a.count() != 0 {
		t.Errorf("%d calls were served by %s, which this grant does not name", a.count(), collisionMcpA)
	}
	if b.count() != runs {
		t.Errorf("%s served %d of %d calls, want all of them", collisionMcpB, b.count(), runs)
	}
}

// The worse half of the finding: with BOTH granted, the map seed was choosing
// which resource scope applied. Whatever the fix decides the answer is, the
// answer must be the SAME one every time — so either every call is refused, or
// every call lands on one and the same MCP. A split is the bug.
func TestCallTool_RealManagerBothGrantedProducesOneAnswerNotTwo(t *testing.T) {
	const runs = 200
	r, a, b := newRealManagerCollisionRouter(t, []string{collisionMcpA, collisionMcpB})

	seen := map[string]int{}
	for i := 0; i < runs; i++ {
		_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
		text := "ok"
		if err != nil {
			text = "err:" + err.Error()
		}
		seen[text]++
	}
	if len(seen) != 1 {
		t.Errorf("%d identical calls produced %d distinct outcomes: %v", runs, len(seen), seen)
	}
	// And whichever it is, it must not have been split across the two MCPs:
	// that is the resource-scope layer being decided by the map seed.
	if a.count() != 0 && b.count() != 0 {
		t.Errorf("the same call was served by %s %d times and by %s %d times — which allowed_dirs applied was chosen by the map seed",
			collisionMcpA, a.count(), collisionMcpB, b.count())
	}
	// The settled spec: an ambiguous grant is refused, not resolved by picking.
	if a.count()+b.count() != 0 {
		t.Errorf("an ambiguous grant reached an MCP %d times — a collision the caller cannot disambiguate must be refused",
			a.count()+b.count())
	}
}
