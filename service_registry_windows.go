//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

func setProcessGroup(cmd *exec.Cmd) {
	// TODO: Windows process group setup.
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func shellQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func buildCommand(config *ServiceConfig) *exec.Cmd {
	fullCmd := shellQuote(config.Command)
	for _, arg := range config.Args {
		fullCmd += " " + shellQuote(arg)
	}

	cmd := exec.Command("cmd.exe", "/c", fullCmd)
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
	// Stub: Windows process liveness check not yet implemented.
	return false
}
