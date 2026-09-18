// Command testtarget is a real spawnable binary the shim's hermetic tests
// use as the "thing being launched" — a claude/pi/shell stand-in — without
// exercising any real provider. Most of what it does proves what the shim
// did to its process (fd table, pgid, signals, exit code) by writing simple
// markers a test can assert against on disk; -calltool-out is the one
// exception, a real, tokenless internal/bridge call this process (or a
// descendant it spawns) makes for itself, so a session-host integration test
// can prove C3 membership from a genuine, independently-spawned process
// rather than from a value any test asserted.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func main() {
	markerPath := flag.String("marker", "", "write a JSON marker file here on start, proving the process actually ran")
	exitCode := flag.Int("exit-code", 0, "exit with this code once told to stop")
	sleepFor := flag.Duration("sleep", 0, "sleep this long before exiting on its own (0 = wait for a signal)")
	signalMarker := flag.String("signal-marker", "", "on receiving a forwarded signal, append its name to this file and exit")
	fd3Check := flag.String("fd3-check", "", "write EBADF|open to this file describing whether fd 3 is open in this process")
	writeOutside := flag.String("write-outside", "", "attempt to create this file; -write-outside-result records whether the OS allowed it")
	writeOutsideResult := flag.String("write-outside-result", "", "write allowed|denied:<error> here after the -write-outside attempt")
	callToolOut := flag.String("calltool-out", "", "make a real, tokenless bridge.Client.CallTool request and write the outcome here as JSON")
	callToolName := flag.String("calltool-name", "echo", "tool name to call for -calltool-out")
	callToolViaChild := flag.Bool("calltool-via-child", false, "spawn one ordinary child to perform -calltool-out, adding one ancestry hop")
	envOut := flag.String("env-out", "", "write this process's environment here as a JSON object")
	detachBeforeCallTool := flag.Bool("detach-before-calltool", false, "double-fork and setsid before -calltool-out, then exit immediately; the detached descendant makes the call after reparenting")
	flag.Parse()

	if *fd3Check != "" {
		state := "open"
		if !fdOpen(3) {
			state = "EBADF"
		}
		_ = os.WriteFile(*fd3Check, []byte(state), 0o600)
	}

	if *envOut != "" {
		env := map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
		b, _ := json.Marshal(env)
		if err := os.WriteFile(*envOut, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "testtarget: write env:", err)
			os.Exit(1)
		}
	}

	if *writeOutside != "" {
		result := "allowed"
		if err := os.WriteFile(*writeOutside, []byte("sandbox probe"), 0o600); err != nil {
			result = "denied:" + err.Error()
		}
		if *writeOutsideResult != "" {
			_ = os.WriteFile(*writeOutsideResult, []byte(result), 0o600)
		}
	}

	if *markerPath != "" {
		info := map[string]any{
			"pid":  os.Getpid(),
			"ppid": os.Getppid(),
			"pgid": getpgrp(),
		}
		b, _ := json.Marshal(info)
		if err := os.WriteFile(*markerPath, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "testtarget: write marker:", err)
			os.Exit(1)
		}
	}

	if *callToolOut != "" {
		runCallTool(*callToolOut, *callToolName, *callToolViaChild, *detachBeforeCallTool)
	}

	if *signalMarker != "" {
		sigCh := make(chan os.Signal, 8)
		signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			sig := <-sigCh
			_ = os.WriteFile(*signalMarker, []byte(sig.String()), 0o600)
			os.Exit(*exitCode)
		}()
	}

	if *sleepFor > 0 {
		time.Sleep(*sleepFor)
		os.Exit(*exitCode)
	}

	// No timer and no signal marker armed: block forever so a test can
	// signal or kill this process explicitly. This is deliberately a sleep
	// loop, not `select {}` or a bare channel receive: with no other
	// goroutine runnable, either of those trips Go's runtime deadlock
	// detector ("fatal error: all goroutines are asleep - deadlock!"),
	// which exits the process almost immediately — silently turning every
	// "block until killed" test into a false pass. A timer-based sleep is
	// never considered a deadlock.
	for {
		time.Sleep(time.Hour)
	}
}

// fdOpen reports whether fd is a valid, open descriptor in this process,
// using fcntl(F_GETFD) so it works for pipes as well as regular files (unlike
// os.NewFile+Stat, which can't distinguish "closed" from "never opened").
func fdOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// envCallToolWaitReparent mirrors membership_auth_darwin_test.go's own
// bridgeCallerDetachEnv: set on a double-forked, setsid'd child so it waits
// to actually be reparented to launchd before it dials the bridge, rather
// than racing the parent's own exit.
const envCallToolWaitReparent = "TESTTARGET_CALLTOOL_WAIT_REPARENT"

type callToolResult struct {
	PID    int             `json:"pid"`
	PPID   int             `json:"ppid"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func writeCallToolResult(path string, r callToolResult) {
	b, _ := json.Marshal(r)
	_ = os.WriteFile(path, b, 0o600)
}

// runCallTool makes a real, tokenless bridge.Client.CallTool request —
// admitted, if at all, purely by the kernel's account of this process's
// ancestry (C3) — and writes the outcome to out. viaChild inserts one
// ordinary ancestry hop before the call; detach severs ancestry first (one
// fork plus setsid, with this process exiting immediately after starting
// it, SP5's shape) so the eventual caller is a real process the kernel no
// longer places under the session root at all.
func runCallTool(out, tool string, viaChild, detach bool) {
	switch {
	case detach:
		cmd := exec.Command(os.Args[0], "-calltool-out", out, "-calltool-name", tool)
		cmd.Env = append(os.Environ(), envCallToolWaitReparent+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			writeCallToolResult(out, callToolResult{Error: "start detached caller: " + err.Error()})
		}
		os.Exit(0)
	case viaChild:
		cmd := exec.Command(os.Args[0], "-calltool-out", out, "-calltool-name", tool)
		cmd.Stderr = os.Stderr
		_ = cmd.Run() // the child writes its own result file regardless of its exit code
		return
	}

	if os.Getenv(envCallToolWaitReparent) == "1" {
		deadline := time.Now().Add(10 * time.Second)
		for os.Getppid() != 1 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}

	result := callToolResult{PID: os.Getpid(), PPID: os.Getppid()}
	sock := os.Getenv("RELAY_BRIDGE_SOCKET")
	if sock == "" {
		result.Error = "RELAY_BRIDGE_SOCKET is not set"
	} else if raw, err := bridge.NewClientAt(sock, "").CallTool(tool, json.RawMessage(`{}`)); err != nil {
		result.Error = err.Error()
	} else {
		result.OK = true
		result.Result = raw
	}
	writeCallToolResult(out, result)
}

func getpgrp() int {
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		return -1
	}
	return pgid
}
