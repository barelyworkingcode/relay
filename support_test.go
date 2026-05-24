package main

// Test support helpers — the canonical entry points for any test that
// needs a sandboxed config dir, a wired router, an in-process bridge,
// or a fake service. See docs/decisions/001-testing-strategy.md and
// docs/decisions/002-test-seams.md.
//
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

	"relaygo/bridge"
)

// repoRoot returns the absolute path to the repository root, derived from
// this file's location. Stable across test working directories.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate support_test.go")
	}
	return filepath.Dir(file)
}

// fixtureRoot returns the path to test/fixtures/.
func fixtureRoot(t *testing.T) string {
	return filepath.Join(repoRoot(t), "test", "fixtures")
}

// mkSandboxRelayHome allocates a tempdir, populates it with a writable copy
// of test/fixtures/relay-home/, points bridge.ConfigDir() at it, and
// registers cleanup. Returns the sandbox directory.
//
// Always call this BEFORE any code path that reads or writes the
// ConfigDir. The sandbox-leak guard (support_safety_test.go) catches
// violations at suite scope, but per-test failures are easier to debug
// when the sandbox is set up correctly from the start.
//
// Uses /tmp instead of t.TempDir() because macOS limits Unix-socket paths
// to 104 chars and bridge.SocketPath() lives inside ConfigDir — a typical
// t.TempDir() path already exceeds the limit. /tmp-based names keep us
// safe. Cleaned up at test exit.
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

// mkEmptySandboxRelayHome is the same as mkSandboxRelayHome but skips the
// fixture copy. For tests that explicitly want to verify first-launch
// behavior or construct settings programmatically.
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

// newSandboxRouter builds an appRouter wired against the sandbox fixture
// install. Use when a test needs a router that thinks it's talking to a
// fully-populated relay (3 projects, 3 MCPs, 2 services from the fixture
// settings.json).
//
// Returns the router and the SettingsStore so tests can mutate state via
// store.With and observe the effect through router calls.
func newSandboxRouter(t *testing.T) (*appRouter, SettingsStore) {
	t.Helper()
	dir := mkSandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
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

func (f *fakeServiceReloader) Reload(id string, cfg *ServiceConfig) error { return nil }

// ---------------------------------------------------------------------------
// FakeService — stub enhanced service for dispatcher tests
// ---------------------------------------------------------------------------

// FakeService is an in-process enhanced service: a Unix socket serving
// HTTP, optionally registered with a manifest, that records inbound
// requests for assertion.
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
	Method        string
	Path          string
	Query         url.Values
	Headers       http.Header
	Body          []byte
	WasWebSocket  bool
}

// FakeServiceOptions configures a FakeService. ServiceID + Manifest are
// required; Handler is optional (defaults to a 200/JSON echo).
type FakeServiceOptions struct {
	ServiceID string
	Manifest  bridge.Manifest
	// Handler is invoked after each request is recorded. nil → default echo handler.
	Handler http.HandlerFunc
}

// NewFakeService starts a FakeService on a short Unix socket path with a
// randomly-generated internal token. Does NOT register with relay — use
// FakeService.Register or pass the value through your test bridge.
//
// Sockets land in /tmp instead of t.TempDir() because macOS limits Unix
// socket paths to 104 chars and t.TempDir() paths often exceed that on
// dev machines. /tmp keeps us comfortably under the cap. Cleaned up on
// test exit via t.Cleanup.
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
			"ok":       true,
			"service":  opts.ServiceID,
			"path":     r.URL.Path,
			"method":   r.Method,
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

// Socket returns the Unix socket path the service listens on.
func (f *FakeService) Socket() string { return f.socket }

// Token returns the internal bearer token relay will inject when proxying.
func (f *FakeService) Token() string { return f.token }

// ServiceID returns the service identifier used in RegisterManifest.
func (f *FakeService) ServiceID() string { return f.serviceID }

// Manifest returns the declared manifest.
func (f *FakeService) Manifest() bridge.Manifest { return f.manifest }

// Requests returns a snapshot of recorded requests in arrival order.
func (f *FakeService) Requests() []*fakeServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeServiceRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// LastRequest returns the most-recent recorded request or nil.
func (f *FakeService) LastRequest() *fakeServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// Register sends a RegisterManifest call to relay via the given
// service-authenticated bridge client.
func (f *FakeService) Register(client *bridge.Client) error {
	return client.RegisterManifest(bridge.RegisterManifestRequest{
		ServiceID:      f.serviceID,
		Manifest:       f.manifest,
		InternalSocket: f.socket,
		InternalToken:  f.token,
	})
}

// NewFakeRelayLLMService is a specialization of NewFakeService preloaded
// with the relayLLM manifest from test/fixtures/manifests/relayllm.json.
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

// ---------------------------------------------------------------------------
// Filesystem helpers
// ---------------------------------------------------------------------------

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

// substituteHomePlaceholder rewrites ${RELAY_HOME} → home in the named
// file (idempotent — silently does nothing if the placeholder isn't
// present). Mirrors what scripts/demo.sh does.
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

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// assertNoErr fails the test with formatted context if err is non-nil.
func assertNoErr(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		args = append(args, err)
		t.Fatalf(format+": %v", args...)
	}
}

// assertManifestHasRoutes fails the test if any of the wanted routes are
// missing from the manifest. Order- and extras-tolerant.
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

// dialUnixWithTimeout opens a Unix-socket connection with a short timeout.
// Useful in service-readiness polls.
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

