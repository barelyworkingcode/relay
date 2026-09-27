package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// The browser-test lock is eve's (scripts/browser-lock.js): the same file and
// the same protocol, so a screen phase and an eve browser run never share the
// screen. The kernel drops a flock when its holder dies, so there is no
// staleness check.

const browserLockRetry = time.Second

func browserLockPath() string {
	if p := os.Getenv("EVE_BROWSER_LOCK"); p != "" {
		abs, err := filepath.Abs(p)
		if err == nil {
			return abs
		}
		return p
	}
	return filepath.Join(home, ".cache", "eve", "browser-tests.lock")
}

func browserLockTimeout() (time.Duration, error) {
	raw := os.Getenv("EVE_BROWSER_LOCK_TIMEOUT")
	if raw == "" {
		return 1800 * time.Second, nil
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("EVE_BROWSER_LOCK_TIMEOUT must be a whole number of seconds, got %q", raw)
	}
	return time.Duration(n) * time.Second, nil
}

type lockHolder struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Since   string `json:"since"`
}

// lockClock is the retry loop's time source, injected so a test can step it.
type lockClock struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

var realLockClock = lockClock{now: time.Now, after: time.After}

func takeBrowserLock(ctx context.Context, command string) (release func(), err error) {
	return takeBrowserLockWith(ctx, command, realLockClock)
}

func takeBrowserLockWith(ctx context.Context, command string, clock lockClock) (release func(), err error) {
	path := browserLockPath()
	timeout, err := browserLockTimeout()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot take %s: %w", path, err)
	}
	// This is deliberate: append mode, so a failed attempt never truncates
	// the holder's record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot take %s: %w", path, err)
	}
	deadline := clock.now().Add(timeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("cannot take %s: %w", path, err)
		}
		if clock.now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("gave up after %s; %s is held by %s", timeout, path, describeHolder(path))
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-clock.after(browserLockRetry):
		}
	}
	rec, _ := json.Marshal(lockHolder{PID: os.Getpid(), Command: command, Since: clock.now().UTC().Format(time.RFC3339)})
	err = f.Truncate(0)
	if err == nil {
		_, err = f.Write(append(rec, '\n'))
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot record the lock holder in %s: %w", path, err)
	}
	return func() {
		_ = f.Truncate(0)
		_ = f.Close()
	}, nil
}

func describeHolder(path string) string {
	raw, err := os.ReadFile(path)
	var h lockHolder
	if err != nil || json.Unmarshal(raw, &h) != nil || h.PID == 0 {
		return "an unknown holder"
	}
	return fmt.Sprintf("pid %d (%s) since %s", h.PID, h.Command, h.Since)
}
