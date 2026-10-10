// Command selecttests picks the e2e feature tests a change reaches and prints
// the -run pattern for them. It runs the map rules first.
//
//	go run ./coverage/cmd/selecttests --repo DIR (--base REV [--head REV] | --full)
//	    [--labels-json JSON] [--env FILE] [--summary FILE]
//
// Exit 0: selected. Exit 1: a map finding, an unmapped code path, or a
// narrowing without the map-narrowing-approved label. Exit 2: usage or git
// error. No error ever turns into a full run.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"relaye2e/coverage"
)

const approvedLabel = "map-narrowing-approved"

func main() { os.Exit(run()) }

func run() int {
	repo := flag.String("repo", "", "repository root (required)")
	base := flag.String("base", "", "base revision of the change")
	head := flag.String("head", "HEAD", "head revision of the change")
	full := flag.Bool("full", false, "select every feature test")
	labels := flag.String("labels-json", "", "JSON array of the PR's label names")
	envFile := flag.String("env", "", "file to append SELECT_RUN=<pattern> to")
	summary := flag.String("summary", "", "file to write the job summary to")
	flag.Parse()
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "selecttests: "+format+"\n", a...)
		return code
	}
	if *repo == "" || (*base == "") == !*full {
		return fail(2, "--repo and exactly one of --base or --full are required")
	}
	approved := false
	if strings.TrimSpace(*labels) != "" {
		var names []string
		if err := json.Unmarshal([]byte(*labels), &names); err != nil {
			return fail(2, "--labels-json: %v", err)
		}
		for _, n := range names {
			approved = approved || n == approvedLabel
		}
	}

	findings, err := coverage.CheckMap(*repo)
	if err != nil {
		return fail(2, "%v", err)
	}
	if failures := coverage.Failures(findings); len(failures) > 0 {
		for _, f := range failures {
			fmt.Println(f)
		}
		return 1
	}

	var sel coverage.Selection
	if *full {
		sel, err = coverage.SelectFull(*repo, *head)
	} else {
		sel, err = coverage.Select(*repo, *base, *head)
	}
	if err != nil {
		return fail(2, "%v", err)
	}

	fmt.Println("AREAS " + strings.Join(sel.Areas, ","))
	fmt.Printf("TESTS %d/%d\n", len(sel.Tests), sel.Total)
	for _, p := range sel.Unmapped {
		fmt.Println("UNMAPPED " + p)
	}
	paths := make([]string, 0, len(sel.Narrowed))
	for p := range sel.Narrowed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Printf("NARROWED %s %s\n", p, strings.Join(sel.Narrowed[p], ","))
	}
	pattern := coverage.RunPattern(sel)
	code := 0
	if len(sel.Unmapped) > 0 || (len(sel.Narrowed) > 0 && !approved) {
		code = 1
	} else {
		fmt.Println("RUN " + pattern)
	}

	if *summary != "" {
		if err := appendTo(*summary, summaryText(sel)); err != nil {
			return fail(2, "writing summary: %v", err)
		}
	}
	if *envFile != "" && code == 0 {
		if err := appendTo(*envFile, "SELECT_RUN="+pattern+"\n"); err != nil {
			return fail(2, "writing env file: %v", err)
		}
	}
	return code
}

func summaryText(sel coverage.Selection) string {
	var b strings.Builder
	n := len(sel.Tests)
	fmt.Fprintf(&b, "### e2e test selection\n\nselected %d of %d\n\n", n, sel.Total)
	if sel.Full {
		b.WriteString("Full run: every feature test.\n\n")
	}
	fmt.Fprintf(&b, "Affected areas: %s\n\n", orNone(strings.Join(sel.Areas, ", ")))
	b.WriteString("Tests run:\n\n")
	for _, t := range sel.Tests {
		b.WriteString("- " + t + "\n")
	}
	fmt.Fprintf(&b, "\nNot run: %d\n", sel.Total-n)
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func appendTo(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
