package session_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
)

const stopAllSessionID = "66666666-0000-0000-0000-000000000001"

// useTestCodex builds testcodex and points it at the completed-turn fixture.
func useTestCodex(t *testing.T) string {
	t.Helper()
	bin := buildTestCodex(t)
	fx, err := filepath.Abs(filepath.Join("..", "provider", "testdata", "codex", "turn_ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTCODEX_FIXTURE", fx)
	return bin
}

// codexWithGrandchild returns a codex binary that first leaves a grandchild
// holding the child's stdout, so the provider's output drain outlasts the
// reap. The grandchild dies at test cleanup.
func codexWithGrandchild(t *testing.T) string {
	t.Helper()
	bin := useTestCodex(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	wrapper := filepath.Join(t.TempDir(), "codex-wrapper")
	script := "#!/bin/sh\ntail -f /dev/null 2>/dev/null &\necho $! > '" + pidFile + ".tmp'\nmv '" + pidFile + ".tmp' '" + pidFile + "'\nexec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func codexSpec(t *testing.T, key string, resume bool) session.CreateSpec {
	return session.CreateSpec{
		SessionID: stopAllSessionID, ProjectID: "11111111-0000-0000-0000-000000000001",
		Kind: session.KindCodex, Model: "codex/gpt-6-luna", Directory: t.TempDir(),
		ModelKey: key, Resume: resume,
	}
}

func TestStopAll_ReturnsAfterExitHandlerPersisted(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(session.Config{Codex: provider.CodexConfig{Binary: codexWithGrandchild(t)}}, session.NewStore(dir), nil)
	exited := make(chan struct{})
	var once sync.Once
	mgr.SetExitHandler(func(string, int) { once.Do(func() { close(exited) }) })
	// A late exit handler would write into dir during its removal.
	t.Cleanup(func() {
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("exit handler never ran")
		}
	})

	if _, err := mgr.Create(codexSpec(t, "", false)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mgr.StopAll()

	select {
	case <-exited:
	default:
		t.Fatal("StopAll returned before the exit handler persisted the record")
	}
	if _, err := session.NewStore(dir).Load(stopAllSessionID); err != nil {
		t.Fatalf("Load after StopAll: %v", err)
	}
}

// Resuming a live session under a new key displaces its provider; that
// provider's exit must not be reported, or relay revokes the new key.
func TestCreate_ResumeWithNewKey_DropsDisplacedExit(t *testing.T) {
	mgr := session.NewManager(session.Config{Codex: provider.CodexConfig{Binary: useTestCodex(t)}}, session.NewStore(t.TempDir()), nil)
	var mu sync.Mutex
	var reported []string
	mgr.SetExitHandler(func(id string, _ int) {
		mu.Lock()
		reported = append(reported, id)
		mu.Unlock()
	})
	t.Cleanup(mgr.StopAll)

	if _, err := mgr.Create(codexSpec(t, "rmk_old", false)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := mgr.Create(codexSpec(t, "rmk_new", true)); err != nil {
		t.Fatalf("Create resume: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 0 {
		t.Fatalf("displaced provider's exit was reported for %v", reported)
	}
}
