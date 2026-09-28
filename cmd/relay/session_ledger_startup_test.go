package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// A chat left "live" on disk by a previous run must be relaunched on resume:
// its provider died with that run, so answering "already live" strands it.
func TestOpenSessionLedger_StaleLiveChatResumesWithLaunch(t *testing.T) {
	f := newSessionRoutesFixture(t)
	configDir := mkSandboxRelayHome(t)

	prev, err := ledger.Open(configDir)
	assertNoErr(t, err, "open previous run's ledger")
	assertNoErr(t, prev.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateLive,
		SessionRequest: json.RawMessage(`{"projectId":"` + f.proj.ID + `","directory":"` + f.proj.Path + `","model":"m1"}`),
	}), "seed stale live record")

	sessLedger := openSessionLedger(configDir)
	if sessLedger == nil {
		t.Fatal("openSessionLedger returned nil for a readable ledger")
	}

	// Routes close over a copy of deps, so they are registered again on a
	// fresh mux once the startup ledger replaces the fixture's own.
	f.deps.sessions = sessLedger
	f.mux = http.NewServeMux()
	RegisterSessionRoutes(&control.RouteRegistrar{Mux: f.mux, Transport: control.TransportSocket}, f.deps)

	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler:   fakeLaunchHandler(&fs, func(id string) string { return `{"sessionId":"` + id + `"}` }),
	})
	f.registerFakeSessionsHost(t, fs, selfPeerToken(t).Process())

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req.SetPathValue("id", "s1")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var body resumeResponseBody
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode resume response")
	if body.SessionID != "s1" || !body.Resumed {
		t.Fatalf("resume response = %+v, want {s1 true}", body)
	}

	reqs := fs.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/launch" {
		t.Fatalf("host requests = %+v, want exactly one /launch", reqs)
	}
	var spec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(reqs[0].Body, &spec), "unmarshal spec relay-sessions received")
	if spec.SessionID != "s1" || !spec.Resume {
		t.Fatalf("launch spec session_id=%q resume=%v, want s1 true", spec.SessionID, spec.Resume)
	}
}
