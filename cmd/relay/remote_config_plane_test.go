package main

// The remote listener's configuration plane (ADR-018 decision 4): what a
// cli-admin enrolment may reach over mTLS, and — just as important — what it
// structurally cannot. Acceptance criteria references are the implementing
// spec's own numbering (§8).

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// setCLIAdmin flips an enrolment's bit directly through updateEnrolment
// (bypassing EnrolmentOps' own gate — the toggle itself is covered
// elsewhere), so these tests can drive the bit deterministically.
func setCLIAdmin(t *testing.T, store config.SettingsStore, clientID string, on bool) {
	t.Helper()
	v := on
	if _, _, err := updateEnrolment(store, enrolmentUpdateRequest{ClientID: clientID, CLIAdmin: &v}); err != nil {
		t.Fatalf("setCLIAdmin(%s, %v): %v", clientID, on, err)
	}
}

// ---------------------------------------------------------------------------
// AC-8: bit off => no config route reachable
// ---------------------------------------------------------------------------

func TestCliAdmin_BitOffMakesConfigRouteUnreachable(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()
	if c == nil {
		t.Fatal("dial failed")
	}

	before := odwSnap(t, f.dir)

	resp := c.roundTrip(`{"type":"DescribeGrant"}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "cli-admin") {
		t.Fatalf("DescribeGrant with the bit off = %s %q, want a cli-admin refusal", resp.Type, resp.Message)
	}
	resp = c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "cli-admin") {
		t.Fatalf("NarrowGrant with the bit off = %s %q, want a cli-admin refusal", resp.Type, resp.Message)
	}
	before.assertUntouched(t, f.dir, "config requests with the bit off")

	resp = c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespTools {
		t.Fatalf("ListTools on the same connection: %s %s", resp.Type, resp.Message)
	}
	resp = c.roundTrip(`{"type":"CallTool","name":"mail_search","arguments":{}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("CallTool on the same connection: %s %s", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// AC-9: bit on => the enrolment can narrow its own granted tools
// ---------------------------------------------------------------------------

func TestCliAdmin_NarrowsOwnGrantedTools(t *testing.T) {
	// secondTool gives the fixture MCP a second tool ("mail_send") the
	// stored "mail_*" pattern already grants, so ListTools returning
	// exactly [mail_search] after narrowing is evidence the narrow REMOVED
	// mail_send — not merely that the narrowed set happens to have one
	// entry, which the fixture's single-tool default would prove
	// identically with narrowing absent (Finding F).
	f := newRemoteFixture(t, remoteFixtureOpts{secondTool: "mail_send"})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespTools {
		t.Fatalf("ListTools before narrowing: %s %s", resp.Type, resp.Message)
	}
	var before []mcp.Tool
	assertNoErr(t, json.Unmarshal(resp.Tools, &before), "parse tools")
	if len(before) != 2 {
		t.Fatalf("tool list before narrowing = %+v, want both mail_search and mail_send", before)
	}

	resp = c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}

	proj, _ := config.FindProjectByID(f.store.Get(), f.project.ID)
	if proj == nil {
		t.Fatal("the profile vanished")
	}
	if got := proj.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_search" {
		t.Fatalf("stored allowed_tools[macmcp] = %v, want [mail_search]", got)
	}

	resp = c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespTools {
		t.Fatalf("ListTools after narrowing: %s %s", resp.Type, resp.Message)
	}
	var tools []mcp.Tool
	assertNoErr(t, json.Unmarshal(resp.Tools, &tools), "parse tools")
	if len(tools) != 1 || tools[0].Name != "mail_search" {
		t.Fatalf("tool list after narrowing = %+v, want exactly [mail_search] — mail_send must have disappeared", tools)
	}
}

// ---------------------------------------------------------------------------
// Finding A: a resend of an already-applied narrowing must not rewrite
// settings.json or double the audit trail. narrowsOnly accepts "request
// equals what is already stored" — that is not a widening — but
// applyProjectUpdate's mutators write unconditionally once invoked, and
// withDeclinable's only lever against a write is a callback error. Six
// identical NarrowGrant requests must produce one write and one
// config_change, not six.
// ---------------------------------------------------------------------------

func TestCliAdmin_RepeatedIdenticalNarrowingWritesNothingTheSecondTime(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	const req = `{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`

	resp := c.roundTrip(req)
	if resp.Type != bridge.RespResult {
		t.Fatalf("first NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}

	countConfigChanges := func() int {
		n := 0
		for _, e := range readLoggedEvents(t, f.audit) {
			if e.Event == AuditEventConfigChange && e.Credential == auditCredentialProjectGrant {
				n++
			}
		}
		return n
	}
	if got := countConfigChanges(); got != 1 {
		t.Fatalf("config_change count after the first narrowing = %d, want 1", got)
	}

	before := odwSnap(t, f.dir)

	// Five more IDENTICAL requests — a resend, not a further narrowing.
	for i := 0; i < 5; i++ {
		resp = c.roundTrip(req)
		if resp.Type != bridge.RespResult {
			t.Fatalf("repeat NarrowGrant #%d refused: %s %s", i, resp.Type, resp.Message)
		}
		var result remoteNarrowGrantResult
		assertNoErr(t, json.Unmarshal(resp.Result, &result), "parse remoteNarrowGrantResult")
		if len(result.Changed) != 0 {
			t.Errorf("repeat NarrowGrant #%d reported Changed = %v, want none", i, result.Changed)
		}
	}

	before.assertUntouched(t, f.dir, "five identical resends of an already-applied narrowing")
	if got := countConfigChanges(); got != 1 {
		t.Fatalf("config_change count after five identical resends = %d, want still 1", got)
	}
}

// ---------------------------------------------------------------------------
// AC-10: bit on => widening refused on every axis
// ---------------------------------------------------------------------------

func TestCliAdmin_WideningRefusedOnEveryAxis(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	t.Run("mcp id not already granted", func(t *testing.T) {
		before := odwSnap(t, f.dir)
		resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_mcp_ids":["macmcp","other"]}}`)
		if resp.Type != bridge.RespError {
			t.Fatalf("widening allowed_mcp_ids was accepted: %s", resp.Type)
		}
		before.assertUntouched(t, f.dir, "widen allowed_mcp_ids")
	})

	t.Run("access write refused", func(t *testing.T) {
		before := odwSnap(t, f.dir)
		resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"access":{"macmcp":"write"}}}`)
		if resp.Type != bridge.RespError {
			t.Fatalf("access:write was accepted: %s", resp.Type)
		}
		before.assertUntouched(t, f.dir, "widen access")
	})

	t.Run("allow_external true refused", func(t *testing.T) {
		before := odwSnap(t, f.dir)
		resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allow_external":{"macmcp":true}}}`)
		if resp.Type != bridge.RespError {
			t.Fatalf("allow_external:true was accepted: %s", resp.Type)
		}
		before.assertUntouched(t, f.dir, "widen allow_external")
	})

	// The stored set has to already be a literal name for "the stored set
	// does not match" to be the reason (the spec's own "mail_* from
	// mail_search" example) — so narrow to a literal first, legitimately.
	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("legitimate narrowing to mail_search was refused: %s %s", resp.Type, resp.Message)
	}

	for _, pattern := range []string{"mail_*", "**"} {
		t.Run("tool pattern "+pattern, func(t *testing.T) {
			before := odwSnap(t, f.dir)
			resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["` + pattern + `"]}}}`)
			if resp.Type != bridge.RespError {
				t.Fatalf("widening allowed_tools to %q was accepted: %s", pattern, resp.Type)
			}
			before.assertUntouched(t, f.dir, "widen allowed_tools to "+pattern)
		})
	}
}

// ---------------------------------------------------------------------------
// Finding 1 (security): an escaped glob must not widen the stored grant.
// path.Match treats '\' as an escape, same as '*', '?' and '['; a requested
// pattern containing one can match a NAME hasGlobMeta calls literal while
// the pattern itself, once stored, matches a different, wider set of tool
// names than what was validated.
// ---------------------------------------------------------------------------

func TestCliAdmin_EscapedGlobCannotWidenGrant(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	// Stored grant: "mail_?x" matches any 7-character name of that shape
	// (e.g. "mail_ax") but NOT "mail_x" (6 characters — "?" requires
	// exactly one character in that position).
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		s.UpdateProjectAllowedTools(f.project.ID, map[string][]string{"macmcp": {"mail_?x"}})
	}), "seed stored pattern")
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	before := odwSnap(t, f.dir)
	// "mail_\x" (a backslash-escaped "x") reads, to toolAllowedByPatterns,
	// as the literal NAME "mail_\x" — which "mail_?x" matches, since "?"
	// matches any single character including "\". narrowsOnly's
	// literal-tool-name branch is meant to accept only a requested pattern
	// whose OWN matched set is a subset of what's stored. But once stored,
	// "mail_\x" is not a literal name any more — path.Match reads it as a
	// PATTERN where "\x" escapes to the literal character "x", so the
	// stored pattern matches the tool named "mail_x". "mail_?x" does not
	// match "mail_x": the request would reach a tool its own stored grant
	// denies.
	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_\\x"]}}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("escaped-glob pattern %q was accepted: %s %s (this widens the stored grant — see the test's own comment)",
			`mail_\x`, resp.Type, resp.Message)
	}
	before.assertUntouched(t, f.dir, "escaped-glob widening attempt")
}

// ---------------------------------------------------------------------------
// AC-11 / AC-12: bit on => still no command registration, no minting
// ---------------------------------------------------------------------------

func TestCliAdmin_CannotRegisterACommand(t *testing.T) {
	if _, ok := remoteHandlers[bridge.ReqAdminOp]; ok {
		t.Fatal("admin_op is registered on remoteHandlers")
	}
	if _, ok := remoteConfigHandlers[bridge.ReqAdminOp]; ok {
		t.Fatal("admin_op is registered on remoteConfigHandlers")
	}

	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"admin_op","name":"mcp.register","arguments":{}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("admin_op/mcp.register was answered: %s %s", resp.Type, resp.Message)
	}
}

func TestCliAdmin_CannotMintACredential(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	before := len(f.store.Get().APICredentials)
	c := f.dial()

	resp := c.roundTrip(`{"type":"admin_op","name":"credential.mint","arguments":{}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("admin_op/credential.mint was answered: %s %s", resp.Type, resp.Message)
	}
	if got := len(f.store.Get().APICredentials); got != before {
		t.Fatalf("APICredentials count changed: before %d, after %d", before, got)
	}
}

// ---------------------------------------------------------------------------
// AC-13: bit on => cannot touch another enrolment or its profile
// ---------------------------------------------------------------------------

func TestCliAdmin_CannotTouchAnotherEnrolmentsProfile(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)

	var calProj config.Project
	var createErr error
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		calProj, createErr = createProjectWithTokenKind(s, config.ProjectKindRemote, "Calendar", "", []string{"macmcp"}, []string{}, nil, nil)
	}), "create B's profile")
	assertNoErr(t, createErr, "create B's profile")
	_, err := createEnrolment(f.store, enrolmentRequest{ClientID: "hermes-cal", ProjectIDs: []string{calProj.ID}})
	assertNoErr(t, err, "createEnrolment for B")

	before := odwSnap(t, f.dir)
	c := f.dial() // dials as A (hermes-mail)
	resp := c.roundTrip(`{"type":"NarrowGrant","project_id":"` + calProj.ID + `","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("A reached B's profile: %s %s", resp.Type, resp.Message)
	}
	if !strings.Contains(resp.Message, "does not grant") {
		t.Errorf("refusal message = %q, want it to name the missing grant", resp.Message)
	}
	before.assertUntouched(t, f.dir, "A narrowing B's profile")
}

// ---------------------------------------------------------------------------
// Finding 2: DescribeGrant/NarrowGrant must not disclose sibling enrolments.
// grantView.Enrolments is built for the operator-facing `relay grant`; going
// over the wire verbatim to a remote caller lets enrolment A, describing its
// OWN posture, learn the client_id and cli_admin state of every other
// enrolment granting the same profile. The reachability boundary (A cannot
// ACT on B) still holds — this is about what A can learn about B.
// ---------------------------------------------------------------------------

func TestCliAdmin_DescribeGrantDoesNotDiscloseSiblingEnrolments(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	// B grants the SAME profile A holds, so A's own posture and the
	// operator's `relay grant` view of that profile both name B.
	_, err := createEnrolment(f.store, enrolmentRequest{ClientID: "hermes-cal", ProjectIDs: []string{f.project.ID}})
	assertNoErr(t, err, "createEnrolment for sibling B")

	c := f.dial() // dials as A (hermes-mail)
	resp := c.roundTrip(`{"type":"DescribeGrant"}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("DescribeGrant refused: %s %s", resp.Type, resp.Message)
	}
	if strings.Contains(string(resp.Result), "hermes-cal") {
		t.Fatalf("DescribeGrant result names sibling enrolment hermes-cal: %s", resp.Result)
	}
	var view grantView
	assertNoErr(t, json.Unmarshal(resp.Result, &view), "parse grantView")
	if len(view.Enrolments) != 0 {
		t.Fatalf("DescribeGrant.Enrolments = %+v, want empty — a remote caller's own posture, not sibling enumeration", view.Enrolments)
	}

	// The operator-facing view is untouched: relay grant still names both
	// A and B on the same profile.
	opView := newGrantView(f.store.Get(), f.project)
	var sawMail, sawCal bool
	for _, e := range opView.Enrolments {
		if e.ClientID == "hermes-mail" {
			sawMail = true
		}
		if e.ClientID == "hermes-cal" {
			sawCal = true
		}
	}
	if !sawMail || !sawCal {
		t.Fatalf("newGrantView (relay grant) Enrolments = %+v, want both hermes-mail and hermes-cal", opView.Enrolments)
	}
}

func TestCliAdmin_NarrowGrantResultDoesNotDiscloseSiblingEnrolments(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	_, err := createEnrolment(f.store, enrolmentRequest{ClientID: "hermes-cal", ProjectIDs: []string{f.project.ID}})
	assertNoErr(t, err, "createEnrolment for sibling B")

	c := f.dial()
	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}
	if strings.Contains(string(resp.Result), "hermes-cal") {
		t.Fatalf("NarrowGrant result names sibling enrolment hermes-cal: %s", resp.Result)
	}
	var result remoteNarrowGrantResult
	assertNoErr(t, json.Unmarshal(resp.Result, &result), "parse remoteNarrowGrantResult")
	if len(result.Grant.Enrolments) != 0 {
		t.Fatalf("NarrowGrant result Grant.Enrolments = %+v, want empty", result.Grant.Enrolments)
	}
}

// TestRemoteNarrowFields_NamesNoOtherIdentity is an INVENTORY, not a
// denylist (Finding E): the earlier version banned four specific tag names
// ("client_id", "enrolment", "project_id", "owner"), so a field tagged
// e.g. "acting_as" or "target_project" — naming another object just as
// surely, in a word the denylist did not happen to think of — would have
// passed it silently. Asserting the exact field set instead means ANY
// added field fails this test until someone updates the list on purpose,
// which is where "does this name something other than the caller's own"
// gets asked and answered.
func TestRemoteNarrowFields_NamesNoOtherIdentity(t *testing.T) {
	want := map[string]bool{
		"allowed_mcp_ids": true,
		"allowed_tools":   true,
		"access":          true,
		"allow_external":  true,
	}
	typ := reflect.TypeOf(remoteNarrowFields{})
	got := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		got[tag] = true
	}
	for tag := range want {
		if !got[tag] {
			t.Errorf("remoteNarrowFields is missing the %q field", tag)
		}
	}
	for tag := range got {
		if !want[tag] {
			t.Errorf("remoteNarrowFields has an unlisted field tagged %q — every field on the remote configuration surface must be named in this test's own list, on purpose, before it ships", tag)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-14: bit on => cannot flip allow_cwd_auth
// ---------------------------------------------------------------------------

func TestCliAdmin_CannotFlipAllowCwdAuth(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allow_cwd_auth":true}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("allow_cwd_auth was accepted: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "allow_cwd_auth") {
		t.Errorf("refusal does not name the unknown field: %q", resp.Message)
	}
}

func TestRemoteNarrowFields_HasNoAllowCwdAuthField(t *testing.T) {
	typ := reflect.TypeOf(remoteNarrowFields{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "AllowCwdAuth" {
			t.Fatal("remoteNarrowFields has an AllowCwdAuth field")
		}
	}
}

func TestValidateProjectShape_StillRefusesAllowCwdAuthOnRemote(t *testing.T) {
	proj := &config.Project{Kind: config.ProjectKindRemote, AllowCwdAuth: true}
	err := validateProjectShape(proj)
	if err == nil || !strings.Contains(err.Error(), "allow_cwd_auth") {
		t.Fatalf("validateProjectShape(remote, allow_cwd_auth=true) = %v, want a refusal naming allow_cwd_auth", err)
	}
}

// ---------------------------------------------------------------------------
// AC-15: flipping the bit off denies the NEXT request, not the connection
// ---------------------------------------------------------------------------

func TestCliAdmin_FlippingBitOffDeniesTheNextRequestNotTheConnection(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("first NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}

	// Out of band, for real: written through a SECOND store instance
	// pointed at the same file (f.cliWriter(), the same stand-in for "a
	// separate `relay enrol update` process" that TestRemoteServer_
	// AcceptsAnEnrolmentCreatedByAnotherProcess uses), not through f.store
	// — the listener's own store, which shares its in-memory cache with
	// the toggle if called directly and would prove only that the
	// listener re-reads a change already sitting in its own process, not
	// the freshSettings-vs-Get() property this criterion names.
	setCLIAdmin(t, f.cliWriter(), "hermes-mail", false)

	// Precondition: the listener's own cached settings must still show the
	// bit on, or this test is back to exercising the in-process case and
	// proves nothing about freshSettings.
	if e := findEnrolment(f.store.Get(), "hermes-mail"); e == nil || !e.CLIAdmin {
		t.Fatal("the listener's cached settings already show cli_admin off; " +
			"this test no longer reproduces the cross-process condition it was written for")
	}

	resp = c.roundTrip(`{"type":"NarrowGrant","arguments":{}}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "cli-admin") {
		t.Fatalf("next NarrowGrant on the same connection = %s %q, want a cli-admin refusal", resp.Type, resp.Message)
	}

	resp = c.roundTrip(`{"type":"CallTool","name":"mail_search","arguments":{}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("CallTool on the same connection after the bit flipped: %s %s", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// AC-16: a request mid-flight when the bit flips completes (run -race)
// ---------------------------------------------------------------------------

// blockingConfigurer wraps a real RemoteConfigurer and blocks inside
// NarrowForEnrolment until told to proceed, standing in for a slow narrowing
// so the test can flip the bit while one is genuinely in flight.
type blockingConfigurer struct {
	real    RemoteConfigurer
	started chan struct{}
	proceed chan struct{}
}

func (b *blockingConfigurer) DescribeGrant(s *config.Settings, p *config.Project) grantView {
	return b.real.DescribeGrant(s, p)
}

func (b *blockingConfigurer) NarrowForEnrolment(ctx context.Context, projectID string, f remoteNarrowFields, caller bridge.RemoteCaller, surfaces func() McpSurfaces) (config.Project, []string, error) {
	close(b.started)
	<-b.proceed
	return b.real.NarrowForEnrolment(ctx, projectID, f, caller, surfaces)
}

func TestCliAdmin_InFlightRequestCompletesAfterBitFlips(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	setCLIAdmin(t, f.store, "hermes-mail", true)

	bc := &blockingConfigurer{real: f.configurer(), started: make(chan struct{}), proceed: make(chan struct{})}
	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit, bc, f.mgr.AllMcpSurfaces)
	assertNoErr(t, err, "NewRemoteServer")
	f.server = rs
	go rs.Serve()
	t.Cleanup(rs.Close)

	c := f.dial()
	if c == nil {
		t.Fatal("dial failed")
	}

	type rtResult struct {
		resp bridge.BridgeResponse
		err  error
	}
	done := make(chan rtResult, 1)
	go func() {
		if err := c.sendRaw(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`); err != nil {
			done <- rtResult{err: err}
			return
		}
		resp, err := c.readFrame()
		done <- rtResult{resp: resp, err: err}
	}()

	select {
	case <-bc.started:
	case <-time.After(10 * time.Second):
		t.Fatal("NarrowForEnrolment never started")
	}

	// Flip the bit off while the first request is still blocked inside
	// NarrowForEnrolment.
	setCLIAdmin(t, f.store, "hermes-mail", false)
	close(bc.proceed)

	res := <-done
	assertNoErr(t, res.err, "read NarrowGrant response")
	if res.resp.Type != bridge.RespResult {
		t.Fatalf("the in-flight NarrowGrant did not complete: %s %s", res.resp.Type, res.resp.Message)
	}
	proj, _ := config.FindProjectByID(f.store.Get(), f.project.ID)
	if proj == nil || len(proj.AllowedTools["macmcp"]) != 1 || proj.AllowedTools["macmcp"][0] != "mail_search" {
		t.Fatalf("the in-flight narrowing did not land: %+v", proj)
	}

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{}}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "cli-admin") {
		t.Fatalf("the request AFTER the bit flipped = %s %q, want a cli-admin refusal", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// AC-17 / AC-18: the class-transport matrix and the table's exact contents
// ---------------------------------------------------------------------------

func TestBuildRemoteConfigHandlers_DropsExecuteAndProxyClasses(t *testing.T) {
	for _, class := range []control.CapabilityClass{control.ClassExecute, control.ClassProxy} {
		got := buildRemoteConfigHandlers(map[string]remoteConfigEntry{
			"Fake": {class, handleRemoteDescribeGrant},
		})
		if len(got) != 0 {
			t.Errorf("class %q was installed on the remote configuration table: %v", class, got)
		}
	}
	for reqType, entry := range remoteConfigHandlers {
		if !control.ClassReachableOn(entry.class, control.TransportTCP) {
			t.Errorf("%s has class %q, which control.ClassReachableOn refuses on TCP", reqType, entry.class)
		}
	}
}

func TestRemoteConfigHandlers_HoldsExactlyDescribeGrantAndNarrowGrant(t *testing.T) {
	if len(remoteConfigHandlers) != 2 {
		t.Fatalf("remoteConfigHandlers has %d entries: %v", len(remoteConfigHandlers), remoteConfigHandlers)
	}
	for _, want := range []string{bridge.ReqDescribeGrant, bridge.ReqNarrowGrant} {
		if _, ok := remoteConfigHandlers[want]; !ok {
			t.Errorf("remoteConfigHandlers is missing %s", want)
		}
	}
	// The tool table did not grow — the two live structural tests already
	// assert this unedited; this is a second, independent assertion of the
	// same fact so this file does not depend on theirs.
	if len(remoteHandlers) != 2 {
		t.Fatalf("remoteHandlers has %d entries, want 2", len(remoteHandlers))
	}
}

// ---------------------------------------------------------------------------
// AC-19 / AC-20: audit attribution
// ---------------------------------------------------------------------------

func TestCliAdmin_NarrowGrantAttributedToTheEnrolment(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}

	events := readLoggedEvents(t, f.audit)
	var found *AuditEvent
	for i := range events {
		if events[i].Event == AuditEventConfigChange && events[i].Credential == auditCredentialProjectGrant {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no config_change recorded for the NarrowGrant: %+v", events)
	}
	if found.Actor.Kind != AuditActorRemote {
		t.Errorf("actor.kind = %q, want %q", found.Actor.Kind, AuditActorRemote)
	}
	if found.Actor.Auth != AuditAuthMTLS {
		t.Errorf("actor.auth = %q, want %q", found.Actor.Auth, AuditAuthMTLS)
	}
	if found.Actor.ClientID != "hermes-mail" {
		t.Errorf("actor.client_id = %q, want hermes-mail", found.Actor.ClientID)
	}
	if found.Actor.Fingerprint == "" {
		t.Error("actor.fingerprint is empty")
	}
	if found.Via != auditViaRemote {
		t.Errorf("via = %q, want %q", found.Via, auditViaRemote)
	}
	if found.Subject != f.project.ID {
		t.Errorf("subject = %q, want the profile id %q", found.Subject, f.project.ID)
	}
	if len(found.Grants) == 0 {
		t.Error("grants is empty, want the changed field names")
	}
	if found.Actor.PID != 0 {
		t.Errorf("actor.pid = %d, want 0 — this must not be attributed to the tray process", found.Actor.PID)
	}
}

func TestCliAdmin_RefusedConfigRequestWritesControlDecision(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("expected a refusal, got %s", resp.Type)
	}

	events := readLoggedEvents(t, f.audit)
	var found *AuditEvent
	for i := range events {
		if events[i].Event == AuditEventControlDecision && events[i].Class == string(control.ClassConfigure) {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no control_decision recorded for the refused NarrowGrant: %+v", events)
	}
	if found.Outcome != AuditOutcomeDenied {
		t.Errorf("outcome = %q, want %q", found.Outcome, AuditOutcomeDenied)
	}
	if found.Transport != string(control.TransportTCP) {
		t.Errorf("transport = %q, want %q", found.Transport, control.TransportTCP)
	}
	if found.Actor.ClientID != "hermes-mail" {
		t.Errorf("actor.client_id = %q, want hermes-mail", found.Actor.ClientID)
	}
}

// ---------------------------------------------------------------------------
// Finding (class label): DescribeGrant's own class label has no assertion
// anywhere — relabelling remoteConfigHandlers[bridge.ReqDescribeGrant] from
// control.ClassRead to control.ClassConfigure leaves the suite green, because the read-only
// entry's class is never surfaced back for a test to check. NarrowGrant's
// label is already caught, the same way this one now is: a refusal writes
// the entry's class into the control_decision record.
// ---------------------------------------------------------------------------

func TestCliAdmin_RefusedDescribeGrantRecordsItsOwnClass(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	resp := c.roundTrip(`{"type":"DescribeGrant"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("expected a refusal, got %s", resp.Type)
	}

	events := readLoggedEvents(t, f.audit)
	var found *AuditEvent
	for i := range events {
		if events[i].Event == AuditEventControlDecision && events[i].Method == bridge.ReqDescribeGrant {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no control_decision recorded for the refused DescribeGrant: %+v", events)
	}
	if found.Class != string(control.ClassRead) {
		t.Errorf("class = %q, want %q — DescribeGrant is registered as read, not configure", found.Class, control.ClassRead)
	}
}

// ---------------------------------------------------------------------------
// AC-24: no presence prompt is reachable from the remote listener
// ---------------------------------------------------------------------------

func TestCliAdmin_NoPresencePromptReachableFromRemoteListener(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	setCLIAdmin(t, f.store, "hermes-mail", true)

	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")
	configurer := &ProjectOps{
		Store:    f.store,
		Gate:     gate,
		Issuance: issuanceAuditorOrNil(f.audit),
		OnChange: func() {},
	}

	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit, configurer, f.mgr.AllMcpSurfaces)
	assertNoErr(t, err, "NewRemoteServer")
	f.server = rs
	go rs.Serve()
	t.Cleanup(rs.Close)

	c := f.dial()
	if c == nil {
		t.Fatal("dial failed")
	}

	resp := c.roundTrip(`{"type":"DescribeGrant"}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("DescribeGrant refused: %s %s", resp.Type, resp.Message)
	}
	resp = c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}
	resp = c.roundTrip(`{"type":"CallTool","name":"mail_search","arguments":{}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("CallTool refused: %s %s", resp.Type, resp.Message)
	}

	if n := recording.Calls(); n != 0 {
		t.Fatalf("the presence provider was called %d time(s) from the remote listener; want 0", n)
	}
}

// ---------------------------------------------------------------------------
// NarrowForEnrolment reuses applyProjectUpdate rather than writing the
// narrowed fields to the store directly (SPEC-step2-cli-admin.md §4.3).
// narrowsOnly alone cannot observe this: it only judges whether a REQUEST is
// narrower than what is stored, never what the mutator that applies an
// accepted request actually does to the record. A direct field write would
// still satisfy every narrowsOnly rule while skipping SyncProjectToken's own
// pruning of a dropped MCP's now-orphaned allowed_tools/access entries.
// ---------------------------------------------------------------------------

func TestNarrowForEnrolment_DroppingAnMcpPrunesItsStaleGrantEntries(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")

	// Widened behind the guards, the same way TestRemoteServer_
	// RefusesAProjectThatIsNoLongerRemote seeds a stale record: standing in
	// for a grant an operator already validated through PUT
	// /api/projects/{id}, which is the only door that can ever put a
	// project in this shape.
	assertNoErr(t, store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, mail.ID)
		if p == nil {
			t.Fatal("seeded project vanished")
		}
		p.AllowedMcpIDs = []string{"macmcp", "other"}
		p.AllowedTools = map[string][]string{"macmcp": {"mail_search"}, "other": {"other_tool"}}
		p.Access = map[string]string{"macmcp": config.AccessRead, "other": config.AccessRead}
	}), "widen behind the guards")

	ops := &ProjectOps{Store: store, Issuance: pgwWithIssuance(t), OnChange: func() {}}
	surfaces := func() McpSurfaces { return McpSurfaces{"macmcp": macmcpSurface()} }
	caller := bridge.RemoteCaller{ClientID: "hermes-mail", Fingerprint: "sha256:" + strings.Repeat("a", 64)}

	narrowedIDs := []string{"macmcp"}
	_, _, err := ops.NarrowForEnrolment(context.Background(), mail.ID,
		remoteNarrowFields{AllowedMcpIDs: &narrowedIDs}, caller, surfaces)
	assertNoErr(t, err, "NarrowForEnrolment dropping an MCP")

	proj, _ := config.FindProjectByID(store.Get(), mail.ID)
	if proj == nil {
		t.Fatal("the project vanished")
	}
	if _, stale := proj.AllowedTools["other"]; stale {
		t.Error("a dropped MCP's allowed_tools entry survived narrowing: NarrowForEnrolment must reuse applyProjectUpdate (SyncProjectToken prunes it), not write AllowedMcpIDs to the store directly")
	}
	if _, stale := proj.Access["other"]; stale {
		t.Error("a dropped MCP's access entry survived narrowing")
	}
	if got := proj.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_search" {
		t.Errorf("the kept MCP's allowlist changed: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Finding C: NarrowForEnrolment's fail-closed issuance guard has no
// coverage of its own. Every sibling core has this test (AC-6 has it for
// EnrolmentOps.Update) — deleting requireIssuanceAuditor(o.Issuance) from
// NarrowForEnrolment leaves the rest of the suite green, since
// recordConfigChangeRemote returns nil for a nil auditor and a remote
// narrowing would land silently unrecorded.
// ---------------------------------------------------------------------------

func TestNarrowForEnrolment_IssuanceAuditingOffRefusesBeforeTouchingStore(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	assertNoErr(t, store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, mail.ID)
		if p == nil {
			t.Fatal("seeded project vanished")
		}
		p.AllowedMcpIDs = []string{"macmcp"}
		p.AllowedTools = map[string][]string{"macmcp": {"mail_*"}}
	}), "seed allowed_mcp_ids and allowed_tools")

	before := odwSnap(t, dir)
	ops := &ProjectOps{Store: store, Issuance: nil, OnChange: func() {}}
	surfaces := func() McpSurfaces { return McpSurfaces{"macmcp": macmcpSurface()} }
	caller := bridge.RemoteCaller{ClientID: "hermes-mail", Fingerprint: "sha256:" + strings.Repeat("a", 64)}

	_, _, err := ops.NarrowForEnrolment(context.Background(), mail.ID,
		remoteNarrowFields{AllowedTools: &map[string][]string{"macmcp": {"mail_search"}}}, caller, surfaces)
	if !errors.Is(err, errIssuanceAuditingRequired) {
		t.Fatalf("NarrowForEnrolment with a nil Issuance: err = %v, want errIssuanceAuditingRequired", err)
	}
	before.assertUntouched(t, dir, "NarrowForEnrolment with issuance auditing unavailable")
}

// ---------------------------------------------------------------------------
// Finding D: a nil RemoteConfigurer means the configuration table is
// absent — handleRequest must treat every config request type as unknown,
// fail-closed, without panicking the connection goroutine. This also pins
// the ordering inside handleRequest: the cli-admin bit is checked BEFORE
// the nil-configurer check, so a certificate without cli-admin gets the
// cli-admin refusal even on a listener with no configurer wired at all —
// the two refusals are never to be confused with each other.
// ---------------------------------------------------------------------------

func TestCliAdmin_NilConfigurerRefusesConfigRequestsWithoutPanicking(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	setCLIAdmin(t, f.store, "hermes-mail", true)

	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit, nil, f.mgr.AllMcpSurfaces)
	assertNoErr(t, err, "NewRemoteServer with a nil configurer")
	f.server = rs
	go rs.Serve()
	t.Cleanup(rs.Close)

	c := f.dial()
	if c == nil {
		t.Fatal("dial failed")
	}

	resp := c.roundTrip(`{"type":"DescribeGrant"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("DescribeGrant with no configurer wired = %s %s, want a refusal", resp.Type, resp.Message)
	}
	resp = c.roundTrip(`{"type":"NarrowGrant","arguments":{}}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("NarrowGrant with no configurer wired = %s %s, want a refusal", resp.Type, resp.Message)
	}

	// The connection goroutine survived both refusals: an ordinary tool
	// call still works on the same connection.
	resp = c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespTools {
		t.Fatalf("ListTools after a nil-configurer refusal: %s %s", resp.Type, resp.Message)
	}
}

func TestCliAdmin_BitIsCheckedBeforeTheNilConfigurerCheck(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{skipServe: true})
	// cli_admin stays off — the fixture's default.

	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit, nil, f.mgr.AllMcpSurfaces)
	assertNoErr(t, err, "NewRemoteServer with a nil configurer")
	f.server = rs
	go rs.Serve()
	t.Cleanup(rs.Close)

	c := f.dial()
	if c == nil {
		t.Fatal("dial failed")
	}

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{}}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "cli-admin") {
		t.Fatalf("NarrowGrant with the bit off AND no configurer wired = %s %q, want the cli-admin refusal (bit must be checked before the nil configurer)", resp.Type, resp.Message)
	}
}
