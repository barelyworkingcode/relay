// Command testtarget is a real spawnable binary the shim's hermetic tests
// use as the "thing being launched" — a claude/pi/shell stand-in — without
// exercising any real provider. It never talks to relay; it only proves
// what the shim did to its process (fd table, pgid, signals, exit code) by
// writing simple markers a test can assert against on disk.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	markerPath := flag.String("marker", "", "write a JSON marker file here on start, proving the process actually ran")
	exitCode := flag.Int("exit-code", 0, "exit with this code once told to stop")
	sleepFor := flag.Duration("sleep", 0, "sleep this long before exiting on its own (0 = wait for a signal)")
	signalMarker := flag.String("signal-marker", "", "on receiving a forwarded signal, append its name to this file and exit")
	fd3Check := flag.String("fd3-check", "", "write EBADF|open to this file describing whether fd 3 is open in this process")
	flag.Parse()

	if *fd3Check != "" {
		state := "open"
		if !fdOpen(3) {
			state = "EBADF"
		}
		_ = os.WriteFile(*fd3Check, []byte(state), 0o600)
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

func getpgrp() int {
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		return -1
	}
	return pgid
}
