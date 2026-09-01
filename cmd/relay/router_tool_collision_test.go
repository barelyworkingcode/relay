package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

type dispatchedCall struct {
	mcpID string
	tool  string
	meta  string
}

// collidingProvider is a ToolManager backed by an ORDERED slice rather than a
// map, so a test can state the connection order explicitly instead of hoping a
// map reproduces one.
type collidingProvider struct {
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
// v1 MCP, which leaves the per-MCP context blob passed through to _meta
// verbatim — so what CallTool records as `meta` is exactly the resource scope
// that was in force.
func (p *collidingProvider) McpSurfaceFor(string) McpSurface { return McpSurface{} }

func (p *collidingProvider) Reconcile(context.Context, []config.ExternalMcp)           {}
func (p *collidingProvider) Reload(context.Context, string, *config.ExternalMcp) error { return nil }

func (p *collidingProvider) dispatches() []dispatchedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *collidingProvider) dispatchedIDs() []string {
	var out []string
	for _, c := range p.dispatches() {
		out = append(out, c.mcpID)
	}
	return out
}

const (
	collisionMcpA = "fs-a"
	collisionMcpB = "fs-b"
	// collisionMcpC exposes nothing that collides. It exists so a grant can
	// name a real, connected, permitted MCP while admitting NEITHER owner of
	// the colliding tool.
	collisionMcpC = "notes"

	collidingTool = "fs_read"
)

func collisionTools() map[string][]mcp.Tool {
	return map[string][]mcp.Tool{
		collisionMcpA: simpleTools(collidingTool, "fs_write", "a_only"),
		collisionMcpB: simpleTools(collidingTool, "fs_write", "b_only"),
		collisionMcpC: simpleTools("note_read"),
	}
}

// collisionScopes is the per-MCP _meta context. The two values are different
// on purpose: which one goes on the wire is the resource-scope layer.
func collisionScopes() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		collisionMcpA: json.RawMessage(`{"allowed_dirs":["/scope/only-a"]}`),
		collisionMcpB: json.RawMessage(`{"allowed_dirs":["/scope/only-b"]}`),
		collisionMcpC: json.RawMessage(`{"allowed_dirs":["/scope/only-c"]}`),
	}
}

// rec may be shared across many routers so a determinism loop writes one
// audit log; pass nil for no auditing.
//
// It does not go through newTestRouter because that helper takes a concrete
// *ExternalMcpManager and this needs an injected ToolManager.
func newCollisionRouter(t *testing.T, rec *AuditRecorder, dir string, order, allowedMcpIDs []string, disabled map[string][]string) (*appRouter, *collidingProvider) {
	t.Helper()
	tp := newCollidingProvider(order, collisionTools())

	mcps := make([]config.ExternalMcp, 0, 3)
	for _, id := range []string{collisionMcpA, collisionMcpB, collisionMcpC} {
		mcps = append(mcps, config.ExternalMcp{ID: id, DisplayName: id})
	}
	s := &config.Settings{
		Version:      1,
		ExternalMcps: mcps,
		Projects: []config.Project{{
			ID:            "collision-project",
			Name:          "collision",
			Path:          "/tmp/collision",
			AllowedMcpIDs: slices.Clone(allowedMcpIDs),
			Token:         config.NewSecret(testToken),
			TokenHash:     config.HashToken(testToken),
			DisabledTools: disabled,
			Context:       collisionScopes(),
		}},
		AdminSecret: config.NewSecret("supersecretadmin"),
	}
	store := config.NewSettingsStoreWithCache(dir, testSealer(), s)
	return &appRouter{
		store:    store,
		tools:    tp,
		services: NewServiceRegistry(),
		onChange: func() {},
		audit:    rec,
	}, tp
}

func bothOrders() map[string][]string {
	return map[string][]string{
		"a-then-b": {collisionMcpA, collisionMcpB, collisionMcpC},
		"b-then-a": {collisionMcpB, collisionMcpA, collisionMcpC},
	}
}

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

const collisionDeterminismRuns = 50

// Both orders belong in the SAME identity set. "Deterministic" here does not
// mean "stable given a fixed enumeration order"; it means the answer does not
// depend on enumeration order at all.
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

			// Tallied rather than fatal so the determinism comparison below
			// still runs and the failure shows BOTH halves of the finding.
			for _, id := range tp.dispatchedIDs() {
				reached[id]++
			}
		}
	}

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

	if lastErr == nil {
		t.Fatal("a grant admitting both fs-a and fs-b served a colliding tool instead of refusing the ambiguity")
	}
	if code := codeOf(lastErr); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
	}
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

func fingerprintBody(labelled string) string {
	if _, body, ok := strings.Cut(labelled, " -> "); ok {
		return body
	}
	return labelled
}

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
	// If the fix instead decides that a tool no GRANTED MCP owns is simply not
	// in this caller's surface, this is the one line to revisit. The
	// determinism assertion above is not negotiable either way.
	if code := codeOf(lastErr); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d); the refusal was %v",
			code, jsonrpc.CodeUnauthorized, lastErr)
	}
}

func TestCallTool_GrantAdmittingNoOwnerNamesNoMcpOutsideTheGrant(t *testing.T) {
	for orderName, order := range bothOrders() {
		r, tp := newCollisionRouter(t, nil, t.TempDir(), order, []string{collisionMcpC}, nil)

		_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatalf("%s: a grant naming only %s reached %q, which it does not own", orderName, collisionMcpC, collidingTool)
		}
		if got := tp.dispatchedIDs(); len(got) != 0 {
			t.Fatalf("%s: a refused call still reached %v", orderName, got)
		}
		msg := err.Error()
		for _, leaked := range []string{collisionMcpA, collisionMcpB} {
			if strings.Contains(msg, leaked) {
				t.Errorf("%s: refusal named %q, an MCP outside this grant: %q", orderName, leaked, msg)
			}
		}
		if !strings.Contains(msg, collisionMcpC) {
			t.Errorf("%s: refusal did not name the grant's own MCP %q: %q", orderName, collisionMcpC, msg)
		}
		if !strings.Contains(msg, collidingTool) {
			t.Errorf("%s: refusal did not name the tool %q: %q", orderName, collidingTool, msg)
		}
	}
}

func TestCallTool_GrantedMcpOwnerStillResolvedWhenItRefusesItself(t *testing.T) {
	rec := newTestAudit(t, nil)
	r, tp := newCollisionRouter(t, rec, t.TempDir(),
		[]string{collisionMcpA, collisionMcpB, collisionMcpC}, []string{collisionMcpA},
		map[string][]string{collisionMcpA: {collidingTool}})

	_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
	if err == nil {
		t.Fatal("a hand-disabled tool was not refused")
	}
	if got := tp.dispatchedIDs(); len(got) != 0 {
		t.Fatalf("a refused call still reached %v", got)
	}
	if strings.Contains(err.Error(), "is available to this grant") {
		t.Errorf("a tool the grant's own MCP disabled must get THAT layer's denial, not R6's generic one: %q", err.Error())
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("expected 1 audit record, got %d", len(events))
	}
	if events[0].McpID != collisionMcpA {
		t.Errorf("audit names mcp_id=%q, want the grant's own %q — resolution must still land on it, not go unresolved",
			events[0].McpID, collisionMcpA)
	}
}

func TestCallTool_SingleMcpBehaviourIsUnchanged(t *testing.T) {
	soloTools := map[string][]mcp.Tool{collisionMcpA: simpleTools(collidingTool, "fs_write")}
	soloOrder := []string{collisionMcpA}

	newSolo := func(t *testing.T, allowed []string, disabled map[string][]string) (*appRouter, *collidingProvider, *AuditRecorder) {
		t.Helper()
		rec := newTestAudit(t, nil)
		tp := newCollidingProvider(soloOrder, soloTools)
		s := &config.Settings{
			Version:      1,
			ExternalMcps: []config.ExternalMcp{{ID: collisionMcpA, DisplayName: collisionMcpA}, {ID: collisionMcpC, DisplayName: collisionMcpC}},
			Projects: []config.Project{{
				ID:            "solo-project",
				Name:          "solo",
				Path:          "/tmp/solo",
				AllowedMcpIDs: allowed,
				Token:         config.NewSecret(testToken),
				TokenHash:     config.HashToken(testToken),
				DisabledTools: disabled,
				Context:       collisionScopes(),
			}},
			AdminSecret: config.NewSecret("supersecretadmin"),
		}
		store := config.NewSettingsStoreWithCache(t.TempDir(), testSealer(), s)
		r := &appRouter{
			store:    store,
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

func newRealManagerCollisionRouter(t *testing.T, allowedMcpIDs []string) (*appRouter, *collisionCounter, *collisionCounter) {
	t.Helper()
	a, b := &collisionCounter{}, &collisionCounter{}
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, collisionMcpA, newMockConn(collisionMcpA, simpleTools(collidingTool), a.handler(collisionMcpA)))
	addMockConn(mgr, collisionMcpB, newMockConn(collisionMcpB, simpleTools(collidingTool), b.handler(collisionMcpB)))

	s := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{
			{ID: collisionMcpA, DisplayName: collisionMcpA},
			{ID: collisionMcpB, DisplayName: collisionMcpB},
		},
		Projects: []config.Project{{
			ID:            "collision-project",
			Name:          "collision",
			Path:          "/tmp/collision",
			AllowedMcpIDs: slices.Clone(allowedMcpIDs),
			Token:         config.NewSecret(testToken),
			TokenHash:     config.HashToken(testToken),
			Context:       collisionScopes(),
		}},
		AdminSecret: config.NewSecret("supersecretadmin"),
	}
	return newTestRouter(t, s, mgr), a, b
}

// Go randomises map iteration, so over many identical calls a router that
// resolves globally will resolve to fs-a about half the time and refuse —
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
	if a.count() != 0 && b.count() != 0 {
		t.Errorf("the same call was served by %s %d times and by %s %d times — which allowed_dirs applied was chosen by the map seed",
			collisionMcpA, a.count(), collisionMcpB, b.count())
	}
	if a.count()+b.count() != 0 {
		t.Errorf("an ambiguous grant reached an MCP %d times — a collision the caller cannot disambiguate must be refused",
			a.count()+b.count())
	}
}
