package provider

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
