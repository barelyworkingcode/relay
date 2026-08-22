package main

// ADR-011 decision 2c: the second axis relay can decide by itself — does this
// tool reach outside the host. It is orthogonal to the mode rather than a
// value of it, so the cases that matter are the ones where the two axes
// disagree: a read-only tool that reaches the network (web_fetch) and a
// mutating tool that does not (mail_create_draft).
//
// The polarity is inverted from readOnlyHint and this file is where that is
// pinned. MCP defaults openWorldHint to TRUE, so absent, null, malformed and a
// case variant all mean "open-world" and all deny; only an explicit boolean
// false under the specification's own spelling admits a tool to a grant that
// has not been given allow_external.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// The hint itself
// ---------------------------------------------------------------------------

func TestOpenWorldHint_OnlyAnExplicitFalseSaysAToolStaysOnThisHost(t *testing.T) {
	cases := []struct {
		name        string
		annotations string
		wantOpen    bool
	}{
		// The whole point: absent is open-world, because that is the MCP
		// specification's default and the fail-closed reading of silence.
		{"absent", ``, true},
		{"empty object", `{}`, true},
		{"null hint", `{"openWorldHint":null}`, true},
		{"explicit true", `{"openWorldHint":true}`, true},
		{"explicit false", `{"openWorldHint":false}`, false},
		// Malformed server-supplied bytes must deny and must not panic.
		{"not an object", `"open, honest"`, true},
		{"broken json", `{"openWorldHint":`, true},
		{"wrong type", `{"openWorldHint":"false"}`, true},
		{"number", `{"openWorldHint":0}`, true},
		// Case variants. encoding/json would have matched every one of these
		// onto a struct field; a map lookup does not. A near-miss key is not a
		// declaration an operator could read and diff, so it is not one relay
		// will widen a grant on.
		{"capitalised", `{"OpenWorldHint":false}`, true},
		{"lowercased", `{"openworldhint":false}`, true},
		{"snake case", `{"open_world_hint":false}`, true},
		// The other hint says nothing about this one.
		{"only readOnlyHint", `{"readOnlyHint":true}`, true},
		{"both", `{"readOnlyHint":true,"openWorldHint":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := mcp.Tool{Name: "t", Annotations: json.RawMessage(tc.annotations)}
			if got := toolIsOpenWorld(&tool); got != tc.wantOpen {
				t.Errorf("toolIsOpenWorld(%s) = %v, want %v", tc.annotations, got, tc.wantOpen)
			}
		})
	}
	// A definition relay could not find is open-world: a grant must not be
	// widened by relay's own ignorance of what it is about to call.
	if !toolIsOpenWorld(nil) {
		t.Error("toolIsOpenWorld(nil) said a tool relay cannot see stays on this host")
	}
}

// The inversion stated as an assertion rather than as a comment, because the
// two functions look like each other and the tidy that merges them is exactly
// what this catches: the SAME blob must answer "not read-only" and
// "open-world", and both answers deny.
func TestHints_SilenceDeniesOnBothAxesInOppositeSpellings(t *testing.T) {
	silent := mcp.Tool{Name: "t"}
	if readOnlyHintTrue(&silent) {
		t.Error("an unannotated tool claimed to be read-only")
	}
	if !toolIsOpenWorld(&silent) {
		t.Error("an unannotated tool claimed to stay on this host")
	}
}

// ---------------------------------------------------------------------------
// The grant
// ---------------------------------------------------------------------------

func TestExternalAllowed_DefaultsToFalseAndAnythingButTrueIsFalse(t *testing.T) {
	var nilTok *StoredToken
	if nilTok.ExternalAllowed("macmcp") {
		t.Error("a nil token allowed external access")
	}
	if (&StoredToken{}).ExternalAllowed("macmcp") {
		t.Error("a token with no map allowed external access")
	}
	tok := &StoredToken{AllowExternal: map[string]bool{"macmcp": false, "other": true}}
	if tok.ExternalAllowed("macmcp") {
		t.Error("an explicit false allowed external access")
	}
	if tok.ExternalAllowed("unnamed") {
		t.Error("an MCP with no entry allowed external access")
	}
	// Per MCP, not per token: granting one outbound channel is not granting
	// every MCP's.
	if !tok.ExternalAllowed("other") {
		t.Error("an explicit true was not honoured")
	}
}

// The asymmetry AccessMode has, deliberately absent here. A reader arriving
// from that function will expect one, so it is measured rather than asserted
// in a comment.
func TestAllowExternal_HasNoLocalRemoteAsymmetry(t *testing.T) {
	for _, kind := range []ProjectKind{ProjectKindLocal, ProjectKindRemote} {
		local := &StoredToken{ProjectKind: kind}
		if local.ExternalAllowed("macmcp") {
			t.Errorf("kind %q defaulted to allowing external access", kind)
		}
	}
	// And through the router, where a local project's default write mode is
	// what would otherwise hide the difference: a local project reaches
	// mail_create_draft and does not reach web_fetch.
	r := newProfileRouter(t, profileOpts{})
	got := listedToolNames(t, r)
	if !slices.Contains(got, "mail_create_draft") {
		t.Errorf("a local project lost a mutating LOCAL tool: %v", got)
	}
	for _, outbound := range []string{"web_fetch", "mail_send"} {
		if slices.Contains(got, outbound) {
			t.Errorf("a local project with no allow_external was served %q", outbound)
		}
		if _, err := r.CallTool(context.Background(), outbound, json.RawMessage(`{}`), testToken); err == nil {
			t.Errorf("a local project with no allow_external called %q", outbound)
		}
	}
}

// ---------------------------------------------------------------------------
// The two axes crossing, which is the reason this is not a third mode
// ---------------------------------------------------------------------------

// web_fetch's shape: readOnlyHint true and openWorldHint true. The mode admits
// it — it is honestly read-only — and it is still refused, which is the
// outbound channel a read-only profile held before this decision.
func TestReadOnlyAndOpenWorld_IsRefusedToAReadProfileWithoutTheGrant(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"web_*", "contacts_*"}},
	})
	if got := listedToolNames(t, r); slices.Contains(got, "web_fetch") {
		t.Errorf("a read-only profile was served web_fetch: %v", got)
	} else if !slices.Contains(got, "contacts_list_groups") {
		t.Errorf("the same profile lost a read-only LOCAL tool: %v", got)
	}
	_, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken)
	if err == nil {
		t.Fatal("a read-only profile with no allow_external fetched a URL")
	}
	if !strings.Contains(err.Error(), "reaches outside this host") {
		t.Errorf("the refusal did not name the layer that made it: %v", err)
	}
	// The mode is not what refused it, and the grant is what admits it.
	r = newProfileRouter(t, profileOpts{
		kind:          ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"web_*"}},
		allowExternal: map[string]bool{"macmcp": true},
	})
	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a read profile allowing external access could not fetch a URL: %v", err)
	}
}

// mail_create_draft's shape: readOnlyHint false and openWorldHint false. This
// is the request the feature exists for — an agent that composes a draft a
// human reviews and sends — and it must work with no outbound grant at all.
func TestMutatingAndLocal_IsAdmittedToAWriteProfileWithoutTheGrant(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
	})
	if got := listedToolNames(t, r); !slices.Contains(got, "mail_create_draft") {
		t.Fatalf("draft-but-not-send did not list mail_create_draft: %v", got)
	}
	if _, err := r.CallTool(context.Background(), "mail_create_draft", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("draft-but-not-send could not draft: %v", err)
	}
	// And the whole point of it: the same profile cannot send.
	if _, err := r.CallTool(context.Background(), "mail_send", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("draft-but-not-send sent mail")
	}
}

// ---------------------------------------------------------------------------
// The list paths, which must filter exactly as the call path does
// ---------------------------------------------------------------------------

// A tool a caller cannot call must not be advertised to it: a listing that
// showed one would have an agent spend a call discovering a refusal, and it is
// the surface an access profile is told its own limits through (decision 8).
func TestListPaths_HideWhatTheOutboundGrantRefuses(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "mail_search", Category: "Mail", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)},
		{Name: "web_fetch", Category: "Web", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":true}`)},
		{Name: "weather_now", Category: "Weather", Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
	}
	r := newProfileRouter(t, profileOpts{tools: tools})

	if got := listedToolNames(t, r); strings.Join(got, ",") != "mail_search" {
		t.Fatalf("ListTools served %v, want only the tool that stays on this host", got)
	}
	// ListSkillBuckets is a second implementation of the same membership and
	// has drifted from ListTools before. A tool relay will refuse must not
	// reach a generated SKILL.md either.
	buckets, err := r.ListSkillBuckets(context.Background(), testToken)
	if err != nil {
		t.Fatalf("ListSkillBuckets: %v", err)
	}
	var listed []string
	for _, b := range buckets {
		for _, tool := range b.Tools {
			listed = append(listed, tool.Name)
		}
	}
	slices.Sort(listed)
	if strings.Join(listed, ",") != "mail_search" {
		t.Fatalf("ListSkillBuckets served %v, want only the tool that stays on this host", listed)
	}
}

// ---------------------------------------------------------------------------
// The audit record
// ---------------------------------------------------------------------------

// The grant in force goes on the record beside the mode, and the FALSE is the
// value that matters: it is the resting state and the one a refusal on this
// layer was decided by. A bool that vanished when false would make "the grant
// was not given" and "nobody recorded a grant" the same absent key.
func TestAudit_RecordsTheOutboundGrantOnBothAPermittedCallAndARefusal(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*", "web_*"}},
	})
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("web_fetch was allowed")
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("expected 2 records, got %d", len(events))
	}
	for i, ev := range events {
		if ev.AllowExternal == nil {
			t.Fatalf("record %d carried no allow_external at all", i)
		}
		if *ev.AllowExternal {
			t.Errorf("record %d recorded a grant the profile does not hold", i)
		}
	}
	if events[1].Outcome != AuditOutcomeDenied {
		t.Errorf("the refusal was recorded as %q", events[1].Outcome)
	}
	// It is on the wire shape too, not only in the struct: `relay audit` and
	// anything grepping the file read the JSON.
	line, err := json.Marshal(events[1])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if !strings.Contains(string(line), `"allow_external":false`) {
		t.Fatalf("the refused grant is not on the audit line: %s", line)
	}

	// And a grant that WAS given is recorded as given.
	r = newProfileRouter(t, profileOpts{
		kind:          ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"web_*"}},
		allowExternal: map[string]bool{"macmcp": true},
	})
	rec = newTestAudit(t, nil)
	r.audit = rec
	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	events = readLoggedEvents(t, rec)
	if len(events) != 1 || events[0].AllowExternal == nil || !*events[0].AllowExternal {
		t.Fatalf("a call made under an outbound grant did not record it: %+v", events)
	}
}
