//go:build !windows

package main

import (
	"os"
	"os/exec"
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
