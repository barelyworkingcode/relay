package main

// Headline rule: every test that touches settings, pidfiles, logs, or
// the bridge socket MUST start with mkSandboxRelayHome(t). The
// sandbox-leak guard in support_safety_test.go fails the suite if you
// forget.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// testSealKeyID/testSealKey are a fixed, non-secret AES-256 key used by
// every hermetic test that needs a working sealer — never the real login
// keychain (headline rule above: no test may touch it), and never derived
// from anything random, so a failure reproduces byte for byte.
var (
	testSealKeyID = "0123456789abcdef"
	testSealKey   = bytes.Repeat([]byte{0x42}, 32)
)

// allowGate returns a presence.Gate wired to presencetest.Allow() — the
// hermetic seam ADR-017 implementation spec §6.8 requires. Every test that
// exercises a gated core's SUCCESS path needs one; a test exercising a
// refusal instead constructs its own presencetest.Deny() / NoSession() /
// Recording, or leaves Gate nil to exercise §6.7's fail-closed default.
func allowGate(t *testing.T) *presence.Gate {
	t.Helper()
	g, err := presence.NewGate(presencetest.Allow())
	if err != nil {
		t.Fatalf("presence.NewGate: %v", err)
	}
	return g
}

// enabledIssuanceRecorder returns an *audit.AuditRecorder that is Ready() —
// enabled and holding a live sink — so requireIssuanceAuditor (§7.4) does
// not refuse it. Every test exercising a gated core's success path needs
// one, since issuance auditing is now a hard dependency; a test exercising
// AC-26 (auditing off refuses) passes nil instead.
func enabledIssuanceRecorder(t *testing.T) *audit.AuditRecorder {
	t.Helper()
	rec, err := audit.NewAuditRecorder(nil, filepath.Join(t.TempDir(), "audit.jsonl"), openAuditWriter)
	if err != nil {
		t.Fatalf("NewAuditRecorder: %v", err)
	}
	if rec == nil {
		t.Fatal("NewAuditRecorder returned nil for an enabled config")
	}
	t.Cleanup(rec.Close)
	return rec
}

// testSealer returns a Sealer over the fixed test key. It needs no *testing.T
// and no cleanup: it is pure in-memory AES-GCM, not a keychain item.
func testSealer() sealed.Sealer {
	s, err := sealed.NewAESSealer(testSealKeyID, testSealKey)
	if err != nil {
		panic(err)
	}
	return s
}

// sealedSettingsStoreAt is the hermetic-suite stand-in for the tray's own
// NewSettingsStoreSealed: a store that can actually write, backed by
// testSealer rather than the login keychain. Most tests that need a
// SettingsStore at all want this one — plain NewSettingsStoreAt is for
// tests specifically exercising the CLI's read-only shape (errSealerRequired)
// or the degraded states in settings_store_sealed_test.go.
func sealedSettingsStoreAt(dir string) *config.FileSettingsStore {
	return config.NewSettingsStoreSealed(dir, testSealer())
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate support_test.go")
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root containing go.mod")
		}
	}
}

func relaySourceDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "cmd", "relay")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("relay command source directory %q is unavailable: %v", dir, err)
	}
	return dir
}

func fixtureRoot(t *testing.T) string {
	return filepath.Join(repoRoot(t), "test", "fixtures")
}

// Always call this BEFORE any code path that reads or writes the
// ConfigDir. The sandbox-leak guard (support_safety_test.go) catches
// violations at suite scope.
//
// Uses /tmp instead of t.TempDir() because macOS limits Unix-socket paths
// to 104 chars and bridge.SocketPath() lives inside ConfigDir — a typical
// t.TempDir() path already exceeds the limit.
func mkSandboxRelayHome(t *testing.T) string {
	t.Helper()
	dir := mkShortTempDir(t, "relay-home-")
	src := filepath.Join(fixtureRoot(t), "relay-home")
	if err := copyTree(src, dir); err != nil {
		t.Fatalf("mkSandboxRelayHome: copy %s → %s: %v", src, dir, err)
	}
	substituteHomePlaceholder(t, filepath.Join(dir, "settings.json"), dir)
	applyOverride(t, dir)
	return dir
}

func mkEmptySandboxRelayHome(t *testing.T) string {
	t.Helper()
	dir := mkShortTempDir(t, "relay-home-empty-")
	applyOverride(t, dir)
	return dir
}

// applyOverride sets bridge's ConfigDir override + HOME/XDG_CONFIG_HOME
// defense-in-depth, with cleanup.
func applyOverride(t *testing.T, dir string) {
	t.Helper()
	bridge.SetConfigDirForTest(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })
}

// mkShortTempDir creates a tempdir under /tmp (short paths) and registers
// cleanup. Use instead of t.TempDir() whenever the dir holds a Unix
// socket — macOS caps sun_path at 104 chars.
func mkShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkShortTempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newSandboxRouter(t *testing.T) (*appRouter, config.SettingsStore) {
	t.Helper()
	dir := mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("newSandboxRouter: EnsureInitialized: %v", err)
	}
	router := &appRouter{
		store:    store,
		tools:    NewExternalMcpManager(nil),
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
	}
	return router, store
}

// fakeServiceReloader is a no-op ServiceReloader. Real reloading would
// require a process supervisor; tests that need service-spawn behavior
// use the cmd/testservice binary via service_registry_test.go.
type fakeServiceReloader struct{}

func (f *fakeServiceReloader) Reload(id string, cfg *config.ServiceConfig) error { return nil }

// newBrokerRouter wires the six S5 op cores onto an appRouter the way
// trayapp.go does, backed by an allowing gate and a live issuance auditor —
// the shape a test needs to prove a brokered CLI command genuinely
// dispatches into its core (ADR-017 implementation spec §7) rather than
// merely reaching the transport. mutate lets a caller narrow one core's
// behaviour (a denying gate, a no-session context) without repeating the
// rest of the wiring.
func newBrokerRouter(t *testing.T, store config.SettingsStore, mutate func(*appRouter)) *appRouter {
	t.Helper()
	gate := allowGate(t)
	// startAuditRecorder, not enabledIssuanceRecorder: this router stands in
	// for the tray, and a test driving a CLI subcommand through it typically
	// wants the SAME on-disk audit log auditLogPath() resolves — the file
	// `relay audit` and this package's own aiLogText helpers read — not an
	// unrelated recorder pointed at a throwaway path.
	audit := startAuditRecorder(store.Get())
	if audit == nil {
		t.Fatal("newBrokerRouter: startAuditRecorder returned nil — auditing is off in this store's settings")
	}
	t.Cleanup(audit.Close)
	issuance := issuanceAuditorOrNil(audit)
	r := &appRouter{
		store:         store,
		tools:         NewExternalMcpManager(nil),
		services:      noopServiceManager{},
		enhanced:      NewEnhancedServiceRegistry(nil),
		onChange:      func() {},
		credentialOps: &CredentialOps{Store: store, Gate: gate, Issuance: issuance},
		enrolmentOps:  &EnrolmentOps{Store: store, Gate: gate, Issuance: issuance},
		loginOps:      &LoginOps{Store: store, Gate: gate, Audit: audit},
		mcpOps:        &McpOps{Store: store, Ctx: context.Background(), Gate: gate, Issuance: issuance},
		serviceOps:    &ServiceOps{Store: store, Registry: noopServiceManager{}, Gate: gate, Issuance: issuance},
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

// serveBroker starts a real bridge server over r on the sandboxed
// bridge.SocketPath() and stops it on cleanup, so a CLI subcommand's
// requireService/AdminOp round trip has a real peer to dial — the same
// transport AC-11/AC-12 exercise from the refusing side.
//
// This is subtle: bridge/server.go resolves each connection's REAL kernel
// audit session by default (§6.6), and the process running `go test` may
// itself belong to a session with no graphic access — over SSH, or in a
// CI/agent harness with no console attached. newBrokerRouter's whole point
// is "prove a brokered CLI command reaches its core given an ALLOWING
// gate" — i.e. simulate the operator sitting at the console — so this
// pins GraphicAccess: true explicitly rather than leaving the outcome
// dependent on whatever environment happens to run the suite. A test that
// wants the OTHER row (a caller that cannot show a prompt) constructs its
// own bridge.BridgeServer and overrides this the other way — see
// TestBridge_NoGraphicAccessRefusesWithoutPromptingTheProvider.
func serveBroker(t *testing.T, r *appRouter) {
	t.Helper()
	bs, err := bridge.NewBridgeServer(context.Background(), r)
	if err != nil {
		t.Fatalf("NewBridgeServer: %v", err)
	}
	bs.SetCallerSessionResolverForTest(func(net.Conn) presence.CallerSession {
		return presence.CallerSession{GraphicAccess: true}
	})
	go bs.Serve()
	t.Cleanup(bs.Close)
}

type FakeService struct {
	t         *testing.T
	serviceID string
	socket    string
	token     string
	manifest  bridge.Manifest

	mu       sync.Mutex
	requests []*fakeServiceRequest

	server   *http.Server
	listener net.Listener
}

type fakeServiceRequest struct {
	Method       string
	Path         string
	Query        url.Values
	Headers      http.Header
	Body         []byte
	WasWebSocket bool
}

type FakeServiceOptions struct {
	ServiceID string
	Manifest  bridge.Manifest
	// Handler is invoked after each request is recorded. nil → default echo handler.
	Handler http.HandlerFunc
}

// NewFakeService does NOT register with relay — use FakeService.Register or
// pass the value through your test bridge.
//
// Sockets land in /tmp instead of t.TempDir() because macOS limits Unix
// socket paths to 104 chars and t.TempDir() paths often exceed that on
// dev machines.
func NewFakeService(t *testing.T, opts FakeServiceOptions) *FakeService {
	t.Helper()
	if opts.ServiceID == "" {
		t.Fatal("NewFakeService: ServiceID required")
	}
	sockDir, err := os.MkdirTemp("/tmp", "fakesvc-")
	if err != nil {
		t.Fatalf("NewFakeService: mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, opts.ServiceID+".sock")

	fs := &FakeService{
		t:         t,
		serviceID: opts.ServiceID,
		socket:    sockPath,
		token:     "fake-internal-token-" + opts.ServiceID,
		manifest:  opts.Manifest,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.requests = append(fs.requests, &fakeServiceRequest{
			Method:       r.Method,
			Path:         r.URL.Path,
			Query:        r.URL.Query(),
			Headers:      r.Header.Clone(),
			Body:         body,
			WasWebSocket: strings.EqualFold(r.Header.Get("Upgrade"), "websocket"),
		})
		fs.mu.Unlock()
		if opts.Handler != nil {
			opts.Handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"service": opts.ServiceID,
			"path":    r.URL.Path,
			"method":  r.Method,
		})
	})

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("NewFakeService: listen %s: %v", sockPath, err)
	}
	fs.listener = ln
	fs.server = &http.Server{Handler: mux}
	go fs.server.Serve(ln)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = fs.server.Shutdown(ctx)
	})
	return fs
}

func (f *FakeService) Socket() string { return f.socket }

func (f *FakeService) Token() string { return f.token }

func (f *FakeService) ServiceID() string { return f.serviceID }

func (f *FakeService) Manifest() bridge.Manifest { return f.manifest }

func (f *FakeService) Requests() []*fakeServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeServiceRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *FakeService) LastRequest() *fakeServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func (f *FakeService) Register(client *bridge.Client) error {
	return client.RegisterManifest(bridge.RegisterManifestRequest{
		ServiceID:      f.serviceID,
		Manifest:       f.manifest,
		InternalSocket: f.socket,
		InternalToken:  f.token,
	})
}

// Drift-mitigation: relayLLM has its own test that asserts its actual
// generated manifest equals this file.
func NewFakeRelayLLMService(t *testing.T) *FakeService {
	t.Helper()
	manifest := loadManifestFixture(t, "relayllm.json")
	return NewFakeService(t, FakeServiceOptions{
		ServiceID: "relayLLM",
		Manifest:  manifest,
	})
}

func loadManifestFixture(t *testing.T, name string) bridge.Manifest {
	t.Helper()
	path := filepath.Join(fixtureRoot(t), "manifests", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loadManifestFixture %s: %v", path, err)
	}
	// Strip the "_comment" key — bridge.Manifest doesn't have it.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("loadManifestFixture %s: %v", path, err)
	}
	delete(raw, "_comment")
	clean, _ := json.Marshal(raw)
	var m bridge.Manifest
	if err := json.Unmarshal(clean, &m); err != nil {
		t.Fatalf("loadManifestFixture %s: %v", path, err)
	}
	return m
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		// Tolerate t.TempDir() permission (0700) for files placed inside it.
		mode := info.Mode().Perm()
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, mode)
	})
}

func substituteHomePlaceholder(t *testing.T, path, home string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		// settings.json may not exist in every fixture — fine.
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("substituteHomePlaceholder: %v", err)
	}
	out := bytes.ReplaceAll(data, []byte("${RELAY_HOME}"), []byte(home))
	if !bytes.Equal(out, data) {
		if err := os.WriteFile(path, out, 0600); err != nil {
			t.Fatalf("substituteHomePlaceholder: %v", err)
		}
	}
}

func assertNoErr(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		args = append(args, err)
		t.Fatalf(format+": %v", args...)
	}
}

func assertManifestHasRoutes(t *testing.T, m bridge.Manifest, want ...string) {
	t.Helper()
	have := make(map[string]bool, len(m.Routes))
	for _, r := range m.Routes {
		have[r] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("manifest missing routes: %v\n  have: %v", missing, m.Routes)
	}
}

func dialUnixWithTimeout(t *testing.T, sock string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dialUnixWithTimeout %s: %v", sock, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakeServiceServer is a scriptable HTTP server over a Unix socket, standing
// in for a managed service's internal endpoint (status, actions, ...).
// Shared by ipc_service_action_test.go; internal/service has its own copy
// for StatusClient's own tests since a test helper cannot cross a package
// boundary.
type fakeServiceServer struct {
	t         *testing.T
	socket    string
	listener  net.Listener
	server    *http.Server
	mu        sync.Mutex
	responses map[string]fakeResponse
	requests  []recordedRequest
}

type fakeResponse struct {
	status int
	body   []byte
}

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
}

func newFakeServiceServer(t *testing.T) *fakeServiceServer {
	t.Helper()
	// /tmp because t.TempDir paths blow past macOS's 104-char unix socket
	// limit (matches the relayLLM-side FakeBridge pattern).
	dir, err := os.MkdirTemp("/tmp", "fss")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	sockPath := filepath.Join(dir, "svc.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeServiceServer{
		t:         t,
		socket:    sockPath,
		listener:  ln,
		responses: make(map[string]fakeResponse),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.server = &http.Server{Handler: mux}
	go f.server.Serve(ln)
	t.Cleanup(func() {
		_ = f.server.Close()
		_ = os.RemoveAll(dir)
	})
	return f
}

func (f *fakeServiceServer) script(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[method+" "+path] = fakeResponse{status: status, body: []byte(body)}
}

func (f *fakeServiceServer) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeServiceServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Auth:   r.Header.Get("Authorization"),
	})
	resp, ok := f.responses[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}
