//go:build !windows

package service

import (
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SetProcessGroup configures a process group for the given command.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcessGroup SIGTERMs the group (not just the shell PID, so children
// that outlive the shell are also caught), waits 1s, then SIGKILLs.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	_ = syscall.Kill(-pid, syscall.SIGTERM)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// KillProcessGroupPID sends SIGKILL straight to the process group led by
// pid. Unlike KillProcessGroup, there is no SIGTERM/wait grace period: R-S9
// calls this only for a root process a relaysessions launch owned once
// that launch has already ended (crash, restart, or an explicit stop), so
// there is no clean shutdown left to request — the only job left is making
// sure nothing orphaned keeps running (spec-session-host.md §4.4, §6).
func KillProcessGroupPID(pid int32) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-int(pid), syscall.SIGKILL)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// BuildCommand constructs an exec.Cmd for a service based on its config.
func BuildCommand(cfg *config.ServiceConfig) (*exec.Cmd, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	fullCmd := shellQuote(cfg.Command)
	for _, arg := range cfg.Args {
		fullCmd += " " + shellQuote(arg)
	}

	cmd := exec.Command(shell, "-l", "-c", fullCmd)
	SetProcessGroup(cmd)
	if cfg.WorkingDir != "" {
		cmd.Dir = cfg.WorkingDir
	}
	env, err := RevealEnv(cfg.Env)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	MergeEnv(cmd, env)
	return cmd, nil
}

// processGroupAlive signals 0 to the negative PID (the process group, not
// the individual PID), so children that outlive the shell are still detected.
func processGroupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}

// processCommand confirms a pid recovered from a pidfile still belongs to
// the expected service -- pids get recycled, so without this check we
// could SIGTERM an unrelated process.
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// commandIdentityMatches reports whether cmdLine's own first field — the
// executable ps reports, never one of its arguments — identifies
// expectCommand, either literally (a full path was configured) or by
// basename (a bare command name was, and the shell or OS resolved it to a
// full path by the time ps saw it). A substring search over the whole line
// would also match expectCommand appearing inside an unrelated argument.
func commandIdentityMatches(cmdLine, expectCommand string) bool {
	if cmdLine == "" || expectCommand == "" {
		return false
	}
	fields := strings.Fields(cmdLine)
	if len(fields) == 0 {
		return false
	}
	got := fields[0]
	return got == expectCommand || filepath.Base(got) == filepath.Base(expectCommand)
}

// ReclaimOrphan kills pid's process group only if it is still alive AND its
// command line still identifies expectCommand. Uses a 2s grace window
// (vs. KillProcessGroup's 1s): reclaim runs at tray startup, where a longer
// wait beats SIGKILLing a daemon mid-shutdown.
func ReclaimOrphan(pid int, expectCommand string) bool {
	if !processGroupAlive(pid) {
		return false
	}
	// Confirm pid is still the leader of its own group before group-killing
	// it (-pid): services are spawned Setpgid (leader PID == PGID), so a
	// recycled PID that is NOT a group leader belongs to an unrelated process.
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return false
	}
	// An empty expectCommand means the config this pidfile was written
	// against carries no command to check identity against — that is "cannot
	// verify", not "skip the check", so it must not reclaim.
	if expectCommand == "" {
		slog.Warn("service: cannot verify orphan identity with no configured command; not reclaiming", "pid", pid)
		return false
	}
	if !commandIdentityMatches(processCommand(pid), expectCommand) {
		return false
	}

	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return true
}
