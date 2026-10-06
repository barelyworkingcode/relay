// Command burnin repeats the Go test functions a pull request adds or changes
// under the race detector, so a flaky new test fails on its own PR.
//
// Usage: go run ./cmd/burnin [-list] <base> <head>, from the repository top
// level. -list prints the selection without running it.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("burnin", flag.ContinueOnError)
	list := fs.Bool("list", false, "print the selected tests and do not run them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		return usage()
	}
	base, head := fs.Arg(0), fs.Arg(1)
	for _, sha := range []string{base, head} {
		if _, err := git(".", "rev-parse", "--verify", sha+"^{commit}"); err != nil {
			fmt.Fprintf(os.Stderr, "burn-in: %s is not a commit\n", sha)
			return usage()
		}
	}

	pkgs, excluded, err := selectTests(".", base, head)
	if err != nil {
		fmt.Printf("::error::burn-in: %v\n", err)
		return 1
	}

	if *list {
		for _, p := range pkgs {
			fmt.Printf("%s\t%s\n", p.ImportPath, runPattern(p))
		}
		if excluded > 0 {
			fmt.Fprintf(os.Stderr, "burn-in: %d changed _test.go file(s) outside the default test build, not run\n", excluded)
		}
		return 0
	}

	if len(pkgs) == 0 {
		fmt.Println("burn-in: no added or changed TestXxx functions; nothing to run")
		return 0
	}

	var failed []PackageTests
	for _, p := range pkgs {
		args := goTestArgs(p)
		fmt.Printf("burn-in: go %s\n", strings.Join(args, " "))
		cmd := exec.Command("go", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			failed = append(failed, p)
		}
	}
	for _, p := range failed {
		fmt.Printf("::error::burn-in: %s failed under go test -race -count=10 -run %s\n", p.ImportPath, runPattern(p))
	}
	if len(failed) > 0 {
		return 1
	}
	return 0
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: go run ./cmd/burnin [-list] <base> <head>")
	return 2
}

func runPattern(p PackageTests) string {
	return "^(" + strings.Join(p.Tests, "|") + ")$"
}

// goTestArgs adds no -p, -cpu or -shuffle: the burn-in repeats tests, it does
// not load the runner. One invocation per package keeps a name shared with an
// unchanged test elsewhere from selecting it.
func goTestArgs(p PackageTests) []string {
	return []string{"test", "-race", "-count=10", "-timeout", "20m", "-run", runPattern(p), p.ImportPath}
}
