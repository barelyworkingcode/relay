//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

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
