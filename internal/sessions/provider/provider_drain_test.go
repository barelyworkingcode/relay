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

// The first spawn leaves a grandchild holding its stderr and exits on its own,
// so its drain stays pending after the reap. The test restarts the provider
// without Kill, then kills the grandchild so the first spawn's drain finishes
// under the new generation. The second spawn is still running, so any
// process_exited after the restart belongs to the first.
func TestProviderExit_RestartedSpawnGetsNoStaleProcessExited(t *testing.T) {
	prev := providerDrainTimeout
	providerDrainTimeout = 30 * time.Second
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
				"sleep 60 >/dev/null &\necho $! > '" + pidFile + ".tmp'\nmv '" + pidFile + ".tmp' '" + pidFile + "'\nexit 0\nfi\n" +
				"exec sleep 30\n"
			sk := spawnFake(t, kind, id, script, func(ev string, data json.RawMessage) {
				if ev == "process_exited" && restarted.Load() {
					select {
					case stale <- data:
					default:
					}
				}
			})
			var waitDone chan struct{}
			var exitOf func() *spawnExit
			switch p := sk.(type) {
			case *ClaudeProvider:
				waitDone, exitOf = p.waitDone, func() *spawnExit { return p.exit }
			case *PiProvider:
				waitDone, exitOf = p.waitDone, func() *spawnExit { return p.exit }
			default:
				t.Fatalf("unexpected provider type %T", sk)
			}
			waitForFile(t, pidFile)
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("read grandchild pid: %v", err)
			}
			if grandchild, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
				t.Fatalf("parse grandchild pid %q: %v", data, err)
			}

			select {
			case <-waitDone:
			case <-time.After(10 * time.Second):
				t.Fatal("first spawn was not reaped")
			}
			old := exitOf()
			if err := sk.Start(); err != nil {
				t.Fatalf("restart: %v", err)
			}
			restarted.Store(true)
			if err := syscall.Kill(grandchild, syscall.SIGKILL); err != nil {
				t.Fatalf("kill grandchild %d: %v", grandchild, err)
			}
			grandchild = 0

			select {
			case <-old.done:
			case <-time.After(10 * time.Second):
				t.Fatal("first spawn's exit was neither delivered nor dropped")
			}
			if !loggedSupersededDrop(logs.all(), id, kind) {
				t.Fatalf("no %q record for %s; stale process_exited delivered: %v", msgSupersededExitDropped, id, len(stale) > 0)
			}
			if len(stale) > 0 {
				t.Fatalf("the first spawn's process_exited %s reached the handler after the restart", <-stale)
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

// With no descendant holding a pipe, the killed spawn's drain can finish
// before the restart's Start runs; that spawn's exit must still not reach
// the handler.
func TestClaudeSetPermissionMode_RestartGetsNoProcessExited(t *testing.T) {
	logs := captureDebugJSON(t)
	const id = "mode-restart"
	var restarting atomic.Bool
	stale := make(chan json.RawMessage, 1)
	p := spawnFake(t, "claude", id, "exec sleep 30\n", func(ev string, data json.RawMessage) {
		if ev == "process_exited" && restarting.Load() {
			select {
			case stale <- data:
			default:
			}
		}
	}).(*ClaudeProvider)

	restarting.Store(true)
	if err := p.SetPermissionMode("plan"); err != nil {
		t.Fatalf("SetPermissionMode: %v", err)
	}
	if !p.Alive() {
		t.Fatal("provider not alive after the permission-mode restart")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case data := <-stale:
			t.Fatalf("the restarted session got process_exited %s from its killed spawn", data)
		default:
		}
		if loggedSupersededDrop(logs.all(), id, "claude") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q record for %s within 5s", msgSupersededExitDropped, id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
