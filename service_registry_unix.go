//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// setProcessGroup puts the command in its own process group so we can kill the
// entire tree (shell + children) on stop.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the entire process group rooted at cmd.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// Negative PID kills the process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildCommand(config *ServiceConfig) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
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
	for k, v := range config.Env {
		cmd.Env = append(cmd.Environ(), k+"="+v)
	}
	return cmd
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
