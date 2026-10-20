// Command fakeoutside stands in for a Claude Code process a person started in
// a terminal. A test runs it under the base name claude, in a working folder,
// and plants the registry entry and transcript files itself.
//
// It prints "ready <pid>" once its handlers are installed, and
// "signal INT|TERM <unix ns>" for each such signal it receives. It exits at the
// first INT or TERM unless that signal is named in --ignore (a comma list); a
// KILL cannot be caught. On SIGUSR1 it starts one child (sleep) and prints
// "child <pid>"; on SIGUSR2 it starts one child that execs
// /usr/bin/caffeinate -i -t 300, as a terminal Claude does at a turn, and
// prints "child <pid>". Every line is a single write to stdout.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }

func run() int {
	ignore := flag.String("ignore", "", "comma list of INT and TERM to survive")
	flag.Parse()
	ignored := map[string]bool{}
	for _, n := range strings.Split(*ignore, ",") {
		if n = strings.TrimSpace(strings.ToUpper(n)); n != "" {
			ignored[n] = true
		}
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	fmt.Fprintf(os.Stdout, "ready %d\n", os.Getpid())

	for s := range sigs {
		switch s {
		case syscall.SIGUSR1, syscall.SIGUSR2:
			child := exec.Command("sleep", "600")
			if s == syscall.SIGUSR2 {
				child = exec.Command("/usr/bin/caffeinate", "-i", "-t", "300")
			}
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "fakeoutside: child:", err)
				continue
			}
			go func() { _ = child.Wait() }()
			fmt.Fprintf(os.Stdout, "child %d\n", child.Process.Pid)
		default:
			name := "INT"
			if s == syscall.SIGTERM {
				name = "TERM"
			}
			fmt.Fprintf(os.Stdout, "signal %s %d\n", name, time.Now().UnixNano())
			if !ignored[name] {
				return 0
			}
		}
	}
	return 0
}
