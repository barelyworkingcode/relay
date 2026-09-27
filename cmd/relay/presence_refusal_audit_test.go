package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

type prRow struct {
	ev   audit.AuditEvent
	raw  map[string]json.RawMessage
	line string
}

// presenceRefusalRows selects rows by the wire definition of a presence
// refusal: a control_decision with via set and no path. Filtering on method
// instead would also match a route decision.
func presenceRefusalRows(t *testing.T, rec *audit.AuditRecorder) []prRow {
	t.Helper()
	rec.Flush()
	data, err := os.ReadFile(rec.Path())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read audit log: %v", err)
	}
	var out []prRow
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r prRow
		r.line = line
		if err := json.Unmarshal([]byte(line), &r.ev); err != nil {
			t.Fatalf("audit line is not an AuditEvent: %v\n%s", err, line)
		}
		if err := json.Unmarshal([]byte(line), &r.raw); err != nil {
			t.Fatalf("audit line is not a JSON object: %v\n%s", err, line)
		}
		if _, hasPath := r.raw["path"]; r.ev.Event == "control_decision" && r.ev.Via != "" && !hasPath {
			out = append(out, r)
		}
	}
	return out
}

func onlyPresenceRefusalRow(t *testing.T, rec *audit.AuditRecorder) prRow {
	t.Helper()
	rows := presenceRefusalRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 presence-refusal row, got %d: %+v", len(rows), rows)
	}
	return rows[0]
}

func prGate(t *testing.T, p presence.Provider) *presence.Gate {
	t.Helper()
	gate, err := presence.NewGate(p)
	assertNoErr(t, err, "NewGate")
	return gate
}

func prMintArgs(t *testing.T, name string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(credentialMintRequest{Name: name, Classes: []string{"read"}})
	assertNoErr(t, err, "marshal mint args")
	return args
}

func prMint(t *testing.T, store config.SettingsStore, gate *presence.Gate, rec *audit.AuditRecorder, ctx context.Context, name string) error {
	t.Helper()
	ops := &CredentialOps{Store: store, Gate: gate, Issuance: issuanceAuditorOrNil(rec)}
	_, _, err := ops.Mint(ctx, credentialMintRequest{Name: name, Classes: []string{"read"}}, auditViaCLI, "")
	return err
}

func TestPresenceRefusal_CancelledPromptOverBridgeIsAudited(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	recording := presencetest.NewRecording(presence.ErrRefused)
	rec := enabledIssuanceRecorder(t)
	router := &appRouter{store: store, credentialOps: &CredentialOps{Store: store, Gate: prGate(t, recording), Issuance: issuanceAuditorOrNil(rec)}}

	ctx, cancel := context.WithCancel(context.Background())
	bs, err := bridge.NewBridgeServer(ctx, router)
	assertNoErr(t, err, "NewBridgeServer")
	bs.SetCallerSessionResolverForTest(func(net.Conn) presence.CallerSession {
		return presence.CallerSession{GraphicAccess: true}
	})
	go bs.Serve()
	t.Cleanup(func() {
		bs.Close()
		cancel()
	})

	before := time.Now().Truncate(time.Millisecond)
	_, opErr := bridge.NewClient("").AdminOp("credential.mint", prMintArgs(t, "  acme-deploy  "))
	after := time.Now()
	if opErr == nil || !strings.Contains(opErr.Error(), "presence was refused") {
		t.Fatalf("credential.mint error = %v, want the presence refusal", opErr)
	}

	row := onlyPresenceRefusalRow(t, rec)
	ev := row.ev
	if _, err := uuid.Parse(ev.ID); err != nil {
		t.Errorf("id = %q, not a uuid: %v", ev.ID, err)
	}
	if ev.TS.Before(before) || ev.TS.After(after) {
		t.Errorf("ts = %v, want within the call [%v, %v]", ev.TS, before, after)
	}
	if !strings.HasSuffix(strings.Trim(string(row.raw["ts"]), `"`), "Z") {
		t.Errorf("ts = %s, want UTC", row.raw["ts"])
	}
	if ev.DurMs < 0 || ev.DurMs > after.Sub(before).Milliseconds()+1 {
		t.Errorf("dur_ms = %d, want within the call's %v", ev.DurMs, after.Sub(before))
	}
	if ev.Outcome != "denied" || ev.Error != "presence was refused" || ev.Method != "credential.mint" ||
		ev.Subject != "acme-deploy" || ev.Via != "cli" || ev.IssuanceTruncated {
		t.Errorf("row = outcome %q error %q method %q subject %q via %q truncated %v; want denied, presence was refused, credential.mint, acme-deploy, cli, false",
			ev.Outcome, ev.Error, ev.Method, ev.Subject, ev.Via, ev.IssuanceTruncated)
	}
	if string(row.raw["scope"]) != "null" {
		t.Errorf("scope = %s, want null", row.raw["scope"])
	}
	proc, parent := audit.ProcessNames(os.Getpid())
	want := audit.AuditActor{Kind: "operator", Auth: "none", PID: os.Getpid(), Proc: proc, Parent: parent}
	if ev.Actor != want {
		t.Errorf("actor = %+v, want %+v", ev.Actor, want)
	}
	for _, key := range []string{"path", "class", "transport", "presence_id", "credential", "subject_name", "grants", "args"} {
		if _, ok := row.raw[key]; ok {
			t.Errorf("presence-refusal row carries %q: %s", key, row.line)
		}
	}
	if reasons := recording.Reasons(); len(reasons) != 1 || strings.Contains(row.line, reasons[0]) {
		t.Errorf("prompt reasons %q; the row must not carry the reason text: %s", reasons, row.line)
	}
	for _, e := range readLoggedEvents(t, rec) {
		if e.Event == audit.AuditEventCredentialIssued {
			t.Errorf("credential_issued row written for a refused mint: %+v", e)
		}
	}
}

func prSessionFixture(t *testing.T, p presence.Provider) (*sessionFixture, *audit.AuditRecorder) {
	t.Helper()
	fx := newSessionFixture(t, makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil), mcpbroker.NewManager(nil))
	rec := enabledIssuanceRecorder(t)
	fx.r.credentialOps = &CredentialOps{Store: fx.r.store, Gate: prGate(t, p), Issuance: issuanceAuditorOrNil(rec)}
	return fx, rec
}

func prMemberCtx(fx *sessionFixture, pid int) context.Context {
	ctx := bridge.WithCallerPID(fx.conn(pid), pid)
	return presence.WithCallerSession(ctx, presence.CallerSession{GraphicAccess: true})
}

func assertSessionActor(t *testing.T, a audit.AuditActor, sessionID string, pid int) {
	t.Helper()
	if a.Kind != "project_session" || a.Auth != "session" || a.SessionID != sessionID ||
		a.ProjectID != "test-project" || a.ProjectName != "" || a.PID != pid {
		t.Errorf("actor = %+v, want project_session/session, session %q, project test-project (no name), pid %d", a, sessionID, pid)
	}
}

func TestPresenceRefusal_SessionMemberIsNamed(t *testing.T) {
	fx, rec := prSessionFixture(t, presencetest.Deny())
	root, _ := fx.startSession(t, "sess-p1", "test-project")
	child := fx.spawn(root, 20)

	_, err := fx.r.AdminOp(prMemberCtx(fx, child), "credential.mint", prMintArgs(t, "probe"))
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("err = %v, want presence.ErrRefused", err)
	}
	assertSessionActor(t, onlyPresenceRefusalRow(t, rec).ev.Actor, "sess-p1", child)
}

func TestPresenceRefusal_CallerThatLeftMidPromptIsStillNamed(t *testing.T) {
	prompt := presencetest.NewBlocking()
	fx, rec := prSessionFixture(t, prompt)
	t.Cleanup(func() { prompt.Release(presence.ErrRefused) })
	root, _ := fx.startSession(t, "sess-p2", "test-project")
	child := fx.spawn(root, 20)

	ctx, cancel := context.WithCancel(prMemberCtx(fx, child))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fx.r.AdminOp(ctx, "credential.mint", prMintArgs(t, "probe"))
		done <- err
	}()
	select {
	case <-prompt.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("credential.mint never reached the prompt")
	}

	fx.rootExits(root)
	cancel()
	prompt.Release(nil)
	var opErr error
	select {
	case opErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("credential.mint did not return after the prompt was answered")
	}
	if !errors.Is(opErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", opErr)
	}

	ev := onlyPresenceRefusalRow(t, rec).ev
	if ev.Error != "context canceled" {
		t.Errorf("error = %q, want %q", ev.Error, "context canceled")
	}
	assertSessionActor(t, ev.Actor, "sess-p2", child)
}

func TestPresenceRefusal_EveryGatedOpIsAudited(t *testing.T) {
	fixed := func(s string) func(config.SettingsStore) string {
		return func(config.SettingsStore) string { return s }
	}
	want := map[string]struct {
		subject func(config.SettingsStore) string
		via     string
	}{
		"credential.mint": {fixed("eve-view"), "cli"},
		"credential.revoke": {func(s config.SettingsStore) string {
			creds := s.Get().APICredentials
			return creds[len(creds)-1].ID
		}, "cli"},
		"enrolment.create":     {fixed("pgw-client"), "cli"},
		"enrolment.update":     {fixed("pgw-update"), "cli"},
		"enrolment.revoke":     {fixed("pgw-revoke"), "cli"},
		"enrolment.sign":       {fixed("pgw-sign-client"), "cli"},
		"login.bootstrap.mint": {fixed(""), "cli"},
		"login.passkey.revoke": {fixed("pgw-passkey"), "ipc"},
		"mcp.register":         {fixed("pgw-mcp"), "cli"},
		"mcp.oauth.start":      {fixed("pgw-oauth"), "cli"},
		"service.register":     {fixed("pgw-svc"), "cli"},
		"project.rotate_token": {func(s config.SettingsStore) string { return s.Get().Projects[0].ID }, "cli"},
		"project.grant":        {fixed("pgw-grant"), "cli"},
		"remote.configure":     {fixed("remote"), "cli"},
		"eve.enrolment.open":   {fixed(""), "cli"},
		"eve.passkey.revoke":   {fixed("pgw-eve-passkey"), "cli"},
	}
	for _, tc := range pgwCases(t) {
		t.Run(tc.op, func(t *testing.T) {
			w, ok := want[tc.op]
			if !ok {
				t.Fatalf("no expected subject/via for gated op %q", tc.op)
			}
			_, store := pgwSandbox(t)
			tc.seed(t, store)
			wantSubject := w.subject(store)
			rec := enabledIssuanceRecorder(t)

			if err := tc.run(t, store, prGate(t, presencetest.Deny()), issuanceAuditorOrNil(rec)); !errors.Is(err, presence.ErrRefused) {
				t.Fatalf("err = %v, want presence.ErrRefused", err)
			}
			ev := onlyPresenceRefusalRow(t, rec).ev
			if ev.Method != tc.op || ev.Subject != wantSubject || ev.Via != w.via || ev.Outcome != "denied" {
				t.Errorf("row = method %q subject %q via %q outcome %q; want %q %q %q denied",
					ev.Method, ev.Subject, ev.Via, ev.Outcome, tc.op, wantSubject, w.via)
			}
		})
	}
}

func TestPresenceRefusal_EveryRefusalCauseIsAudited(t *testing.T) {
	for _, tc := range []struct {
		name      string
		provider  presence.Provider // nil wires the core with no gate
		noGraphic bool
		wantErr   error
		wantText  string
	}{
		{"owner cancels", presencetest.Deny(), false, presence.ErrRefused, "presence was refused"},
		{"caller cannot show a prompt", presencetest.NewRecording(nil), true, presence.ErrNoSession, "no session can display a presence prompt"},
		{"presence unavailable", presencetest.NoSession(), false, presence.ErrUnavailable, "presence checking is unavailable"},
		{"no gate wired", nil, false, errPresenceGateNotWired, "presence gate is not wired for this operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := pgwSandbox(t)
			rec := enabledIssuanceRecorder(t)
			var gate *presence.Gate
			if tc.provider != nil {
				gate = prGate(t, tc.provider)
			}
			ctx := context.Background()
			if tc.noGraphic {
				ctx = presence.WithCallerSession(ctx, presence.CallerSession{GraphicAccess: false})
			}

			if err := prMint(t, store, gate, rec, ctx, "probe"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if r, ok := tc.provider.(*presencetest.Recording); ok && r.Calls() != 0 {
				t.Errorf("provider called %d time(s); want 0", r.Calls())
			}
			if ev := onlyPresenceRefusalRow(t, rec).ev; ev.Error != tc.wantText {
				t.Errorf("error = %q, want %q", ev.Error, tc.wantText)
			}
		})
	}
}

func TestPresenceRefusal_HTTPDoorNamesTheCredential(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	cred, bearer := acsMintCredential(t, store, control.ClassGrant)
	rec := acsNewAuditRecorder(t)

	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket, Authz: NewCredentialAuthorizer(store), Auditor: rec}
	RegisterEnrolmentRoutes(rr, &EnrolmentOps{Store: store, Gate: prGate(t, presencetest.Deny()), Issuance: issuanceAuditorOrNil(rec)})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, err := json.Marshal(enrolmentFields{ClientID: "acme-client", ProjectIDs: []string{profile.ID}})
	assertNoErr(t, err, "marshal body")
	req, err := http.NewRequest("POST", srv.URL+"/api/enrolments", bytes.NewReader(body))
	assertNoErr(t, err, "NewRequest")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "POST /api/enrolments")
	resp.Body.Close()

	var decisions []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Event == audit.AuditEventControlDecision {
			decisions = append(decisions, ev)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("want 2 control_decision rows (route, then presence), got %d: %+v", len(decisions), decisions)
	}
	if route := decisions[0]; route.Path != "/api/enrolments" || route.Outcome != "ok" {
		t.Errorf("first row = path %q outcome %q, want the route decision ok", route.Path, route.Outcome)
	}
	p := decisions[1]
	want := audit.AuditActor{Kind: "control", Auth: "token", CredID: cred.ID}
	if p.Path != "" || p.Outcome != "denied" || p.Via != "http" || p.Method != "enrolment.create" || p.Subject != "acme-client" || p.Actor != want {
		t.Errorf("second row = %+v; want the presence refusal via http naming credential %s", p, cred.ID)
	}
}

func TestPresenceRefusal_BrokenSinkStillRefuses(t *testing.T) {
	dir, store := pgwSandbox(t)
	before := odwSnap(t, dir)
	rec := enabledIssuanceRecorder(t)
	assertNoErr(t, rec.CloseWriterForTest(), "CloseWriterForTest")

	if err := prMint(t, store, prGate(t, presencetest.Deny()), rec, context.Background(), "probe"); !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("err = %v, want presence.ErrRefused", err)
	}
	before.assertUntouched(t, dir, "credential.mint refused with a broken audit sink")
}

func TestPresenceRefusal_AuditingOffStillRefuses(t *testing.T) {
	_, store := pgwSandbox(t)
	ops := &EnrolmentOps{Store: store, Gate: prGate(t, presencetest.Deny())}
	_, err := ops.SetRemoteConfig(context.Background(), remoteConfigFields{Enabled: true, Listen: "127.0.0.1:9910"}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("err = %v, want presence.ErrRefused", err)
	}
}

func TestPresenceRefusal_GrantedPromptWritesNoRefusalRow(t *testing.T) {
	_, store := pgwSandbox(t)
	rec := enabledIssuanceRecorder(t)
	if err := prMint(t, store, prGate(t, presencetest.Allow()), rec, context.Background(), "probe"); err != nil {
		t.Fatalf("mint with an allowing gate: %v", err)
	}
	if rows := presenceRefusalRows(t, rec); len(rows) != 0 {
		t.Errorf("granted prompt wrote %d presence-refusal row(s): %+v", len(rows), rows)
	}
	var issued []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Event == audit.AuditEventCredentialIssued {
			issued = append(issued, ev)
		}
	}
	if len(issued) != 1 || issued[0].PresenceID == "" {
		t.Errorf("credential_issued rows = %+v, want one carrying a presence_id", issued)
	}
}

func TestPresenceRefusal_SubjectIsCapped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		subject       string
		wantTruncated bool
	}{
		{"exactly 256 bytes", "a" + strings.Repeat("é", 127) + "a", false},
		{"over 256 bytes, cut inside a rune", "a" + strings.Repeat("é", 200), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := pgwSandbox(t)
			rec := enabledIssuanceRecorder(t)
			if err := prMint(t, store, prGate(t, presencetest.Deny()), rec, context.Background(), tc.subject); !errors.Is(err, presence.ErrRefused) {
				t.Fatalf("err = %v, want presence.ErrRefused", err)
			}
			ev := onlyPresenceRefusalRow(t, rec).ev
			if len(ev.Subject) > 256 || !utf8.ValidString(ev.Subject) || !strings.HasPrefix(tc.subject, ev.Subject) {
				t.Errorf("subject (%d bytes) = %q; want a valid-UTF-8 prefix of at most 256 bytes", len(ev.Subject), ev.Subject)
			}
			if ev.IssuanceTruncated != tc.wantTruncated {
				t.Errorf("issuance_truncated = %v, want %v", ev.IssuanceTruncated, tc.wantTruncated)
			}
			if !tc.wantTruncated && ev.Subject != tc.subject {
				t.Errorf("subject = %q, want it whole", ev.Subject)
			}
		})
	}
}

func TestAuditDetail_PresenceRefusalRow(t *testing.T) {
	row := func(method, subject string, truncated bool) audit.AuditEvent {
		return audit.AuditEvent{Event: audit.AuditEventControlDecision, Outcome: "denied", Error: "presence was refused",
			Method: method, Subject: subject, Via: "cli", IssuanceTruncated: truncated}
	}
	for _, tc := range []struct {
		name string
		ev   audit.AuditEvent
		want string
	}{
		{"with subject", row("credential.mint", "ci-deploy", false), "credential.mint  ci-deploy  via=cli  presence was refused"},
		{"without subject", row("login.bootstrap.mint", "", false), "login.bootstrap.mint  via=cli  presence was refused"},
		{"truncated subject", row("credential.mint", "ci-deploy", true), "credential.mint  ci-deploy  via=cli  presence was refused  (truncated)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditDetail(tc.ev); got != tc.want {
				t.Errorf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}
