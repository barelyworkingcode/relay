// Command testmembership is a real spawnable binary the darwin membership
// tests use to build actual process trees — direct child, grandchild, and a
// double-forked detachment — without mocking exec.Command. It knows nothing
// about relay's bridge or identities; it only shapes ancestry.
//
// Built on demand by TestMain in the membership package's darwin integration
// test, following cmd/testservice's on-demand-build pattern.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	mode := flag.String("mode", "chain", "chain|doublefork")
	pidfiles := flag.String("pidfiles", "", "chain mode: comma-separated pidfile path chain, this process first")
	pidfile := flag.String("pidfile", "", "doublefork mode: pidfile for the detached target")
	flag.Parse()

	switch *mode {
	case "chain":
		runChain(strings.Split(*pidfiles, ","))
	case "doublefork":
		runDoubleFork(*pidfile)
	default:
		fmt.Fprintf(os.Stderr, "testmembership: unknown -mode %q\n", *mode)
		os.Exit(2)
	}
}

// runChain writes its own pid to files[0], and if more files remain, spawns
// a further copy of itself as a genuine child to write the rest — building
// an ancestry chain one real fork+exec at a time. Every level blocks on a
// signal afterward rather than exiting, so the whole chain stays alive (and
// its pids and start times stay valid) for as long as the test needs it.
func runChain(files []string) {
	if len(files) == 0 || files[0] == "" {
		fmt.Fprintln(os.Stderr, "testmembership: chain mode requires -pidfiles")
		os.Exit(2)
	}
	writePID(files[0])
	if len(files) > 1 {
		child := exec.Command(mustExecutable(), "-mode=chain", "-pidfiles="+strings.Join(files[1:], ","))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "testmembership: spawn child:", err)
			os.Exit(1)
		}
		// Deliberately not Wait()ed on: this process must keep running (and
		// stay the child's parent) for the ancestry chain to hold, so the
		// child is intentionally left running past this function's return.
	}
	block()
}

// runDoubleFork is the SP5 double-fork idiom, done in-process instead of
// shelling out: this process becomes a session leader (so the detached
// target below it has no controlling terminal to inherit), spawns the
// target as an ordinary child, and then exits immediately. Once it exits,
// the target's ppid becomes 1 (launchd reparents it) rather than staying a
// child of a live process — the same shape a double-forked daemon leaves
// behind in the real world.
func runDoubleFork(pidfile string) {
	if pidfile == "" {
		fmt.Fprintln(os.Stderr, "testmembership: doublefork mode requires -pidfile")
		os.Exit(2)
	}
	if _, err := syscall.Setsid(); err != nil {
		fmt.Fprintln(os.Stderr, "testmembership: setsid:", err)
		os.Exit(1)
	}
	target := exec.Command(mustExecutable(), "-mode=chain", "-pidfiles="+pidfile)
	target.Stdout, target.Stderr = os.Stdout, os.Stderr
	if err := target.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "testmembership: spawn detached target:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func writePID(path string) {
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "testmembership: write pidfile:", err)
		os.Exit(1)
	}
}

func block() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
}

// mustExecutable re-execs the same binary os.Args[0] named, which os/exec
// resolves via PATH lookup rules that don't apply to a path already
// absolute — the test harness always builds and invokes this binary by
// absolute path, so os.Args[0] is safe to reuse directly.
func mustExecutable() string { return os.Args[0] }
