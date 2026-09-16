package provider

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/pioverlay"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// fakeModelBroker starts a Unix-socket HTTP server standing in for relay's
// model.sock, recording the Authorization header it received.
func fakeModelBroker(t *testing.T) (socketPath string, gotAuth *string) {
	t.Helper()
	dir := shortTempDir(t)
	socketPath = filepath.Join(dir, "model.sock")
	var auth string
	gotAuth = &auth
	fakeBroker(t, socketPath, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	return socketPath, gotAuth
}

// TestPiProvider_RealSpawn_NoSecretsInRealEnvOrArgv_OverlayCarriesKey spawns
// a real child through PiProvider.Start, the same standard the Claude
// real-spawn test holds itself to: inspect the actual process's env/argv on
// disk, not just the builder functions. It also proves the one place the
// model key legitimately does land — models.json's apiKey — matches C8's
// LaunchSpec key exactly, and that the file is 0600.
func TestPiProvider_RealSpawn_NoSecretsInRealEnvOrArgv_OverlayCarriesKey(t *testing.T) {
	scratch := t.TempDir()
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	binDir := t.TempDir()

	script := writeEnvArgvDumpScript(t, binDir)
	envOut := filepath.Join(scratch, "env.out")
	argvOut := filepath.Join(scratch, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)
	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")

	modelSocket, gotAuth := fakeModelBroker(t)
	const modelKey = "rmk_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	session := &sessionstypes.Session{
		ID:        "pi-sess-1",
		Model:     "claude-sonnet-4",
		Directory: projectDir,
	}
	p := NewPiProvider(session, func(string, json.RawMessage) {}, PiConfig{
		Binary:       script,
		DataDir:      dataDir,
		BridgeSocket: "/tmp/relay-bridge.sock",
		ModelSocket:  modelSocket,
		ModelKey:     modelKey,
	})
	defer p.Kill()

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForFile(t, envOut)
	waitForFile(t, argvOut)

	env := readLines(t, envOut)
	argv := readLines(t, argvOut)

	assertNoForbiddenSecrets(t, "real pi child env", env)
	assertNoForbiddenSecrets(t, "real pi child argv", argv)

	for _, e := range env {
		if strings.HasPrefix(e, "RELAY_PROJECT_TOKEN=") {
			t.Fatalf("ambient RELAY_PROJECT_TOKEN reached the real pi child: %s", e)
		}
	}

	var overlayDir string
	for _, e := range env {
		if strings.HasPrefix(e, "PI_CODING_AGENT_DIR=") {
			overlayDir = strings.TrimPrefix(e, "PI_CODING_AGENT_DIR=")
		}
	}
	if overlayDir == "" {
		t.Fatal("PI_CODING_AGENT_DIR not set in real child env")
	}
	wantEnv := []string{
		"RELAY_SESSION_ID=pi-sess-1",
		"RELAY_BRIDGE_SOCKET=/tmp/relay-bridge.sock",
		"RELAY_MODEL_SOCKET=" + modelSocket,
	}
	for _, want := range wantEnv {
		found := false
		for _, e := range env {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("real pi child env missing %q; got %v", want, env)
		}
	}

	modelsPath := filepath.Join(overlayDir, "models.json")
	assertFileMode0600(t, modelsPath)

	data, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	var parsed struct {
		Providers map[string]struct {
			APIKey string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal models.json: %v", err)
	}
	relayEntry, ok := parsed.Providers[pioverlay.RelayProvider]
	if !ok {
		t.Fatalf("models.json missing %q provider: %s", pioverlay.RelayProvider, data)
	}
	if relayEntry.APIKey != modelKey {
		t.Errorf("models.json apiKey = %q, want the LaunchSpec key %q", relayEntry.APIKey, modelKey)
	}

	// The proxy must actually forward to the socket with headers intact —
	// exercise it exactly the way pi's own HTTP client would.
	if p.modelProxy == nil {
		t.Fatal("model proxy not started")
	}
	proxyBaseURL := "http://" + p.modelProxy.listener.Addr().String()
	req, err := http.NewRequest(http.MethodPost, proxyBaseURL+"/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+modelKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()
	if *gotAuth != "Bearer "+modelKey {
		t.Errorf("broker saw Authorization = %q, want Bearer %s", *gotAuth, modelKey)
	}

	// Kill must remove the overlay dir so the key does not sit on disk after
	// the session ends (SP9's residual-key-window callout).
	p.Kill()
	if _, err := os.Stat(overlayDir); !os.IsNotExist(err) {
		t.Fatalf("overlay dir %s still present after Kill", overlayDir)
	}
}

// TestPiProvider_Start_FailedStartClosesModelProxyAndOverlay is the fix for
// the leak on every error path after the proxy/overlay are created: a
// nonexistent pi binary makes cmd.Start() itself fail, the last of the
// documented failure points, so this exercises the full unwind.
func TestPiProvider_Start_FailedStartClosesModelProxyAndOverlay(t *testing.T) {
	projectDir := t.TempDir()
	dataDir := t.TempDir()

	modelSocket, _ := fakeModelBroker(t)
	const modelKey = "rmk_" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	session := &sessionstypes.Session{ID: "pi-sess-fail", Model: "claude-sonnet-4", Directory: projectDir}
	p := NewPiProvider(session, func(string, json.RawMessage) {}, PiConfig{
		Binary:      filepath.Join(t.TempDir(), "no-such-pi-binary"),
		DataDir:     dataDir,
		ModelSocket: modelSocket,
		ModelKey:    modelKey,
	})
	defer p.Kill()

	if err := p.Start(); err == nil {
		t.Fatal("expected Start to fail for a nonexistent pi binary")
	}

	if p.modelProxy != nil {
		t.Error("modelProxy still set on the provider after a failed Start")
	}
	if p.overlayDir != "" {
		t.Error("overlayDir still set on the provider after a failed Start")
	}

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("projectDir has leftover overlay entries after a failed Start: %v", entries)
	}
}

// TestPiProvider_Start_ClosesStaleModelProxyAndOverlayFromPriorCall is the
// fix for the second half of the leak: Start must close/remove any
// modelProxy/overlayDir already attached to the provider before creating
// its own, so a re-Start without an intervening Kill (the pattern
// SetPermissionMode's Kill-then-Start idiom establishes, and pi will need
// once SetModel lands) can't silently orphan the previous listener by
// overwriting the pointer.
//
// This attaches the stale resources directly rather than by calling Start
// twice: a second live Start on the same provider would race the first
// call's still-running idleWatcher goroutine, a separate, pre-existing
// concurrency gap in Start/idleWatcher's shared fields that is out of scope
// for this fix (Start has never supported being called again on a live
// provider without Kill in between; this fix only closes the resource leak
// in that scenario, not the rest of the field races).
func TestPiProvider_Start_ClosesStaleModelProxyAndOverlayFromPriorCall(t *testing.T) {
	scratch := t.TempDir()
	projectDir := t.TempDir()
	dataDir := t.TempDir()
	binDir := t.TempDir()

	script := writeEnvArgvDumpScript(t, binDir)
	envOut := filepath.Join(scratch, "env.out")
	argvOut := filepath.Join(scratch, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", envOut)
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)

	modelSocket, _ := fakeModelBroker(t)
	const modelKey = "rmk_" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	session := &sessionstypes.Session{ID: "pi-sess-restart", Model: "claude-sonnet-4", Directory: projectDir}
	p := NewPiProvider(session, func(string, json.RawMessage) {}, PiConfig{
		Binary:      script,
		DataDir:     dataDir,
		ModelSocket: modelSocket,
		ModelKey:    modelKey,
	})
	defer p.Kill()

	staleBaseURL, staleProxy, err := startModelProxy(modelSocket)
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	staleAddr := staleProxy.listener.Addr().String()
	p.modelProxy = staleProxy

	staleOverlayDir, err := pioverlay.MaterializePiOverlay(projectDir, pioverlay.PiOverlayInputs{
		ModelID:  "claude-sonnet-4",
		ModelKey: modelKey,
		BaseURL:  staleBaseURL + "/v1",
	})
	if err != nil {
		t.Fatalf("MaterializePiOverlay: %v", err)
	}
	p.overlayDir = staleOverlayDir

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, envOut)

	if conn, err := net.DialTimeout("tcp", staleAddr, time.Second); err == nil {
		conn.Close()
		t.Fatalf("stale model proxy listener at %s is still accepting connections after Start", staleAddr)
	}
}
