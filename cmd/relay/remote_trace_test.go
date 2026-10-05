package main

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/project"
)

const remoteTraceCanary = "canary-remote-arg-5d21"

func remoteRequestLines(t *testing.T, buf *lrSyncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range traceLines(t, buf) {
		if m["op"] == "remote.request" {
			out = append(out, m)
		}
	}
	return out
}

func remoteCallAuditByPhase(t *testing.T, f *remoteFixture) (intent, completion audit.AuditEvent) {
	t.Helper()
	var n int
	for _, ev := range readLoggedEvents(t, f.audit) {
		switch ev.Phase {
		case audit.AuditPhaseIntent:
			intent = ev
			n++
		case audit.AuditPhaseCompletion:
			completion = ev
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want one intent and one completion record, got %d phase records", n)
	}
	return intent, completion
}

func TestRemoteTrace_OneCallSharesOneTraceAcrossAuditAndLogAndEachRequestGetsANewOne(t *testing.T) {
	buf := captureTraceLogs(t)
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	resp := c.roundTrip(`{"type":"CallTool","name":"mail_search","arguments":{"q":"` + remoteTraceCanary + `"}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("CallTool returned %s: %s", resp.Type, resp.Message)
	}
	intent, completion := remoteCallAuditByPhase(t, f)
	if !traceHexID.MatchString(intent.TraceID) || intent.TraceID != completion.TraceID {
		t.Fatalf("intent trace %q, completion trace %q; want one shared 32-hex ID", intent.TraceID, completion.TraceID)
	}

	lines := remoteRequestLines(t, buf)
	if len(lines) != 1 || lines[0]["trace_id"] != intent.TraceID {
		t.Fatalf("remote.request lines = %v, want one with trace_id %s", lines, intent.TraceID)
	}
	if err := checkNineKeys(lines[0]); err != nil {
		t.Errorf("remote.request line breaks the schema: %v", err)
	}
	if lines[0]["status"] != "ok" {
		t.Errorf("status = %v, want ok", lines[0]["status"])
	}

	// A second request on the same connection is a new action.
	c.roundTrip(`{"type":"ListTools"}`)
	lines = remoteRequestLines(t, buf)
	if len(lines) != 2 || lines[1]["trace_id"] == lines[0]["trace_id"] || lines[1]["trace_id"] == "" {
		t.Fatalf("second request trace = %v, want a different non-empty ID", lines)
	}

	if strings.Contains(buf.String(), remoteTraceCanary) {
		t.Fatalf("tool arguments reached the log:\n%s", buf.String())
	}
}

func TestRemoteTrace_RefusedRequestLogsDeniedAndNeverTheToken(t *testing.T) {
	buf := captureTraceLogs(t)
	f := newRemoteFixture(t, remoteFixtureOpts{})
	var ungranted config.Project
	var err error
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		ungranted, err = project.CreateWithTokenKind(s, config.ProjectKindRemote, "Calendar", "", []string{"macmcp"}, []string{}, nil, nil)
	}), "create ungranted project")
	assertNoErr(t, err, "create ungranted project")
	projTok, _ := f.project.Token.Reveal()

	c := f.dial()
	if resp := c.roundTrip(`{"type":"CallTool","name":"mail_search","project_id":"` + ungranted.ID + `"}`); resp.Type != bridge.RespError {
		t.Fatalf("ungranted project was honoured: %s", resp.Type)
	}
	// The strict decoder refuses a token; the value must still stay out of the log.
	if resp := c.roundTrip(`{"type":"ListTools","token":"` + projTok + `"}`); resp.Type != bridge.RespError {
		t.Fatalf("token field was honoured: %s", resp.Type)
	}

	lines := remoteRequestLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("want 2 remote.request lines, got %v", lines)
	}
	if lines[0]["status"] != "denied" {
		t.Errorf("refused request status = %v, want denied: %v", lines[0]["status"], lines[0])
	}
	if lines[1]["status"] != "error" {
		t.Errorf("malformed request status = %v, want error", lines[1]["status"])
	}
	if strings.Contains(buf.String(), projTok) {
		t.Fatalf("project token reached the log:\n%s", buf.String())
	}
}

func TestRemoteTrace_ClientCannotChooseTheTraceID(t *testing.T) {
	buf := captureTraceLogs(t)
	f := newRemoteFixture(t, remoteFixtureOpts{})
	chosen := "ffffffffffffffffffffffffffffffff"

	resp := f.dial().roundTrip(`{"type":"ListTools","trace_id":"` + chosen + `"}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "trace_id") {
		t.Fatalf("a request carrying trace_id returned %s/%q, want a refusal naming the field", resp.Type, resp.Message)
	}
	for _, m := range traceLines(t, buf) {
		if m["trace_id"] == chosen {
			t.Fatalf("client-chosen trace_id reached a log line: %v", m)
		}
	}
}

func TestRemoteTrace_RepeatedUnenrolledReconnectsLogOnce(t *testing.T) {
	buf := captureTraceLogs(t)
	f := newRemoteFixture(t, remoteFixtureOpts{})
	ca, err := enrolment.LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")
	keyPEM, certPEM, _, err := ca.IssueClientCert("ghost")
	assertNoErr(t, err, "IssueClientCert")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	assertNoErr(t, err, "X509KeyPair")

	for i := 0; i < 4; i++ {
		if c := f.dialWith(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: f.caPool()}); c != nil {
			// The server closes without answering; reading to EOF orders the
			// close after the server's log call.
			_, _ = c.readFrame()
		}
	}

	// A fifth, enrolled connection proves the server finished the earlier ones.
	f.dial().roundTrip(`{"type":"ListTools"}`)

	n := 0
	for _, m := range traceLines(t, buf) {
		if m["msg"] == "remote: closing connection, certificate is not enrolled" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("4 unenrolled reconnects logged %d lines, want 1:\n%s", n, buf.String())
	}
}
