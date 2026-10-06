package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// A grandchild holds the child's stdout, so the drain cannot finish until
// providerDrainTimeout after the reap. Kill returning at the reap is then
// observable: the handler has not run yet.
const grandchildPrologue = "tail -f /dev/null 2>/dev/null &\necho $! > \"$PIDFILE.tmp\"\nmv \"$PIDFILE.tmp\" \"$PIDFILE\"\n"

func killGrandchildOnCleanup(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestProviderKill_DeliversProcessExitedBeforeReturn(t *testing.T) {
	prev := providerDrainTimeout
	providerDrainTimeout = 500 * time.Millisecond
	t.Cleanup(func() { providerDrainTimeout = prev })

	for _, kind := range []string{"claude", "pi", "codex"} {
		t.Run(kind, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			t.Setenv("PIDFILE", pidFile)
			killGrandchildOnCleanup(t, pidFile)

			var exited atomic.Bool
			ready := make(chan struct{}, 1)
			handler := func(ev string, _ json.RawMessage) {
				switch ev {
				case "process_exited":
					exited.Store(true)
				case "raw_output":
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			}

			var p startKiller
			if kind == "codex" {
				fx, err := filepath.Abs(codexFixtureOK)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("TESTCODEX_FIXTURE", fx)
				wrapper := writeFakeCLI(t, grandchildPrologue+"exec '"+buildTestCodexBinary(t)+"' \"$@\"\n")
				sess := &sessionstypes.Session{ID: "kill-codex", Model: "codex/gpt-6-luna", Directory: t.TempDir()}
				p = NewCodexProvider(sess, handler, CodexConfig{Binary: wrapper})
				t.Cleanup(p.Kill)
				if err := p.Start(); err != nil {
					t.Fatalf("codex Start: %v", err)
				}
			} else {
				p = spawnFake(t, kind, "kill-"+kind, grandchildPrologue+"echo ready\nexec cat >/dev/null\n", handler)
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("fake CLI never printed its ready line")
				}
			}
			waitForFile(t, pidFile)

			p.Kill()
			if !exited.Load() {
				t.Fatal("Kill returned before process_exited reached the handler")
			}
		})
	}
}
