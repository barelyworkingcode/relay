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
)

// The grandchild keeps only stdout open; its stderr goes to /dev/null so the
// stdout drain alone decides when process_exited arrives.
func TestProviderExit_GrandchildHoldingStdoutDoesNotBlockExit(t *testing.T) {
	prev := providerDrainTimeout
	providerDrainTimeout = 100 * time.Millisecond
	t.Cleanup(func() { providerDrainTimeout = prev })

	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			var grandchild int
			t.Cleanup(func() {
				if grandchild > 0 {
					_ = syscall.Kill(grandchild, syscall.SIGKILL)
				}
			})
			rec := newExitRecorder()
			started := time.Now()
			spawnFake(t, kind, "drain-"+kind, "sleep 30 2>/dev/null &\necho $! > '"+pidFile+".tmp'\nmv '"+pidFile+".tmp' '"+pidFile+"'\nexit 0\n", rec.handle)
			waitForFile(t, pidFile)
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("read grandchild pid: %v", err)
			}
			if grandchild, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
				t.Fatalf("parse grandchild pid %q: %v", data, err)
			}
			rec.waitExited(t, time.Second)
			elapsed := time.Since(started)
			if err := syscall.Kill(grandchild, 0); err != nil {
				t.Fatalf("grandchild %d gone before process_exited was checked, so stdout was never held: %v", grandchild, err)
			}
			t.Logf("process_exited after %s", elapsed)
		})
	}
}

const msgSupersededExitDropped = "provider exit from a superseded spawn dropped"

// The first spawn leaves a grandchild holding its stderr, so that spawn's
// drain runs out its deadline only after Kill has returned and the second
// spawn is live. The second spawn is still sleeping, so any process_exited
// after the restart belongs to the first.
func TestProviderExit_RestartedSpawnGetsNoStaleProcessExited(t *testing.T) {
	prev := providerDrainTimeout
	providerDrainTimeout = 100 * time.Millisecond
	t.Cleanup(func() { providerDrainTimeout = prev })

	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			var grandchild int
			t.Cleanup(func() {
				if grandchild > 0 {
					_ = syscall.Kill(grandchild, syscall.SIGKILL)
				}
			})
			logs := captureDebugJSON(t)
			id := "restart-" + kind
			var restarted atomic.Bool
			stale := make(chan json.RawMessage, 1)
			script := "if [ ! -e '" + pidFile + "' ]; then\n" +
				"sleep 30 >/dev/null &\necho $! > '" + pidFile + ".tmp'\nmv '" + pidFile + ".tmp' '" + pidFile + "'\nfi\n" +
				"exec sleep 30\n"
			p := spawnFake(t, kind, id, script, func(ev string, data json.RawMessage) {
				if ev == "process_exited" && restarted.Load() {
					select {
					case stale <- data:
					default:
					}
				}
			})
			waitForFile(t, pidFile)
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("read grandchild pid: %v", err)
			}
			if grandchild, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
				t.Fatalf("parse grandchild pid %q: %v", data, err)
			}

			p.Kill()
			if err := p.Start(); err != nil {
				t.Fatalf("restart: %v", err)
			}
			restarted.Store(true)

			deadline := time.Now().Add(5 * time.Second)
			for !loggedSupersededDrop(logs.all(), id, kind) {
				if time.Now().After(deadline) {
					t.Fatalf("no %q record for %s; stale process_exited delivered: %v", msgSupersededExitDropped, id, len(stale) > 0)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(stale) > 0 {
				t.Fatalf("the killed spawn's process_exited %s reached the handler after the restart", <-stale)
			}
		})
	}
}

func loggedSupersededDrop(log, session, kind string) bool {
	for _, l := range strings.Split(log, "\n") {
		var r struct{ Level, Msg, Session, Kind string }
		if json.Unmarshal([]byte(l), &r) == nil && r.Level == "DEBUG" && r.Msg == msgSupersededExitDropped && r.Session == session && r.Kind == kind {
			return true
		}
	}
	return false
}
