package main

// Deliberate: annotation-derived layers (readOnlyHint) must not narrow the
// route — an MCP could otherwise capture a colliding name by editing its own
// hints.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"relaygo/mcp"
)

func narrowingRouter(t *testing.T, order []string, proj Project, tools map[string][]mcp.Tool) (*appRouter, *collidingProvider) {
	t.Helper()
	if tools == nil {
		tools = collisionTools()
	}
	tp := newCollidingProvider(order, tools)
	proj.Token = NewSecret(testToken)
	proj.TokenHash = hashToken(testToken)
	if proj.Context == nil {
		proj.Context = collisionScopes()
	}
	s := &Settings{
		Version: 1,
		ExternalMcps: []ExternalMcp{
			{ID: collisionMcpA, DisplayName: collisionMcpA},
			{ID: collisionMcpB, DisplayName: collisionMcpB},
			{ID: collisionMcpC, DisplayName: collisionMcpC},
		},
		Projects:    []Project{proj},
		AdminSecret: NewSecret("supersecretadmin"),
	}
	return &appRouter{
		store:    &FileSettingsStore{cache: s, dir: t.TempDir(), sealer: testSealer()},
		tools:    tp,
		services: NewServiceRegistry(),
		onChange: func() {},
	}, tp
}

func TestCallTool_DisabledToolOnOneColliderIsNotAmbiguous(t *testing.T) {
	for name, order := range bothOrders() {
		t.Run(name, func(t *testing.T) {
			r, tp := narrowingRouter(t, order, Project{
				ID: "p", Name: "p", Path: "/tmp/p",
				AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
				DisabledTools: map[string][]string{collisionMcpA: {collidingTool}},
			}, nil)
			if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
				t.Fatalf("%q is disabled on %s, so only %s can serve it: %v", collidingTool, collisionMcpA, collisionMcpB, err)
			}
			if got := tp.dispatchedIDs(); !slices.Equal(got, []string{collisionMcpB}) {
				t.Fatalf("dispatched to %v, want [%s]", got, collisionMcpB)
			}
		})
	}
}

// Subtle: an absent allowed_tools entry means no tools at all for a remote
// profile — fs-a can serve nothing under this grant.
func TestCallTool_AllowedToolsOnOneColliderIsNotAmbiguous(t *testing.T) {
	for name, order := range bothOrders() {
		t.Run(name, func(t *testing.T) {
			r, tp := narrowingRouter(t, order, Project{
				ID: "p", Name: "p", Kind: ProjectKindRemote,
				AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
				AllowedTools:  map[string][]string{collisionMcpB: {collidingTool}},
				Access:        map[string]string{collisionMcpB: AccessWrite},
				AllowExternal: map[string]bool{collisionMcpB: true},
			}, nil)
			if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
				t.Fatalf("only %s allows %q under this profile: %v", collisionMcpB, collidingTool, err)
			}
			if got := tp.dispatchedIDs(); !slices.Equal(got, []string{collisionMcpB}) {
				t.Fatalf("dispatched to %v, want [%s]", got, collisionMcpB)
			}
		})
	}
}

// Deliberate: access mode is derived from the MCP's own readOnlyHint, so it
// must not narrow routing — otherwise an MCP could capture a name meant for
// another server by self-annotating read-only.
func TestCallTool_AnnotationDerivedLayersDoNotNarrowTheRoute(t *testing.T) {
	tools := map[string][]mcp.Tool{
		collisionMcpA: readOnlyTools(collidingTool),
		collisionMcpB: simpleTools(collidingTool),
		collisionMcpC: simpleTools("note_read"),
	}
	for name, order := range bothOrders() {
		t.Run(name, func(t *testing.T) {
			r, tp := narrowingRouter(t, order, Project{
				ID: "p", Name: "p", Kind: ProjectKindRemote,
				AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
				AllowedTools:  map[string][]string{collisionMcpA: {collidingTool}, collisionMcpB: {collidingTool}},
				Access:        map[string]string{collisionMcpA: AccessRead, collisionMcpB: AccessRead},
				AllowExternal: map[string]bool{collisionMcpA: true, collisionMcpB: true},
			}, tools)
			_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
			if err == nil {
				t.Fatalf("an MCP must not win a route by declaring readOnlyHint; call reached %v", tp.dispatchedIDs())
			}
			if got := tp.dispatchedIDs(); len(got) != 0 {
				t.Fatalf("a refused call still reached %v", got)
			}
		})
	}
}

func TestCallTool_TwoCollidersBothAllowedIsStillRefused(t *testing.T) {
	for name, order := range bothOrders() {
		t.Run(name, func(t *testing.T) {
			r, tp := narrowingRouter(t, order, Project{
				ID: "p", Name: "p", Path: "/tmp/p",
				AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
			}, nil)
			_, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken)
			if err == nil {
				t.Fatal("two MCPs both allowed to serve the name is the ambiguity the refusal exists for")
			}
			for _, want := range []string{collidingTool, collisionMcpA, collisionMcpB} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err.Error(), want)
				}
			}
			if got := tp.dispatchedIDs(); len(got) != 0 {
				t.Fatalf("a refused call still reached %v", got)
			}
		})
	}
}

// Subtle: the deny-set is built by walking settings.ExternalMcps, so an
// unregistered MCP gets no PermOff entry — omission is not the same as denial.
func TestCallTool_ConnectedButUnregisteredMcpIsNotACandidate(t *testing.T) {
	const ghost = "ghost"
	tools := map[string][]mcp.Tool{
		ghost:         simpleTools(collidingTool),
		collisionMcpB: simpleTools(collidingTool),
	}
	t.Run("ghost alone cannot serve", func(t *testing.T) {
		tp := newCollidingProvider([]string{ghost}, tools)
		s := &Settings{
			Version:      1,
			ExternalMcps: []ExternalMcp{{ID: collisionMcpB, DisplayName: collisionMcpB}},
			Projects: []Project{{ID: "p", Name: "p", Path: "/tmp/p",
				AllowedMcpIDs: []string{collisionMcpB},
				Token:         NewSecret(testToken), TokenHash: hashToken(testToken)}},
			AdminSecret: NewSecret("supersecretadmin"),
		}
		r := &appRouter{store: &FileSettingsStore{cache: s, dir: t.TempDir(), sealer: testSealer()},
			tools: tp, services: NewServiceRegistry(), onChange: func() {}}
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err == nil {
			t.Fatalf("an unregistered MCP served a call; dispatched=%v", tp.dispatchedIDs())
		}
		if got := tp.dispatchedIDs(); len(got) != 0 {
			t.Fatalf("an unregistered MCP was reached: %v", got)
		}
	})
	t.Run("ghost does not make a granted MCP ambiguous", func(t *testing.T) {
		tp := newCollidingProvider([]string{ghost, collisionMcpB}, tools)
		s := &Settings{
			Version:      1,
			ExternalMcps: []ExternalMcp{{ID: collisionMcpB, DisplayName: collisionMcpB}},
			Projects: []Project{{ID: "p", Name: "p", Path: "/tmp/p",
				AllowedMcpIDs: []string{collisionMcpB},
				Token:         NewSecret(testToken), TokenHash: hashToken(testToken)}},
			AdminSecret: NewSecret("supersecretadmin"),
		}
		r := &appRouter{store: &FileSettingsStore{cache: s, dir: t.TempDir(), sealer: testSealer()},
			tools: tp, services: NewServiceRegistry(), onChange: func() {}}
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
			t.Fatalf("a granted MCP was refused because an unregistered one also exposes the name: %v", err)
		}
		if got := tp.dispatchedIDs(); !slices.Equal(got, []string{collisionMcpB}) {
			t.Fatalf("dispatched to %v, want [%s]", got, collisionMcpB)
		}
	})
}

func TestListing_AgreesWithCallToolOnCollidingNames(t *testing.T) {
	count := func(t *testing.T, r *appRouter, name string) (int, int) {
		t.Helper()
		raw, err := r.ListTools(context.Background(), testToken)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		var listed []mcp.Tool
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatalf("unmarshal tools: %v", err)
		}
		inTools := 0
		for _, tl := range listed {
			if tl.Name == name {
				inTools++
			}
		}
		buckets, err := r.ListSkillBuckets(context.Background(), testToken)
		if err != nil {
			t.Fatalf("ListSkillBuckets: %v", err)
		}
		inSkills := 0
		for _, b := range buckets {
			for _, tl := range b.Tools {
				if tl.Name == name {
					inSkills++
				}
			}
		}
		return inTools, inSkills
	}

	t.Run("an ambiguous name is advertised nowhere", func(t *testing.T) {
		r, _ := narrowingRouter(t, []string{collisionMcpA, collisionMcpB}, Project{
			ID: "p", Name: "p", Path: "/tmp/p",
			AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
		}, nil)
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err == nil {
			t.Fatal("precondition: this grant must be ambiguous for the tool")
		}
		if inTools, inSkills := count(t, r, collidingTool); inTools != 0 || inSkills != 0 {
			t.Errorf("CallTool refuses %q but ListTools advertises it %d time(s) and the skill renderer %d time(s)",
				collidingTool, inTools, inSkills)
		}
		// The MCPs' unshared tools are untouched: this withholds a name, not a server.
		if inTools, _ := count(t, r, "a_only"); inTools != 1 {
			t.Errorf("an unshared tool was withheld too: a_only listed %d time(s)", inTools)
		}
	})

	t.Run("a name narrowed to one MCP is advertised once", func(t *testing.T) {
		r, _ := narrowingRouter(t, []string{collisionMcpA, collisionMcpB}, Project{
			ID: "p", Name: "p", Path: "/tmp/p",
			AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
			DisabledTools: map[string][]string{collisionMcpA: {collidingTool}},
		}, nil)
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
			t.Fatalf("precondition: the grant narrows this to one MCP: %v", err)
		}
		if inTools, inSkills := count(t, r, collidingTool); inTools != 1 || inSkills != 1 {
			t.Errorf("a callable tool is advertised %d time(s) by ListTools and %d time(s) by the skill renderer, want 1 and 1",
				inTools, inSkills)
		}
	})
}

func TestAudit_AmbiguityRefusalNamesNoMcpAndKeepsTheCollidersInTheError(t *testing.T) {
	for name, order := range bothOrders() {
		t.Run(name, func(t *testing.T) {
			rec := newTestAudit(t, nil)
			tp := newCollidingProvider(order, collisionTools())
			s := &Settings{
				Version: 1,
				ExternalMcps: []ExternalMcp{
					{ID: collisionMcpA, DisplayName: collisionMcpA},
					{ID: collisionMcpB, DisplayName: collisionMcpB},
					{ID: collisionMcpC, DisplayName: collisionMcpC},
				},
				Projects: []Project{{ID: "p", Name: "p", Path: "/tmp/p",
					AllowedMcpIDs: []string{collisionMcpA, collisionMcpB},
					Token:         NewSecret(testToken), TokenHash: hashToken(testToken),
					Context: collisionScopes()}},
				AdminSecret: NewSecret("supersecretadmin"),
			}
			r := &appRouter{store: &FileSettingsStore{cache: s, dir: t.TempDir(), sealer: testSealer()},
				tools: tp, services: NewServiceRegistry(), onChange: func() {}, audit: rec}

			if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err == nil {
				t.Fatal("precondition: this grant must be ambiguous")
			}
			events := readLoggedEvents(t, rec)
			if len(events) != 1 {
				t.Fatalf("expected 1 audit record, got %d", len(events))
			}
			ev := events[0]
			if ev.Outcome != AuditOutcomeDenied {
				t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomeDenied)
			}
			// Naming either collider would blame an MCP that never ran.
			if ev.McpID != "" {
				t.Errorf("mcp_id = %q on a refusal no MCP served", ev.McpID)
			}
			for _, want := range []string{collidingTool, collisionMcpA, collisionMcpB} {
				if !strings.Contains(ev.Error, want) {
					t.Errorf("audit error %q does not name %q — the record is the only place the colliders survive", ev.Error, want)
				}
			}
		})
	}
}
