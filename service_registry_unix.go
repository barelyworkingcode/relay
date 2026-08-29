//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGTERMs the group (not just the shell PID, so children
// that outlive the shell are also caught), waits 1s, then SIGKILLs.
func killProcessGroup(cmd *exec.Cmd) {
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildCommand(config *ServiceConfig) (*exec.Cmd, error) {
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
	env, err := revealEnvOrErr(config.Env)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	mergeEnv(cmd, env)
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

// reclaimOrphan kills pid's process group only if it is still alive AND its
// command line still references expectCommand. Uses a 2s grace window
// (vs. killProcessGroup's 1s): reclaim runs at tray startup, where a longer
// wait beats SIGKILLing a daemon mid-shutdown.
func reclaimOrphan(pid int, expectCommand string) bool {
	if !processGroupAlive(pid) {
		return false
	}
	// Confirm pid is still the leader of its own group before group-killing
	// it (-pid): services are spawned Setpgid (leader PID == PGID), so a
	// recycled PID that is NOT a group leader belongs to an unrelated process.
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
