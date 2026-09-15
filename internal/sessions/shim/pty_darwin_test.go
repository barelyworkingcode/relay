//go:build darwin

package shim_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTYPair opens a fresh BSD pty pair without pulling in creack/pty
// (reserved for R-S6 per go.mod's own comment): /dev/ptmx plus the three
// ioctls darwin's posix_openpt/grantpt/unlockpt/ptsname are themselves
// built on.
func openPTYPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatalf("TIOCPTYGRANT: %v", err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatalf("TIOCPTYUNLK: %v", err)
	}
	var buf [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, m.Fd(), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		t.Fatalf("TIOCPTYGNAME: %v", errno)
	}
	name := string(bytes.TrimRight(buf[:], "\x00"))
	s, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open slave %s: %v", name, err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, s
}

type targetMarker struct {
	PID  int `json:"pid"`
	PPID int `json:"ppid"`
	PGID int `json:"pgid"`
}

// TestExec_PTY_CttyAndForegroundPgrp covers C6's PTY bullet: the shim must
// become a session leader with the given slave as its controlling terminal
// (step 2), and the target it spawns must be the foreground process group
// on that terminal (step 5's Setpgid+Foreground+Ctty:0).
func TestExec_PTY_CttyAndForegroundPgrp(t *testing.T) {
	_, testTargetBin := buildBinaries(t)
	master, slave := openPTYPair(t)

	dir := mkShortTempDir(t, "pty-")
	marker := filepath.Join(dir, "marker.json")

	h := startExec(t, execOpts{
		sessionID:  "sess-pty",
		pty:        true,
		ptySlave:   slave,
		targetArgv: []string{testTargetBin, "-marker", marker},
	})
	// The child now has its own duplicate of the slave fd; this process's
	// copy must go, both so EOF/hangup semantics on the pty behave normally
	// and so lsof-style fd inspection of the shim/target never shows a
	// leaked extra reference through the test process.
	_ = slave.Close()

	h.waitForEvent("started", 10*time.Second)
	waitForFile(t, marker, 5*time.Second)

	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var info targetMarker
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse marker: %v", err)
	}

	fgpgrp, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil {
		t.Fatalf("TIOCGPGRP: %v", err)
	}
	if fgpgrp != info.PGID {
		t.Fatalf("foreground pgrp = %d, want the target's own pgid %d", fgpgrp, info.PGID)
	}

	shimPID := h.cmd.Process.Pid
	shimSID, err := syscall.Getsid(shimPID)
	if err != nil {
		t.Fatalf("getsid(shim): %v", err)
	}
	if shimSID != shimPID {
		t.Fatalf("shim sid = %d, want %d (shim must be its own session leader)", shimSID, shimPID)
	}
	targetSID, err := syscall.Getsid(info.PID)
	if err != nil {
		t.Fatalf("getsid(target): %v", err)
	}
	if targetSID != shimPID {
		t.Fatalf("target sid = %d, want %d (the shim's session)", targetSID, shimPID)
	}

	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill target: %v", err)
	}
	h.waitForEvent("exit", 10*time.Second)
	h.wait()
}
