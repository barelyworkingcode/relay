package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// testLock points the browser lock at a temp file and returns it.
func testLock(t *testing.T, timeout string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "browser-tests.lock")
	t.Setenv("EVE_BROWSER_LOCK", path)
	t.Setenv("EVE_BROWSER_LOCK_TIMEOUT", timeout)
	return path
}

// holdLock takes the lock on its own fd, as another process would.
func holdLock(t *testing.T, path string) (*os.File, error) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func TestBrowserLockRecordAndRelease(t *testing.T) {
	path := testLock(t, "0")
	release, err := takeBrowserLock(context.Background(), "devboxverify --phase screen")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(raw), "}\n") || strings.Count(string(raw), "\n") != 1 {
		t.Errorf("holder record %q is not one JSON line", raw)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil || len(rec) != 3 {
		t.Fatalf("holder record %q: want exactly pid, command, since (%v)", raw, err)
	}
	if pid, _ := rec["pid"].(float64); int(pid) != os.Getpid() || rec["command"] != "devboxverify --phase screen" {
		t.Errorf("holder record %q names another pid or command", raw)
	}
	if since, err := time.Parse(time.RFC3339, rec["since"].(string)); err != nil || time.Since(since) > time.Minute {
		t.Errorf("since %v is not a current RFC3339 time: %v", rec["since"], err)
	}
	if _, err := holdLock(t, path); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("a second fd took the held lock: %v", err)
	}
	release()
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Errorf("release left the file at %v bytes (%v), want truncated", fi.Size(), err)
	}
	if _, err := holdLock(t, path); err != nil {
		t.Errorf("the lock is still held after release: %v", err)
	}
}

func TestBrowserLockBusyKeepsHolderRecord(t *testing.T) {
	path := testLock(t, "0")
	f, err := holdLock(t, path)
	if err != nil {
		t.Fatal(err)
	}
	const rec = `{"pid":1,"command":"node eve-browser","since":"2026-01-01T00:00:00.000Z"}` + "\n"
	if _, err := f.WriteString(rec); err != nil {
		t.Fatal(err)
	}
	if release, err := takeBrowserLock(context.Background(), "devboxverify"); err == nil {
		release()
		t.Fatal("took a lock another fd holds")
	}
	if raw, _ := os.ReadFile(path); string(raw) != rec {
		t.Errorf("a busy attempt changed the holder's record to %q", raw)
	}
}

func TestBrowserLockWaitsForTheHolder(t *testing.T) {
	path := testLock(t, "5")
	f, err := holdLock(t, path)
	if err != nil {
		t.Fatal(err)
	}
	unlocked := make(chan struct{})
	go func() {
		defer close(unlocked)
		time.Sleep(300 * time.Millisecond)
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}()
	release, err := takeBrowserLock(context.Background(), "devboxverify")
	<-unlocked
	if err != nil {
		t.Fatalf("did not take the lock once the holder unlocked: %v", err)
	}
	release()
}
