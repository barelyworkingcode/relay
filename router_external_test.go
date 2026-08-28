package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"relaygo/mcp"
)

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
	// nil (a tool definition relay could not find) is open-world too: a grant
	// must not be widened by relay's own ignorance of what it is about to call.
	if !toolIsOpenWorld(nil) {
		t.Error("toolIsOpenWorld(nil) said a tool relay cannot see stays on this host")
	}
}

// readOnlyHintTrue and toolIsOpenWorld look like each other; a tidy that
// merged them would lose that the SAME blob must answer "not read-only" and
// "open-world", both by denying.
func TestHints_SilenceDeniesOnBothAxesInOppositeSpellings(t *testing.T) {
	silent := mcp.Tool{Name: "t"}
	if readOnlyHintTrue(&silent) {
		t.Error("an unannotated tool claimed to be read-only")
	}
	if !toolIsOpenWorld(&silent) {
		t.Error("an unannotated tool claimed to stay on this host")
	}
}

func TestExternalAllowed_ExplicitValueWinsInBothDirections(t *testing.T) {
	var nilTok *StoredToken
	if nilTok.ExternalAllowed("macmcp") {
		t.Error("a nil token allowed external access")
	}
	tok := &StoredToken{AllowExternal: map[string]bool{"macmcp": false, "other": true}}
	if tok.ExternalAllowed("macmcp") {
		t.Error("an explicit false did not refuse")
	}
	if !tok.ExternalAllowed("other") {
		t.Error("an explicit true was not honoured")
	}
	// AllowExternal is a map[string]bool rather than a set of allowed ids so a
	// LOCAL record — allowed by default — can still store an explicit false.
	local := &StoredToken{AllowExternal: map[string]bool{"macmcp": false}}
	if local.ExternalAllowed("macmcp") {
		t.Error("a local project could not refuse its own outbound channel")
	}
}

// The name states "no asymmetry" so the claim cannot quietly disappear if
// this test is ever renamed or merged away.
func TestAllowExternal_HasNoLocalRemoteAsymmetry_InvertedTheAsymmetryIsTheDecision(t *testing.T) {
	if (&StoredToken{ProjectKind: ProjectKindRemote}).ExternalAllowed("macmcp") {
		t.Error("an access profile defaulted to allowing external access")
	}
	for _, kind := range []ProjectKind{ProjectKindLocal, ""} {
		local := &StoredToken{ProjectKind: kind}
		if !local.ExternalAllowed("macmcp") {
			t.Errorf("kind %q defaulted to refusing external access", kind)
		}
	}

	r := newProfileRouter(t, profileOpts{})
	got := listedToolNames(t, r)
	for _, outbound := range []string{"web_fetch", "mail_send"} {
		if !slices.Contains(got, outbound) {
			t.Errorf("a local project was refused %q it could reach with curl: %v", outbound, got)
		}
		if _, err := r.CallTool(context.Background(), outbound, json.RawMessage(`{}`), testToken); err != nil {
			t.Errorf("a local project could not call %q: %v", outbound, err)
		}
	}

	r = newProfileRouter(t, profileOpts{allowExternal: map[string]bool{"macmcp": false}})
	got = listedToolNames(t, r)
	if slices.Contains(got, "web_fetch") {
		t.Errorf("a local project that refused its outbound channel was served web_fetch: %v", got)
	}
	if !slices.Contains(got, "mail_create_draft") {
		t.Errorf("refusing the outbound channel took a LOCAL tool with it: %v", got)
	}

	r = newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*", "web_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
	})
	got = listedToolNames(t, r)
	for _, outbound := range []string{"web_fetch", "mail_send"} {
		if slices.Contains(got, outbound) {
			t.Errorf("an access profile with no allow_external was served %q: %v", outbound, got)
		}
	}
	if !slices.Contains(got, "mail_create_draft") {
		t.Errorf("the same profile lost a mutating LOCAL tool: %v", got)
	}
}

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
	r = newProfileRouter(t, profileOpts{
		kind:          ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"web_*"}},
		allowExternal: map[string]bool{"macmcp": true},
	})
	if _, err := r.CallTool(context.Background(), "web_fetch", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a read profile allowing external access could not fetch a URL: %v", err)
	}
}

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
	if _, err := r.CallTool(context.Background(), "mail_send", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("draft-but-not-send sent mail")
	}
}

func TestListPaths_HideWhatTheOutboundGrantRefuses(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "mail_search", Category: "Mail", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":false}`)},
		{Name: "web_fetch", Category: "Web", Annotations: json.RawMessage(`{"readOnlyHint":true,"openWorldHint":true}`)},
		{Name: "weather_now", Category: "Weather", Annotations: json.RawMessage(`{"readOnlyHint":true}`)},
	}
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*", "web_*", "weather_*"}},
		tools:        tools,
	})

	if got := listedToolNames(t, r); strings.Join(got, ",") != "mail_search" {
		t.Fatalf("ListTools served %v, want only the tool that stays on this host", got)
	}
	// ListSkillBuckets is a second implementation of the same membership test:
	// a tool relay will refuse must not reach a generated SKILL.md either.
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

// AllowExternal is a *bool: a plain bool that omitted itself when false would
// make "the grant was refused" and "nobody recorded a grant" the same absent
// key.
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
	line, err := json.Marshal(events[1])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if !strings.Contains(string(line), `"allow_external":false`) {
		t.Fatalf("the refused grant is not on the audit line: %s", line)
	}

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
