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
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	// Try graceful shutdown first.
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	// Poll for up to 1 second.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still alive -- force kill.
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
	if len(config.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range config.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
