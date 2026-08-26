package main

// Follow-up to issue #35's fix. Resolving a colliding tool name by the MCP
// grant ALONE refused more than it had to: an MCP the grant allows, but on
// which this particular tool is denied by the operator's own allowlist or
// denylist, was still counted as a collider. The grant had already said which
// server should serve the name and relay called it ambiguous anyway.
//
// These pin the narrowed rule and, just as importantly, its two edges: the
// annotation-derived layers must NOT narrow (an MCP would then be able to
// route calls to itself by editing its own hints), and two MCPs that both
// genuinely allow the name are still refused.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"relaygo/mcp"
)

// narrowingRouter builds a router over two colliding MCPs plus a third that
// shares nothing, with the project spelled out field by field so each test can
// say exactly which operator layer is doing the narrowing.
func narrowingRouter(t *testing.T, order []string, proj Project, tools map[string][]mcp.Tool) (*appRouter, *collidingProvider) {
	t.Helper()
	if tools == nil {
		tools = collisionTools()
	}
	tp := newCollidingProvider(order, tools)
	proj.Token = testToken
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
		AdminSecret: "supersecretadmin",
	}
	return &appRouter{
		store:    &FileSettingsStore{cache: s, dir: t.TempDir()},
		tools:    tp,
		services: NewServiceRegistry(),
		onChange: func() {},
	}, tp
}

// A denylist entry on one collider leaves exactly one MCP able to serve the
// name, so there is nothing left to be ambiguous about.
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

// Same for an allowlist that names the tool on one collider only. This is the
// sharpest case on a remote profile, where an ABSENT allowed_tools entry means
// no tools at all: fs-a can serve nothing under this grant, yet before the
// narrowing it still made the name uncallable.
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

// THE EDGE THAT MATTERS. The access mode refuses a mutating tool under a read
// grant, but it decides that from the MCP's OWN readOnlyHint. If routing
// narrowed by it, an MCP could make itself the only candidate for a name by
// annotating a tool read-only, and capture a call meant for another server.
// So a collider the mode would refuse is still a collider, and the call is
// refused rather than routed to the survivor.
func TestCallTool_AnnotationDerivedLayersDoNotNarrowTheRoute(t *testing.T) {
	// fs-a's copy is read-only, fs-b's is not; the grant is read-only on both.
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

// Both MCPs genuinely allow the name: still refused, still naming both. The
// narrowing must not have turned the ambiguity rule off.
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

// A connection relay holds no configuration for must not be a candidate: the
// deny-set is built by walking settings.ExternalMcps, so an unregistered MCP
// gets no PermOff entry and would otherwise read as granted to every token.
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
				Token:         testToken, TokenHash: hashToken(testToken)}},
			AdminSecret: "supersecretadmin",
		}
		r := &appRouter{store: &FileSettingsStore{cache: s, dir: t.TempDir()},
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
				Token:         testToken, TokenHash: hashToken(testToken)}},
			AdminSecret: "supersecretadmin",
		}
		r := &appRouter{store: &FileSettingsStore{cache: s, dir: t.TempDir()},
			tools: tp, services: NewServiceRegistry(), onChange: func() {}}
		if _, err := r.CallTool(context.Background(), collidingTool, json.RawMessage(`{}`), testToken); err != nil {
			t.Fatalf("a granted MCP was refused because an unregistered one also exposes the name: %v", err)
		}
		if got := tp.dispatchedIDs(); !slices.Equal(got, []string{collisionMcpB}) {
			t.Fatalf("dispatched to %v, want [%s]", got, collisionMcpB)
		}
	})
}

// The listing and dispatch must agree. A name CallTool refuses outright must
// not be advertised by tools/list or written into a SKILL.md, and a name the
// operator's layers have narrowed to one MCP must still be advertised.
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
