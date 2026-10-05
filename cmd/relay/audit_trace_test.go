package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
)

func TestAuditTrace_BridgeCallRecordCarriesTheInboundTrace(t *testing.T) {
	mkSandboxRelayHome(t)
	mock := newMockConn("fsmcp", localTools("read_file"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)
	id := logging.NewTraceID()

	ctx := logging.ContextWithTrace(context.Background(), id)
	if _, err := r.CallTool(ctx, "read_file", nil, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if ev := onlyEvent(t, readLoggedEvents(t, rec)); ev.TraceID != id {
		t.Errorf("record trace_id = %q, want %q", ev.TraceID, id)
	}
}

func TestAuditTrace_CallWithNoTraceLeavesTheFieldOut(t *testing.T) {
	mkSandboxRelayHome(t)
	mock := newMockConn("fsmcp", localTools("read_file"), okHandler(`{"content":[]}`))
	r, rec := auditedRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn}, nil,
		map[string]*mockMcpConn{"fsmcp": mock}, nil)

	if _, err := r.CallTool(context.Background(), "read_file", nil, testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if strings.Contains(amLogBytes(t, rec), "trace_id") {
		t.Error("a call with no trace wrote a trace_id field")
	}
}

func TestAuditTrace_ModelCallRecordCarriesTheRequestTrace(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)
	rec := newTestAudit(t, nil)
	wireModelAudit(m, rec)
	id := logging.NewTraceID()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"vCode","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(logging.TraceHeader, id)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != audit.AuditEventModelCall || ev.TraceID != id {
		t.Errorf("model_call record = event %q trace_id %q, want trace_id %q", ev.Event, ev.TraceID, id)
	}
}
