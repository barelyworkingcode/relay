package main

// The remote listener's configuration plane (ADR-018 decision 4): what a
// cli-admin enrolment may reach over mTLS, and — just as important — what it
// structurally cannot. Acceptance criteria references are the implementing
// spec's own numbering (§8).

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"relaygo/bridge"
	"relaygo/mcp"
	"relaygo/presence"
	"relaygo/presence/presencetest"
)

// setCLIAdmin flips an enrolment's bit directly through updateEnrolment
// (bypassing EnrolmentOps' own gate — the toggle itself is covered
// elsewhere), so these tests can drive the bit deterministically.
func setCLIAdmin(t *testing.T, store SettingsStore, clientID string, on bool) {
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
	f := newRemoteFixture(t, remoteFixtureOpts{})
	setCLIAdmin(t, f.store, "hermes-mail", true)
	c := f.dial()

	resp := c.roundTrip(`{"type":"NarrowGrant","arguments":{"allowed_tools":{"macmcp":["mail_search"]}}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("NarrowGrant refused: %s %s", resp.Type, resp.Message)
	}

	proj, _ := f.store.Get().findProjectByID(f.project.ID)
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
		t.Fatalf("tool list after narrowing = %+v", tools)
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

	var calProj Project
	var createErr error
	assertNoErr(t, f.store.With(func(s *Settings) {
		calProj, createErr = s.CreateProjectWithTokenKind(ProjectKindRemote, "Calendar", "", []string{"macmcp"}, []string{}, nil, nil)
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

func TestRemoteNarrowFields_NamesNoOtherIdentity(t *testing.T) {
	banned := map[string]bool{"client_id": true, "enrolment": true, "project_id": true, "owner": true}
	typ := reflect.TypeOf(remoteNarrowFields{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if banned[tag] {
			t.Errorf("remoteNarrowFields has a field tagged %q, which names an object other than the caller's own", tag)
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
	proj := &Project{Kind: ProjectKindRemote, AllowCwdAuth: true}
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

	// Out of band relative to this connection: another process (a CLI
	// invocation) toggles the bit while the connection stays open.
	setCLIAdmin(t, f.store, "hermes-mail", false)

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

func (b *blockingConfigurer) DescribeGrant(s *Settings, p *Project) grantView {
	return b.real.DescribeGrant(s, p)
}

func (b *blockingConfigurer) NarrowForEnrolment(ctx context.Context, projectID string, f remoteNarrowFields, caller bridge.RemoteCaller, surfaces func() McpSurfaces) (Project, []string, error) {
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
	proj, _ := f.store.Get().findProjectByID(f.project.ID)
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
	for _, class := range []CapabilityClass{ClassExecute, ClassProxy} {
		got := buildRemoteConfigHandlers(map[string]remoteConfigEntry{
			"Fake": {class, handleRemoteDescribeGrant},
		})
		if len(got) != 0 {
			t.Errorf("class %q was installed on the remote configuration table: %v", class, got)
		}
	}
	for reqType, entry := range remoteConfigHandlers {
		if !ClassReachableOn(entry.class, TransportTCP) {
			t.Errorf("%s has class %q, which ClassReachableOn refuses on TCP", reqType, entry.class)
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
		if events[i].Event == AuditEventControlDecision && events[i].Class == string(ClassConfigure) {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no control_decision recorded for the refused NarrowGrant: %+v", events)
	}
	if found.Outcome != AuditOutcomeDenied {
		t.Errorf("outcome = %q, want %q", found.Outcome, AuditOutcomeDenied)
	}
	if found.Transport != string(TransportTCP) {
		t.Errorf("transport = %q, want %q", found.Transport, TransportTCP)
	}
	if found.Actor.ClientID != "hermes-mail" {
		t.Errorf("actor.client_id = %q, want hermes-mail", found.Actor.ClientID)
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
