package main

// Fail-closed, two-phase auditing for remote callers (ADR-010 decisions 5 and
// 6), and the local behaviour that must be provably unchanged by it.
//
// The tests that matter here are about *ordering*, not about content: a record
// written after the MCP has run cannot make "refuse a call that cannot be
// logged" mean anything, so the interesting assertions are made from inside the
// MCP handler and from the fact that the handler never ran at all.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"relaygo/bridge"
)

// A full, untruncated fingerprint. Recorded whole so a revoked device's history
// still says which key made the call.
const testFingerprint = "sha256:9f2a41c78b0355ee1d6a4c2f8e0b7a93d5c14e6f8a2b0d9c3e7f15a8b46c2d90"

// remoteCtx returns a context carrying the identity the remote listener
// attests from the TLS connection. Nothing here is caller-asserted; the test
// stands in for the listener, which another change owns.
func remoteCtx(clientID string) context.Context {
	return bridge.WithRemoteCaller(context.Background(), bridge.RemoteCaller{
		ClientID:    clientID,
		Fingerprint: testFingerprint,
		RemoteAddr:  "127.0.0.1:52233",
	})
}

// readLogFile parses the audit log without flushing. Used from inside an MCP
// handler, where the point is what is *already* durably on disk at the moment
// the tool is invoked — flushing first would destroy the thing being measured.
func readLogFile(t *testing.T, rec *AuditRecorder) []AuditEvent {
	t.Helper()
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var out []AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit log line is not valid JSON: %v\nline: %s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

// ---------------------------------------------------------------------------
// Ordering: the intent record exists before the MCP is invoked
// ---------------------------------------------------------------------------

func TestAuditRemote_IntentIsOnDiskBeforeTheMcpRuns(t *testing.T) {
	mkSandboxRelayHome(t)

	// The recorder has to exist before the mock, because the mock reads its log.
	var rec *AuditRecorder
	var seen []AuditEvent
	mock := newMockConn("macmcp", simpleTools("mail_search"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			// Read the log from inside the tool call: whatever is here now was
			// written before the MCP was reached.
			seen = readLogFile(t, rec)
			return json.RawMessage(`{"content":[{"type":"text","text":"3 messages"}]}`), nil
		})

	r := setupRouter(t, map[string]Permission{"macmcp": PermOn}, nil, nil,
		map[string]*mockMcpConn{"macmcp": mock})
	rec = newTestAudit(t, nil)
	r.audit = rec

	ctx := remoteCtx("hermes-mail")
	if _, err := r.CallTool(ctx, "mail_search", json.RawMessage(`{"q":"invoice"}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("log held %d records when the MCP ran, want exactly the intent: %+v", len(seen), seen)
	}
	ev := seen[0]
	if ev.Phase != AuditPhaseIntent {
		t.Errorf("phase = %q, want %q", ev.Phase, AuditPhaseIntent)
	}
	if ev.Outcome != AuditOutcomePending {
		t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomePending)
	}
	if ev.Tool != "mail_search" || ev.Actor.ClientID != "hermes-mail" {
		t.Errorf("intent record is missing attribution: tool=%q client=%q", ev.Tool, ev.Actor.ClientID)
	}
	// Actor, tool and redacted arguments, per decision 5.
	if string(ev.Args) != `{"q":"invoice"}` {
		t.Errorf("intent args = %s, want the redacted call arguments", ev.Args)
	}
}

// The refusal is the entire trade decision 5 makes: availability for evidence.
// If the intent cannot be written the MCP must never be reached — asserting the
// call returned an error is not enough, because a call that failed *after* the
// mailbox was read is exactly the failure this design rejects.
func TestAuditRemote_FailedIntentRefusesTheCallAndTheMcpNeverRuns(t *testing.T) {
	mkSandboxRelayHome(t)

	called := false
	mock := newMockConn("macmcp", simpleTools("mail_search"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			called = true
			return json.RawMessage(`{"content":[]}`), nil
		})
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	// Break the sink the way a full or unwritable disk would: the file handle
	// goes away underneath the writer, so the encode fails for real rather than
	// through a test-only switch in production code.
	if err := rec.w.Close(); err != nil {
		t.Fatalf("close audit sink: %v", err)
	}

	_, err := r.CallTool(remoteCtx("hermes-mail"), "mail_search", json.RawMessage(`{}`), testToken)
	if err == nil {
		t.Fatal("remote call succeeded despite an unwritable audit log")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("error = %v, want it to name auditing as the reason", err)
	}
	if called {
		t.Error("the MCP was invoked for a call whose intent record could not be written")
	}
}

// A local call keeps ADR-008's fail-open path exactly: the sink can be entirely
// broken and the tool call still succeeds.
func TestAuditLocal_UnwritableSinkStillCompletesTheCall(t *testing.T) {
	mkSandboxRelayHome(t)

	called := false
	mock := newMockConn("fsmcp", simpleTools("read_file"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			called = true
			return json.RawMessage(`{"content":[]}`), nil
		})
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if err := rec.w.Close(); err != nil {
		t.Fatalf("close audit sink: %v", err)
	}

	if _, err := r.CallTool(context.Background(), "read_file", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a broken audit sink must not fail a local tool call: %v", err)
	}
	if !called {
		t.Error("the MCP was not invoked for a local call")
	}
}

// ---------------------------------------------------------------------------
// Correlation
// ---------------------------------------------------------------------------

func TestAuditRemote_IntentAndCompletionShareOneEventID(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("macmcp", simpleTools("mail_search"),
		okHandler(`{"content":[{"type":"text","text":"3 messages"}]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	if _, err := r.CallTool(remoteCtx("hermes-mail"), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("a remote call wrote %d records, want intent + completion: %+v", len(events), events)
	}
	// readLoggedEvents preserves file order: intent first, completion second.
	intent, completion := events[0], events[1]
	if intent.Phase != AuditPhaseIntent || completion.Phase != AuditPhaseCompletion {
		t.Fatalf("phases = %q then %q, want intent then completion", intent.Phase, completion.Phase)
	}
	if intent.ID == "" || intent.ID != completion.ID {
		t.Errorf("ids = %q and %q, want one shared id", intent.ID, completion.ID)
	}
	if completion.Outcome != AuditOutcomeOK {
		t.Errorf("completion outcome = %q, want ok (error=%q)", completion.Outcome, completion.Error)
	}
	if completion.ResultBytes == 0 {
		t.Error("completion recorded no result size")
	}
	if intent.ResultBytes != 0 {
		t.Error("intent recorded a result size, which it cannot possibly know")
	}
}

// A refusal that happens before the MCP could be reached is a single record:
// there is no side effect to have written a promise about.
func TestAuditRemote_DeniedCallIsOneRecordWithNoIntent(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("macmcp", simpleTools("mail_search", "send_mail"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn},
		map[string][]string{"macmcp": {"send_mail"}},
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	if _, err := r.CallTool(remoteCtx("hermes-mail"), "send_mail", nil, testToken); err == nil {
		t.Fatal("disabled tool was not denied")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != AuditOutcomeDenied {
		t.Errorf("outcome = %q, want denied", ev.Outcome)
	}
	if ev.Phase != "" {
		t.Errorf("phase = %q, want empty for a call that never reached an MCP", ev.Phase)
	}
	// The refusal is still attributable to the certificate that made it.
	if ev.Actor.Kind != AuditActorRemote || ev.Actor.ClientID != "hermes-mail" {
		t.Errorf("denied remote call lost its attestation: %+v", ev.Actor)
	}
}

// ---------------------------------------------------------------------------
// Remote actor identity (decision 6)
// ---------------------------------------------------------------------------

func TestAuditRemote_ActorIsAttestedAndProcessFieldsAreAbsent(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("macmcp", simpleTools("mail_search"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	// A peer pid in the context as well: even if one somehow rides along, a
	// remote record must not be attributed to a local process.
	ctx := bridge.WithCallerPID(remoteCtx("hermes-mail"), os.Getpid())
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("want intent + completion, got %d: %+v", len(events), events)
	}
	for _, ev := range events {
		a := ev.Actor
		if a.Kind != AuditActorRemote {
			t.Errorf("actor kind = %q, want %q", a.Kind, AuditActorRemote)
		}
		if a.Auth != AuditAuthMTLS {
			t.Errorf("actor auth = %q, want %q", a.Auth, AuditAuthMTLS)
		}
		if a.ClientID != "hermes-mail" {
			t.Errorf("client_id = %q, want hermes-mail", a.ClientID)
		}
		if a.Fingerprint != testFingerprint {
			t.Errorf("fingerprint = %q, want the full value %q", a.Fingerprint, testFingerprint)
		}
		if a.RemoteAddr != "127.0.0.1:52233" {
			t.Errorf("remote_addr = %q", a.RemoteAddr)
		}
		// Remote *and* acting as a project grant: both facts matter.
		if a.ProjectID != "test-project" || a.ProjectName != "test" {
			t.Errorf("project attribution = %q/%q, want test-project/test", a.ProjectID, a.ProjectName)
		}
		if a.PID != 0 || a.Proc != "" || a.Parent != "" {
			t.Errorf("local process fields set on a remote actor: %+v", a)
		}
	}

	// Omitted, not zero-filled: an absent field reads as "not applicable", a
	// present-but-zero one reads as "unknown", and those are different claims.
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var raw struct {
			Actor map[string]json.RawMessage `json:"actor"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("bad log line: %v", err)
		}
		for _, key := range []string{"pid", "proc", "parent"} {
			if _, ok := raw.Actor[key]; ok {
				t.Errorf("actor.%s present on a remote record: %s", key, line)
			}
		}
	}
}

// An unresolved grant on a remote connection is still a known certificate.
func TestAuditRemote_UnauthorizedKeepsTheAttestedIdentity(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("macmcp", simpleTools("mail_search"), okHandler(`{}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	if _, err := r.CallTool(remoteCtx("hermes-mail"), "mail_search", nil, "not-a-real-token"); err == nil {
		t.Fatal("a bogus credential was accepted")
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != AuditOutcomeUnauthorized {
		t.Errorf("outcome = %q, want unauthorized", ev.Outcome)
	}
	if ev.Actor.Kind != AuditActorRemote || ev.Actor.Fingerprint != testFingerprint {
		t.Errorf("unauthorized remote call lost its attestation: %+v", ev.Actor)
	}
}

// ---------------------------------------------------------------------------
// Local behaviour is untouched (decision 5, last paragraph)
// ---------------------------------------------------------------------------

func TestAuditLocal_StillWritesExactlyOneRecordWithNoPhase(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	ctx := bridge.WithCallerPID(context.Background(), os.Getpid())
	if _, err := r.CallTool(ctx, "read_file", json.RawMessage(`{"path":"/tmp/x"}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Phase != "" {
		t.Errorf("phase = %q, want absent: a local call is one record, exactly as before", ev.Phase)
	}
	if ev.Actor.Kind != AuditActorProject || ev.Actor.Auth != AuditAuthToken {
		t.Errorf("actor = %q/%q, want project/token", ev.Actor.Kind, ev.Actor.Auth)
	}
	if ev.Actor.ClientID != "" || ev.Actor.Fingerprint != "" || ev.Actor.RemoteAddr != "" {
		t.Errorf("remote fields leaked onto a local record: %+v", ev.Actor)
	}
	if ev.Actor.PID != os.Getpid() {
		t.Errorf("actor pid = %d, want %d", ev.Actor.PID, os.Getpid())
	}
}

// The bounded channel, the drop-and-count, and the refusal to stall a tool call
// are unchanged for local callers: with the writer wedged and the queue full, a
// local call still completes rather than waiting for the sink.
func TestAuditLocal_DropsRatherThanBlockingWhenTheQueueIsFull(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("fsmcp", simpleTools("read_file"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"fsmcp": PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	// Wedge the writer goroutine, then fill the queue behind it.
	var wg sync.WaitGroup
	wg.Add(1)
	blocked := make(chan struct{})
	rec.SetSink(func(AuditEvent) {
		close(blocked)
		wg.Wait()
	})
	rec.Record(AuditEvent{ID: "wedge"})
	<-blocked
	for i := 0; i < auditQueueSize+50; i++ {
		rec.Record(AuditEvent{ID: "flood"})
	}
	defer func() {
		rec.SetSink(nil)
		wg.Done()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := r.CallTool(context.Background(), "read_file", nil, testToken)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("local CallTool failed with the audit queue full: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local CallTool blocked on the audit sink; it must drop and continue")
	}
	if rec.Dropped() == 0 {
		t.Error("a full queue did not drop any events")
	}
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

func TestAuditQuery_KindAndThrottledFilters(t *testing.T) {
	rec := newTestAudit(t, nil)
	rec.Record(AuditEvent{ID: "local", Event: AuditEventCallTool, Tool: "read_file", Outcome: AuditOutcomeOK,
		Actor: AuditActor{Kind: AuditActorProject, ProjectID: "p1"}})
	rec.Record(AuditEvent{ID: "remote-ok", Event: AuditEventCallTool, Tool: "mail_search", Outcome: AuditOutcomeOK,
		Phase: AuditPhaseCompletion,
		Actor: AuditActor{Kind: AuditActorRemote, ProjectID: "p1", ClientID: "hermes-mail"}})
	rec.Record(AuditEvent{ID: "remote-throttled", Event: AuditEventCallTool, Tool: "mail_get_emails",
		Outcome: AuditOutcomeThrottled,
		Actor:   AuditActor{Kind: AuditActorRemote, ProjectID: "p1", ClientID: "hermes-mail"}})
	rec.Flush()

	got := rec.Query(AuditQuery{Kind: AuditActorRemote})
	if len(got) != 2 {
		t.Fatalf("kind=remote returned %d events, want 2: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.Actor.Kind != AuditActorRemote {
			t.Errorf("kind filter let a %q actor through", ev.Actor.Kind)
		}
	}

	if got := rec.Query(AuditQuery{Outcome: AuditOutcomeThrottled}); len(got) != 1 || got[0].ID != "remote-throttled" {
		t.Errorf("outcome=throttled returned %+v", got)
	}
	// Combined, the way an operator asks "what did that VM get cut off for".
	if got := rec.Query(AuditQuery{Kind: AuditActorRemote, Outcome: AuditOutcomeThrottled}); len(got) != 1 {
		t.Errorf("kind+outcome returned %+v", got)
	}
	if got := rec.Query(AuditQuery{Kind: AuditActorProject}); len(got) != 1 || got[0].ID != "local" {
		t.Errorf("kind=project returned %+v", got)
	}
}

// `relay audit --kind remote` filters the file, not the ring, so the same
// predicate has to hold over parsed JSONL.
func TestAuditCmd_KindFilterMatchesLoggedRecords(t *testing.T) {
	mkSandboxRelayHome(t)

	mock := newMockConn("macmcp", simpleTools("mail_search"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]Permission{"macmcp": PermOn}, nil,
		map[string]*mockMcpConn{"macmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "mail_search", nil, testToken); err != nil {
		t.Fatalf("local CallTool: %v", err)
	}
	if _, err := r.CallTool(remoteCtx("hermes-mail"), "mail_search", nil, testToken); err != nil {
		t.Fatalf("remote CallTool: %v", err)
	}

	events := readLoggedEvents(t, rec)
	q := AuditQuery{Kind: AuditActorRemote}
	matched := 0
	for i := range events {
		if q.matches(&events[i]) {
			matched++
		}
	}
	// One local record, two remote ones.
	if len(events) != 3 {
		t.Fatalf("log holds %d records, want 3: %+v", len(events), events)
	}
	if matched != 2 {
		t.Errorf("--kind remote matched %d of %d records, want 2", matched, len(events))
	}
}

// ---------------------------------------------------------------------------
// The context carrier
// ---------------------------------------------------------------------------

// Remote identity is admitted only when the certificate attested it. An
// identity with no fingerprint is not a remote caller, and must not be able to
// switch a call onto the fail-closed path.
func TestRemoteCallerContext_RequiresAFingerprint(t *testing.T) {
	if _, ok := bridge.RemoteCallerFromContext(context.Background()); ok {
		t.Error("a bare context reported a remote caller")
	}
	ctx := bridge.WithRemoteCaller(context.Background(), bridge.RemoteCaller{ClientID: "hermes-mail"})
	if _, ok := bridge.RemoteCallerFromContext(ctx); ok {
		t.Error("an identity with no certificate fingerprint was admitted as remote")
	}
	ctx = bridge.WithRemoteCaller(context.Background(), bridge.RemoteCaller{
		ClientID: "hermes-mail", Fingerprint: testFingerprint, RemoteAddr: "127.0.0.1:1",
	})
	got, ok := bridge.RemoteCallerFromContext(ctx)
	if !ok || got.ClientID != "hermes-mail" || got.Fingerprint != testFingerprint {
		t.Errorf("round trip lost the identity: %+v ok=%v", got, ok)
	}
}
