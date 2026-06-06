//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// setProcessGroup puts the command in its own process group so we can kill the
// entire tree (shell + children) on stop.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGTERM to the process group and waits up to 1 second
// for graceful shutdown before falling back to SIGKILL.
// Checks the process group (not just the shell PID) to avoid orphaning children.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	// Try graceful shutdown first.
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	// Poll for up to 1 second. Check the process group, not just the shell PID,
	// to avoid returning early while children are still running.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still alive -- force kill the entire group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildCommand(config *ServiceConfig) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	fullCmd := shellQuote(config.Command)
	for _, arg := range config.Args {
		fullCmd += " " + shellQuote(arg)
	}

	cmd := exec.Command(shell, "-l", "-c", fullCmd)
	setProcessGroup(cmd)
	if config.WorkingDir != "" {
		cmd.Dir = config.WorkingDir
	}
	mergeEnv(cmd, config.Env)
	return cmd
}

// processGroupAlive checks if the process group led by pid has any living members.
// Uses signal 0 to the negative PID (process group) rather than the individual PID,
// so children that outlive the shell are still detected.
func processGroupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}

// processCommand returns the command line for pid as reported by BSD `ps`,
// or "" if the process does not exist or ps fails. Used to confirm a pid we
// recovered from a pidfile still belongs to the service we expect — pids get
// recycled, so without this check we could SIGTERM an unrelated process.
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// reclaimOrphan terminates the process group led by pid if and only if it is
// still alive AND its command line still references expectCommand. Returns
// true when a live process was signaled (caller logs the reclaim); false on
// stale pidfiles or pid-recycling mismatches.
//
// Mirrors killProcessGroup's SIGTERM → poll → SIGKILL pattern but extends the
// grace window to 2s, since reclaim runs at tray startup where a slightly
// longer wait is preferable to SIGKILLing a daemon mid-shutdown.
func reclaimOrphan(pid int, expectCommand string) bool {
	if !processGroupAlive(pid) {
		return false
	}
	// We SIGTERM the whole process group (-pid), so confirm pid is still the
	// leader of its own group before doing so. Our services are spawned Setpgid
	// (leader PID == PGID); a recycled PID that is NOT a group leader belongs to
	// some other process, and group-killing it would take down unrelated work.
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return false
	}
	if expectCommand != "" && !strings.Contains(processCommand(pid), expectCommand) {
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
