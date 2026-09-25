//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

type relayToolsFixture struct {
	store        config.SettingsStore
	enhanced     *EnhancedServiceRegistry
	audit        *audit.AuditRecorder
	frontendSock string
	eveBearer    string
	firstChat    chan []byte
}

// rtBuildBundle builds the real relay and relay-sessions binaries into a
// bundle-shaped directory, so BuiltinRelaySessionsService resolves the helper
// exactly as it does inside Relay.app. It runs before HOME moves, because the
// go build cache lives under HOME.
func rtBuildBundle(t *testing.T) (relayBin string) {
	t.Helper()
	contents := filepath.Join(mkShortTempDir(t, "rt-bundle-"), "Relay.app", "Contents")
	relayBin = filepath.Join(contents, "MacOS", "relay")
	for _, b := range []struct{ out, pkg string }{
		{relayBin, "./cmd/relay"},
		{service.RelaySessionsHelperPath(relayBin), "./cmd/relaysessions"},
	} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		assertNoErr(t, cmd.Run(), "build %s", b.pkg)
	}
	return relayBin
}

// newRelayToolsFixture puts relay's config dir at its default location under
// a temp HOME: `relay mcp` takes no socket flag and dials the default bridge
// socket, so an override the child cannot see would leave it talking to
// nothing.
func newRelayToolsFixture(t *testing.T) *relayToolsFixture {
	t.Helper()
	relayBin := rtBuildBundle(t)

	home, err := filepath.EvalSymlinks(mkShortTempDir(t, "rt-home-"))
	assertNoErr(t, err, "resolve temp home")
	configDir := filepath.Join(home, "Library", "Application Support", "relay")
	assertNoErr(t, os.MkdirAll(configDir, 0o700), "mkdir config dir")
	claudeBin := filepath.Join(home, ".local", "bin", "claude")
	assertNoErr(t, os.MkdirAll(filepath.Dir(claudeBin), 0o700), "mkdir local bin")
	buildClaude := exec.Command("go", "build", "-o", claudeBin, "./cmd/testclaude")
	buildClaude.Dir = repoRoot(t)
	buildClaude.Stderr = os.Stderr
	assertNoErr(t, buildClaude.Run(), "build testclaude as the session's claude")
	bridge.SetConfigDirForTest(configDir)
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("RELAY_MODEL_SOCKET", "")

	store := sealedSettingsStoreAt(configDir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	tools := mcpbroker.NewManager(nil)
	okResult := func(context.Context, string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}
	addMockConn(tools, "rt-mcp", newMockConn("rt-mcp", localTools("echo"), okResult))
	addMockConn(tools, "rt-mcp-b", newMockConn("rt-mcp-b", localTools("b-only"), okResult))

	eveBearer := "rt-eve-bearer"
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.ExternalMcps = append(s.ExternalMcps,
			config.ExternalMcp{ID: "rt-mcp", DisplayName: "Acme MCP"},
			config.ExternalMcp{ID: "rt-mcp-b", DisplayName: "Acme MCP B"},
		)
		addAPICredential(s, config.APICredential{
			ID: "rt-eve", Name: "rt-eve", Hash: config.HashToken(eveBearer),
			Classes: slices.Clone(frontendConsumerClasses), Created: time.Now().UTC().Format(time.RFC3339),
		})
	}), "seed settings")

	on := true
	rec, err := audit.NewAuditRecorder(&config.AuditConfig{LogLists: &on},
		filepath.Join(mkShortTempDir(t, "rt-audit-"), "audit.jsonl"), openAuditWriter)
	assertNoErr(t, err, "NewAuditRecorder")
	t.Cleanup(rec.Close)

	launches := service.NewLaunches()
	enhanced := NewEnhancedServiceRegistry(nil)
	ledgerDB, err := ledger.Open(t.TempDir())
	assertNoErr(t, err, "ledger.Open")
	modelKeys := NewModelKeyTable()
	accounting := newSessionAccounting()

	router := &appRouter{
		store: store, tools: tools, services: &fakeServiceReloader{},
		enhanced: enhanced, launches: launches, onChange: func() {},
		audit: rec, sessions: ledgerDB, modelKeys: modelKeys, sessionAccounts: accounting,
		membership: &membershipAuth{launches: launches, src: membership.NewSource(), hostPID: 1 << 30},
	}
	bsrv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = bsrv.Serve() }()
	t.Cleanup(bsrv.Close)
	_ = dialUnixWithTimeout(t, bridge.SocketPath(), 2*time.Second).Close()

	f := &relayToolsFixture{store: store, enhanced: enhanced, audit: rec, eveBearer: eveBearer, firstChat: make(chan []byte, 1)}
	f.startModelBroker(t)

	f.frontendSock = filepath.Join(mkShortTempDir(t, "rt-fe-"), "frontend.sock")
	deps := sessionRouteDeps{
		store: store, launches: launches, sessions: ledgerDB, modelKeys: modelKeys,
		enhanced: enhanced, auditor: rec, accounting: accounting, resumeGuard: newResumeGuard(),
	}
	fsrv, err := NewFrontendServer(store, tools, tools, tools, Endpoint{Socket: f.frontendSock}, enhanced,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		NewCredentialAuthorizer(store), nil, launches, deps)
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = fsrv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fsrv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, f.frontendSock, 2*time.Second).Close()

	logDir := mkShortTempDir(t, "rt-log-")
	registry := service.NewRegistry()
	registry.Launches = launches
	registry.Enhanced = enhanced
	registry.OpenLog = func(id string) (io.WriteCloser, error) {
		return os.OpenFile(filepath.Join(logDir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	t.Cleanup(registry.StopAll)
	host := service.BuiltinRelaySessionsService(relayBin, configDir, true)
	assertNoErr(t, registry.Start(&host), "start relay-sessions from the built-in record")

	deadline := time.Now().Add(rs10Timeout)
	for enhanced.Get(config.RelaySessionsServiceID) == nil {
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(logDir, config.RelaySessionsServiceID+".log"))
			t.Fatalf("relay-sessions never registered its manifest; its log:\n%s", log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f
}

// startModelBroker answers the chat provider's ping and records the body of
// the first chat request, then streams a one-word reply.
func (f *relayToolsFixture) startModelBroker(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("unix", bridge.ModelSocketPath())
	assertNoErr(t, err, "listen model socket")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case f.firstChat <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// chatFirstRequest launches a chat session with settings in a project that
// grants only rt-mcp, sends one message, and returns the session id and the
// tool names in the model's first request.
func (f *relayToolsFixture) chatFirstRequest(t *testing.T, settings map[string]any) (string, []string) {
	t.Helper()
	proj := config.Project{ID: "rt-p1", Name: "Acme", Path: t.TempDir(),
		AllowedMcpIDs: []string{"rt-mcp"}, AllowedModels: []string{"*"}, AllowedTemplates: []string{"*"}}
	assertNoErr(t, f.store.With(func(s *config.Settings) { s.Projects = append(s.Projects, proj) }), "seed project")

	create, _ := json.Marshal(map[string]any{"projectId": proj.ID, "model": "gpt-5", "settings": settings})
	req, _ := http.NewRequest(http.MethodPost, "http://unix/api/sessions", bytes.NewReader(create))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.eveBearer)
	resp, err := dialFrontendHTTP(f.frontendSock).Do(req)
	assertNoErr(t, err, "POST /api/sessions")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var sess struct {
		ID string `json:"sessionId"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(data, &sess) != nil || sess.ID == "" {
		t.Fatalf("POST /api/sessions: status %d, body %s", resp.StatusCode, data)
	}

	dispatcher := httptest.NewServer(NewFrontendDispatcher(f.enhanced))
	t.Cleanup(dispatcher.Close)
	go func() {
		r, err := http.Post(dispatcher.URL+"/api/sessions/"+sess.ID+"/message", "application/json",
			bytes.NewReader([]byte(`{"text":"list the files in this project"}`)))
		if err == nil {
			r.Body.Close()
		}
	}()

	var body []byte
	select {
	case body = <-f.firstChat:
	case <-time.After(rs10Timeout):
		t.Fatal("the chat session never sent a request to the model broker")
	}
	var chat struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	assertNoErr(t, json.Unmarshal(body, &chat), "decode first chat request %s", body)
	var names []string
	for _, tool := range chat.Tools {
		names = append(names, tool.Function.Name)
	}
	return sess.ID, names
}

func (f *relayToolsFixture) listToolsEvent(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	f.audit.Flush()
	for _, ev := range rs10AuditLines(t, f.audit.Path()) {
		actor, _ := ev["actor"].(map[string]any)
		if ev["event"] == string(audit.AuditEventListTools) && actor["session_id"] == sessionID {
			return ev
		}
	}
	return nil
}

func TestSessionHost_ChatRelayTools_GrantedToolsInFirstRequest(t *testing.T) {
	f := newRelayToolsFixture(t)
	sessionID, tools := f.chatFirstRequest(t, map[string]any{"useRelayTools": true})

	if !slices.Contains(tools, "echo") {
		t.Fatalf("first chat request tools = %q, want the granted MCP's echo", tools)
	}
	if slices.Contains(tools, "b-only") {
		t.Fatalf("first chat request tools = %q, carries b-only from an MCP the project does not grant", tools)
	}

	ev := f.listToolsEvent(t, sessionID)
	if ev == nil {
		t.Fatalf("no %s audit event for session %s", audit.AuditEventListTools, sessionID)
	}
	actor, _ := ev["actor"].(map[string]any)
	if actor["kind"] != audit.AuditActorProjectSession || actor["auth"] != audit.AuditAuthSession {
		t.Fatalf("list_tools actor = %v, want kind %q auth %q", actor, audit.AuditActorProjectSession, audit.AuditAuthSession)
	}
}

func TestSessionHost_ChatRelayTools_OptOutSpawnsNoToolServer(t *testing.T) {
	f := newRelayToolsFixture(t)
	sessionID, tools := f.chatFirstRequest(t, map[string]any{"useRelayTools": false})

	if len(tools) != 0 {
		t.Fatalf("first chat request tools = %q, want none for an opted-out session", tools)
	}
	if ev := f.listToolsEvent(t, sessionID); ev != nil {
		t.Fatalf("an opted-out session listed relay tools: %v", ev)
	}
}
