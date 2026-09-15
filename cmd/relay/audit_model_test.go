package main

// R-M1c: the model endpoint's AuditHook, wired to a real *audit.AuditRecorder
// via recordModelCall, replaces the slog.Debug stand-in trayapp.go carried.
// These tests drive the real handler over the same fake-router-socket
// scaffolding model_endpoint_test.go's own tests use (newModelEndpointTestServer,
// registerFakeHost, fakeRouterMux), with a real recorder writing to a
// sandboxed temp file (newTestAudit, from audit_test.go) — never asserting
// against ModelCallAudit alone, since the point of this unit is what lands
// on disk.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/modelbroker"
)

// wireModelAudit points m's AuditHook at the real recorder path this unit
// adds, exactly as trayapp.go now does.
func wireModelAudit(m *ModelEndpointServer, rec *audit.AuditRecorder) {
	m.AuditHook = func(ev ModelCallAudit) { recordModelCall(rec, ev) }
}

// amLogBytes reads the whole audit log as bytes, for the canary/secret
// absence checks: it does not matter which field a leak rode out on, only
// that it reached the file.
func amLogBytes(t *testing.T, rec *audit.AuditRecorder) string {
	t.Helper()
	rec.Flush()
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read audit log: %v", err)
	}
	return string(data)
}

const (
	amPromptCanary     = "CANARY-PROMPT-9c2f1a"
	amCompletionCanary = "CANARY-COMPLETION-77e0b4"
)

// TestModelAudit_OkRecordsRequestedCanonicalTargetUsageAndProjectActor is the
// golden "ok" record: requested, canonical and upstream-target are three
// distinct facts, usage is parsed, and the actor names the project (id and
// name) it authenticated as via a plain token.
func TestModelAudit_OkRecordsRequestedCanonicalTargetUsageAndProjectActor(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Relay-Model-Target", "ep/gpt-x")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + amCompletionCanary + `"}}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)
	assertNoErr(t, store.With(func(s *config.Settings) {
		for i := range s.Projects {
			if s.Projects[i].ID == "p1" {
				s.Projects[i].Name = "My Project"
			}
		}
	}), "rename project")

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	body := `{"model":"vCode","messages":[{"role":"user","content":"` + amPromptCanary + `"}]}`
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != audit.AuditEventModelCall {
		t.Errorf("event = %q, want %q", ev.Event, audit.AuditEventModelCall)
	}
	if ev.Actor.Kind != audit.AuditActorProject || ev.Actor.Auth != audit.AuditAuthToken {
		t.Errorf("actor = %+v, want kind=project auth=token", ev.Actor)
	}
	if ev.Actor.ProjectID != "p1" || ev.Actor.ProjectName != "My Project" {
		t.Errorf("actor project = %q/%q, want p1/My Project", ev.Actor.ProjectID, ev.Actor.ProjectName)
	}
	if ev.Model != "vCode" || ev.ModelCanonical != "vCode" {
		t.Errorf("model=%q canonical=%q, want vCode/vCode", ev.Model, ev.ModelCanonical)
	}
	if ev.ModelTarget != "ep/gpt-x" {
		t.Errorf("model_target = %q, want ep/gpt-x", ev.ModelTarget)
	}
	if ev.PromptTokens != 11 || ev.CompletionTokens != 22 {
		t.Errorf("usage = %d/%d, want 11/22", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.Outcome != audit.AuditOutcomeOK || ev.Status != http.StatusOK {
		t.Errorf("outcome=%q status=%d, want ok/200", ev.Outcome, ev.Status)
	}
	if ev.Transport != transportSocket || ev.Method != http.MethodPost || ev.Path != "/v1/chat/completions" {
		t.Errorf("route = %s %s over %s, want POST /v1/chat/completions over socket", ev.Method, ev.Path, ev.Transport)
	}

	logBytes := amLogBytes(t, rec)
	for _, secret := range []string{tok, amPromptCanary, amCompletionCanary} {
		if strings.Contains(logBytes, secret) {
			t.Errorf("audit log bytes contain %q, which must never be recorded", secret)
		}
	}
}

// TestModelAudit_DeniedVsNotFoundAreDistinctInAuditButIdentical404OnWire is
// docs/model-endpoint.md's Scoping section made concrete: a model outside
// the grant and a model absent from the catalog answer the byte-identical
// 404 to the caller, but the audit tells them apart.
func TestModelAudit_DeniedVsNotFoundAreDistinctInAuditButIdentical404OnWire(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	// Granted only "vCode": "omlx/Chat" exists in the fake catalog but is not
	// granted (denied); "does-not-exist" is granted nothing to compare
	// against because it is nowhere in the catalog (not_found).
	tok := addModelProject(t, store, "restricted", []string{"vCode"}, false)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	deniedResp := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"omlx/Chat"}`)
	notFoundResp := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"does-not-exist"}`)

	if deniedResp.Code != http.StatusNotFound || notFoundResp.Code != http.StatusNotFound {
		t.Fatalf("wire status = %d / %d, want 404 / 404 (identical to the caller)", deniedResp.Code, notFoundResp.Code)
	}
	if deniedResp.Body.String() != notFoundResp.Body.String() {
		t.Fatalf("denied and not_found bodies differ on the wire: %q vs %q", deniedResp.Body.String(), notFoundResp.Body.String())
	}

	events := readLoggedEvents(t, rec)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	byModel := map[string]audit.AuditEvent{}
	for _, ev := range events {
		byModel[ev.Model] = ev
	}
	if got := byModel["omlx/Chat"].Outcome; got != audit.AuditOutcomeDenied {
		t.Errorf("omlx/Chat outcome = %q, want denied", got)
	}
	if got := byModel["does-not-exist"].Outcome; got != audit.AuditOutcomeNotFound {
		t.Errorf("does-not-exist outcome = %q, want not_found", got)
	}
}

// TestModelAudit_UnauthorizedRecordsUnknownActorWithAttemptedAuth follows
// audit_call.go's setUnauthenticated precedent: a credential that did not
// resolve is Actor.Kind unknown, never a caller-shaped guess, but Auth still
// names what was attempted.
func TestModelAudit_UnauthorizedRecordsUnknownActorWithAttemptedAuth(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	garbage := "garbage-bearer-value-never-resolves"
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", garbage, nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	// GET /v1/models is model_list, gated by LogLists (off by default) —
	// use a call route instead so the unauthorized attempt is unconditionally
	// recorded like any other model_call.
	w = doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", garbage, nil, `{"model":"vCode"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Kind != audit.AuditActorUnknown {
		t.Errorf("actor.kind = %q, want unknown", ev.Actor.Kind)
	}
	if ev.Actor.Auth != audit.AuditAuthToken {
		t.Errorf("actor.auth = %q, want token (a bearer was attempted)", ev.Actor.Auth)
	}
	if ev.Actor.ProjectID != "" || ev.Actor.ProjectName != "" {
		t.Errorf("actor carries project fields for a caller that never resolved: %+v", ev.Actor)
	}
	if ev.Outcome != audit.AuditOutcomeUnauthorized || ev.Status != http.StatusUnauthorized {
		t.Errorf("outcome=%q status=%d, want unauthorized/401", ev.Outcome, ev.Status)
	}

	logBytes := amLogBytes(t, rec)
	if strings.Contains(logBytes, garbage) {
		t.Errorf("audit log bytes contain the presented bearer value %q", garbage)
	}
}

// TestModelAudit_TCPNoHeaderRecordsNoAuthAttempted is the "nothing was even
// tried" half of the unauthenticated case: TCP with no header at all never
// reaches a bearer or identity check, so Auth is none rather than a value
// implying an attempt.
func TestModelAudit_TCPNoHeaderRecordsNoAuthAttempted(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportTCP), http.MethodPost, "/v1/chat/completions", "", nil, `{"model":"vCode"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Kind != audit.AuditActorUnknown || ev.Actor.Auth != audit.AuditAuthNone {
		t.Errorf("actor = %+v, want kind=unknown auth=none", ev.Actor)
	}
	if ev.Transport != transportTCP {
		t.Errorf("transport = %q, want tcp", ev.Transport)
	}
}

// TestModelAudit_ModelKeyRecordsLabelNeverKey is spec-model-broker.md §7's
// "model_key_label, never the key" promise, proven against the file bytes.
func TestModelAudit_ModelKeyRecordsLabelNeverKey(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	addModelProject(t, store, "p1", nil, false)
	key, err := m.modelKeys.Mint("p1", "session:x")
	assertNoErr(t, err, "Mint")

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", key, nil, `{"model":"vCode"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Kind != audit.AuditActorProject || ev.Actor.Auth != audit.AuditAuthModelKey {
		t.Errorf("actor = %+v, want kind=project auth=model_key", ev.Actor)
	}
	if ev.ModelKeyLabel != "session:x" {
		t.Errorf("model_key_label = %q, want session:x", ev.ModelKeyLabel)
	}

	logBytes := amLogBytes(t, rec)
	if strings.Contains(logBytes, key) {
		t.Fatalf("audit log bytes contain the model key plaintext")
	}
}

// TestModelAudit_ServiceIdentityRecordsServiceIDNotProjectFields is the
// launch-identity path (docs/model-endpoint.md's Auth order §2): no bearer
// header at all, resolved by kernel-attested identity on model.sock.
func TestModelAudit_ServiceIdentityRecordsServiceIDNotProjectFields(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	addModelServiceRecord(t, store, "relaytts", []string{"*"})
	svcCtx := bindModelIdentity(t, launches, "relaytts", []config.ServiceCapability{config.ServiceCapabilityModels}, 60001)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", svcCtx, `{"model":"vCode"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Actor.Kind != audit.AuditActorService || ev.Actor.Auth != audit.AuditAuthService {
		t.Errorf("actor = %+v, want kind=service auth=service", ev.Actor)
	}
	if ev.Actor.ServiceID != "relaytts" {
		t.Errorf("actor.service_id = %q, want relaytts", ev.Actor.ServiceID)
	}
	if ev.Actor.ProjectID != "" || ev.Actor.ProjectName != "" {
		t.Errorf("a service actor carries project fields: %+v", ev.Actor)
	}
}

// TestModelAudit_HostUnavailableRecordsOutcome503 covers a call route (not
// a listing, so it is never gated by log_lists) with no model host
// registered at all.
func TestModelAudit_HostUnavailableRecordsOutcome503(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "p1", nil, false)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"vCode"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != "host_unavailable" || ev.Status != http.StatusServiceUnavailable {
		t.Errorf("outcome=%q status=%d, want host_unavailable/503", ev.Outcome, ev.Status)
	}
}

// TestModelAudit_BodyTooLargeRecordsOutcome413 mirrors
// TestModelEndpoint_BodyOverCapRefused413, adding the audit assertion.
func TestModelAudit_BodyTooLargeRecordsOutcome413(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	body := buildJSONBodyOfSize(t, modelbroker.JSONBodyCap+1, "vCode")
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != "body_too_large" || ev.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("outcome=%q status=%d, want body_too_large/413", ev.Outcome, ev.Status)
	}
}

// TestModelAudit_RateLimitedWhenBodyBudgetCannotAdmit forces BodyBudget.Acquire
// to fail IMMEDIATELY (weight > capacity never blocks — bodybudget.go's own
// doc comment) rather than waiting out the real 5s bodyBudgetWaitTimeout, so
// this is a fast, deterministic golden record for "rate_limited" rather than
// a slow one relying on the real timeout firing.
func TestModelAudit_RateLimitedWhenBodyBudgetCannotAdmit(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "p1", nil, false)
	m.bodyBudget = modelbroker.NewBodyBudget(1)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"vCode"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != "rate_limited" || ev.Status != http.StatusTooManyRequests {
		t.Errorf("outcome=%q status=%d, want rate_limited/429", ev.Outcome, ev.Status)
	}
}

// TestModelAudit_ClientAbortWhileWaitingForBodyBudget is the OTHER admission
// outcome (serveModelRoute's own distinction): the caller's own context was
// already done when admission failed, so relay is not the one that refused
// the call — audited client_abort, not rate_limited, even though the wire
// response is the same 429 shape either way.
func TestModelAudit_ClientAbortWhileWaitingForBodyBudget(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "p1", nil, false)
	m.bodyBudget = modelbroker.NewBodyBudget(int64(modelbroker.JSONBodyCap))
	// Occupy the whole budget by hand so the real request below has nothing
	// to acquire; nothing in this test ever releases it.
	if !m.bodyBudget.Acquire(context.Background(), int64(modelbroker.JSONBodyCap)) {
		t.Fatal("could not pre-occupy the body budget")
	}

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the handler ever checks it

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (the wire shape is unchanged regardless of WHY admission failed)", w.Code)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeClientAbort {
		t.Errorf("outcome = %q, want client_abort", ev.Outcome)
	}
}

// TestModelAudit_ErrAbortHandlerPanicRecordsClientAbort exercises proxy()'s
// recover path via the existing proxyPanicForTest seam, this time asserting
// against the real recorder rather than only the ModelCallAudit value.
func TestModelAudit_ErrAbortHandlerPanicRecordsClientAbort(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	proxyPanicForTest = http.ErrAbortHandler
	t.Cleanup(func() { proxyPanicForTest = nil })

	defer func() {
		if panicVal := recover(); panicVal != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler unchanged", panicVal)
		}
		ev := onlyEvent(t, readLoggedEvents(t, rec))
		if ev.Outcome != audit.AuditOutcomeClientAbort {
			t.Errorf("outcome = %q, want client_abort", ev.Outcome)
		}
	}()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)
	t.Fatal("ServeHTTP returned normally; the synthetic client-abort panic should have propagated")
}

// TestModelAudit_SyntheticPanicRecordsErrorAndRepanics is the "error" golden
// record via the same seam, a genuine bug rather than a client fault.
func TestModelAudit_SyntheticPanicRecordsErrorAndRepanics(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)

	synthetic := errors.New("synthetic bug, not a client abort")
	proxyPanicForTest = synthetic
	t.Cleanup(func() { proxyPanicForTest = nil })

	defer func() {
		if got := recover(); got != error(synthetic) {
			t.Fatalf("recovered %v, want the original synthetic value unchanged", got)
		}
		ev := onlyEvent(t, readLoggedEvents(t, rec))
		if ev.Outcome != audit.AuditOutcomeError {
			t.Errorf("outcome = %q, want error", ev.Outcome)
		}
	}()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)
	t.Fatal("ServeHTTP returned normally; the synthetic panic should have propagated")
}

// TestModelAudit_FailOpenOnUnwritableSink proves the recorder's existing
// fail-open contract (internal/audit/audit.go's write()) extends to a model
// call unchanged: the call still succeeds, and the write failure is logged,
// never delaying or failing the response.
func TestModelAudit_FailOpenOnUnwritableSink(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	// failingWriteCloser is defined in audit_issuance_test.go, in this same
	// package: a recorder over a writer that refuses every write is "the
	// sink exists and fails", the state under test — a nil recorder would
	// instead be "auditing is off", a different state entirely.
	rec := audit.NewAuditRecorderWith(audit.ResolveAuditConfig(&config.AuditConfig{}), "am-broken", failingWriteCloser{})
	t.Cleanup(rec.Close)
	wireModelAudit(m, rec)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"vCode"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even though the audit sink cannot be written", w.Code)
	}
	// Flush proves the writer goroutine actually attempted (and failed) the
	// write before this test ends, rather than racing its own assertion
	// against an async goroutine that hasn't run yet.
	rec.Flush()
}

// TestAuditModelDetail_RendersChainTokensAndBytes is audit_cmd.go's table
// rendering, item 3's "model rows render legibly" requirement.
func TestAuditModelDetail_RendersChainTokensAndBytes(t *testing.T) {
	ev := audit.AuditEvent{
		Event:            audit.AuditEventModelCall,
		Method:           http.MethodPost,
		Path:             "/v1/chat/completions",
		Model:            "vCode",
		ModelCanonical:   "vCode",
		ModelTarget:      "ep/gpt-x",
		PromptTokens:     11,
		CompletionTokens: 22,
		RequestBytes:     100,
		ResponseBytes:    200,
	}
	detail := auditBaseDetail(ev)
	for _, want := range []string{"vCode -> ep/gpt-x", "tokens=11/22", "bytes=100/200"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}
}

// TestAuditCallerLabel_ServiceIDUsedWhenNoOtherIdentity proves the CALLER
// column is never a bare dash for a model-endpoint service actor.
func TestAuditCallerLabel_ServiceIDUsedWhenNoOtherIdentity(t *testing.T) {
	got := auditCallerLabel(audit.AuditActor{Kind: audit.AuditActorService, Auth: audit.AuditAuthService, ServiceID: "relaytts"})
	if got != "relaytts" {
		t.Errorf("caller label = %q, want relaytts", got)
	}
}

// TestAuditQuery_FiltersModelEventsAndReservedKindAgainstAFixtureLog proves
// item 3's `--event model_call|model_list` and `--kind project_session`
// filters mechanically work — including project_session, which nothing in
// this repo emits yet — against a hand-built fixture log, the same
// audit.AuditQuery.Matches mechanism runAuditCommand uses.
func TestAuditQuery_FiltersModelEventsAndReservedKindAgainstAFixtureLog(t *testing.T) {
	fixture := []audit.AuditEvent{
		{ID: "1", Event: audit.AuditEventCallTool, Actor: audit.AuditActor{Kind: audit.AuditActorProject}},
		{ID: "2", Event: audit.AuditEventModelCall, Actor: audit.AuditActor{Kind: audit.AuditActorProject}, Model: "vCode"},
		{ID: "3", Event: audit.AuditEventModelList, Actor: audit.AuditActor{Kind: audit.AuditActorService}},
		{ID: "4", Event: audit.AuditEventModelCall, Actor: audit.AuditActor{Kind: audit.AuditActorProjectSession}, Model: "vCode"},
	}
	path := writeFixtureAuditLog(t, fixture)

	events := audit.ReadAuditTail(path, audit.AuditTailBudget)

	modelCalls := filterEvents(events, audit.AuditQuery{Event: audit.AuditEventModelCall})
	if len(modelCalls) != 2 {
		t.Fatalf("--event model_call matched %d events, want 2", len(modelCalls))
	}
	for _, ev := range modelCalls {
		if ev.ID != "2" && ev.ID != "4" {
			t.Errorf("--event model_call matched unexpected id %q", ev.ID)
		}
	}

	modelLists := filterEvents(events, audit.AuditQuery{Event: audit.AuditEventModelList})
	if len(modelLists) != 1 || modelLists[0].ID != "3" {
		t.Fatalf("--event model_list matched %+v, want exactly id 3", modelLists)
	}

	sessions := filterEvents(events, audit.AuditQuery{Kind: audit.AuditActorProjectSession})
	if len(sessions) != 1 || sessions[0].ID != "4" {
		t.Fatalf("--kind project_session matched %+v, want exactly id 4", sessions)
	}
}

// writeFixtureAuditLog writes events as JSONL to a fresh temp file and
// returns its path, standing in for a log `relay audit` would read directly
// off disk.
func writeFixtureAuditLog(t *testing.T, events []audit.AuditEvent) string {
	t.Helper()
	path := t.TempDir() + "/fixture.jsonl"
	f, err := os.Create(path)
	assertNoErr(t, err, "create fixture log")
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, ev := range events {
		assertNoErr(t, enc.Encode(ev), "encode fixture event")
	}
	return path
}

func filterEvents(events []audit.AuditEvent, q audit.AuditQuery) []audit.AuditEvent {
	var out []audit.AuditEvent
	for i := range events {
		if q.Matches(&events[i]) {
			out = append(out, events[i])
		}
	}
	return out
}
