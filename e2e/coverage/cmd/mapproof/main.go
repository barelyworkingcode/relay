// Command mapproof proves that breaking an area's code turns at least one of
// the feature tests selected for it red.
//
//	go run ./coverage/cmd/mapproof --repo DIR (--areas a,b | --all) [--record FILE]
//
// Per area it makes a detached worktree at the repo's HEAD, applies the
// catalogued break (e2e/coverage/proof/breaks.json) as a local commit that is
// never pushed, selects tests with coverage.Select (the code the e2e job
// runs), checks they pass at base and fail on the break, then restores the
// file and compares its sha256 with the original. Exit 0: every area went
// red. Exit 1: an area stayed green or errored. Exit 2: usage error.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"relaye2e/coverage"
)

func main() { os.Exit(run()) }

type result struct {
	brk  coverage.Break
	red  []string
	base string
	err  error
}

func run() int {
	repo := flag.String("repo", "", "repository root (required)")
	areasFlag := flag.String("areas", "", "comma-separated areas to prove")
	all := flag.Bool("all", false, "prove every non-reserved, non-journey-only area")
	record := flag.String("record", "", "file to write the proof record to (docs/map-proof.md)")
	flag.Parse()
	if *repo == "" || (*areasFlag == "") == !*all {
		fmt.Fprintln(os.Stderr, "mapproof: --repo and exactly one of --areas or --all are required")
		return 2
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mapproof:", err)
		return 2
	}
	if err := proveAll(root, *areasFlag, *all, *record); err != nil {
		fmt.Fprintln(os.Stderr, "mapproof:", err)
		return 1
	}
	return 0
}

func proveAll(root, areasFlag string, all bool, record string) error {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(coverage.BreaksPath)))
	if err != nil {
		return err
	}
	var breaks []coverage.Break
	if err := json.Unmarshal(raw, &breaks); err != nil {
		return fmt.Errorf("%s: %w", coverage.BreaksPath, err)
	}
	features, err := os.ReadFile(filepath.Join(root, "docs", "FEATURES.md"))
	if err != nil {
		return err
	}
	am, _ := coverage.ParseAreaMap(string(features))
	var wanted []string
	if all {
		for name := range am.Code {
			if !coverage.IsReserved(name) && !am.JourneyOnly[name] {
				wanted = append(wanted, name)
			}
		}
		sort.Strings(wanted)
	} else {
		wanted = strings.Split(areasFlag, ",")
	}
	byArea := map[string]coverage.Break{}
	for _, b := range breaks {
		byArea[b.Area] = b
	}
	base, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	base = strings.TrimSpace(base)

	var results []result
	failed := 0
	for _, area := range wanted {
		b, ok := byArea[area]
		if !ok {
			results = append(results, result{brk: coverage.Break{Area: area}, base: base, err: fmt.Errorf("no break entry")})
			failed++
			continue
		}
		red, err := proveArea(root, base, b)
		results = append(results, result{brk: b, red: red, base: base, err: err})
		switch {
		case err != nil:
			failed++
			fmt.Printf("ERROR %s: %v\n", area, err)
		default:
			fmt.Printf("RED   %s: %s\n", area, strings.Join(red, ","))
		}
	}
	if record != "" {
		var ok []result
		for _, r := range results {
			if r.err == nil {
				ok = append(ok, r)
			}
		}
		if err := writeRecord(record, ok, am); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d area(s) did not go red", failed)
	}
	return nil
}

// proveArea runs one break in a throwaway worktree and returns the red tests.
func proveArea(root, base string, b coverage.Break) (red []string, err error) {
	tmp, err := os.MkdirTemp("", "mapproof-")
	if err != nil {
		return nil, err
	}
	wt := filepath.Join(tmp, "wt")
	if _, err := git(root, "worktree", "add", "--detach", wt, base); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	defer func() {
		_, _ = git(root, "worktree", "remove", "--force", wt)
		os.RemoveAll(tmp)
	}()

	file := filepath.Join(wt, filepath.FromSlash(b.File))
	orig, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if n := bytes.Count(orig, []byte(b.Find)); n != 1 {
		return nil, fmt.Errorf("%s: find occurs %d times, want exactly 1", b.File, n)
	}
	want := sha256.Sum256(orig)
	broken := bytes.Replace(orig, []byte(b.Find), []byte(b.Replace), 1)
	if err := os.WriteFile(file, broken, 0o644); err != nil {
		return nil, err
	}
	// The commit stays in the throwaway worktree; hooks are off for it only.
	if _, err := git(wt, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.name=mapproof", "-c", "user.email=mapproof@example.invalid",
		"commit", "-q", "-a", "-m", "mapproof break: "+b.Area); err != nil {
		return nil, err
	}
	breakSHA, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	breakSHA = strings.TrimSpace(breakSHA)

	sel, err := coverage.Select(wt, base, breakSHA)
	if err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	if !contains(sel.Areas, b.Area) {
		return nil, fmt.Errorf("%s is not in an area of the selection (%v)", b.File, sel.Areas)
	}
	if sel.Full || len(sel.Tests) == 0 {
		return nil, fmt.Errorf("selection is not an area selection (full=%v, tests=%d)", sel.Full, len(sel.Tests))
	}
	// The map self-test is left out: it checks the map, not the area.
	pattern := coverage.RunPattern(coverage.Selection{Tests: sel.Tests})
	pattern = dropSelfTest(pattern)

	if _, err := git(wt, "checkout", "-q", "--detach", base); err != nil {
		return nil, err
	}
	if out, fails, err := goTest(wt, pattern); err != nil || len(fails) > 0 {
		return nil, fmt.Errorf("area tests not green at base (fails %v, err %v)\n%s", fails, err, tail(out))
	}
	if _, err := git(wt, "checkout", "-q", "--detach", breakSHA); err != nil {
		return nil, err
	}
	out, fails, runErr := goTest(wt, pattern)
	if _, err := git(wt, "checkout", "-q", "--detach", base); err != nil {
		return nil, err
	}
	after, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(after); got != want {
		return nil, fmt.Errorf("%s: sha256 after restore differs from base", b.File)
	}
	if len(fails) == 0 {
		return nil, fmt.Errorf("selected tests stayed green on the break (go test err %v)\n%s", runErr, tail(out))
	}
	return fails, nil
}

func dropSelfTest(pattern string) string {
	pattern = strings.Replace(pattern, "|"+regexp.QuoteMeta(coverage.SelfTest), "", 1)
	return strings.Replace(pattern, regexp.QuoteMeta(coverage.SelfTest)+"|", "", 1)
}

var failLine = regexp.MustCompile(`(?m)^--- FAIL: (\S+)`)

func goTest(wt, pattern string) (string, []string, error) {
	cmd := exec.Command("go", "test", "-race", "-count=1", "-timeout", "35m", "./features", "-run", pattern)
	cmd.Dir = filepath.Join(wt, "e2e")
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	var fails []string
	for _, m := range failLine.FindAllStringSubmatch(string(out), -1) {
		if m[1] != coverage.SelfTest {
			fails = append(fails, m[1])
		}
	}
	sort.Strings(fails)
	return string(out), fails, err
}

// maxRedListed caps the red tests named in a row; the count keeps the rest.
const maxRedListed = 5

func capRed(names []string) string {
	if len(names) <= maxRedListed {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(names[:maxRedListed], ", "), len(names)-maxRedListed)
}

// writeRecord rewrites the proof table: rows of areas proved in this run
// replace their old rows, rows of other areas stay as they were.
func writeRecord(path string, results []result, am coverage.AreaMap) error {
	date, err := exec.Command("date", "+%Y-%m-%d").Output()
	if err != nil {
		return err
	}
	rows := map[string]string{}
	reasons := map[string]string{}
	if old, err := os.ReadFile(path); err == nil {
		inJourney := false
		for _, line := range strings.Split(string(old), "\n") {
			switch {
			case strings.HasPrefix(line, "#"):
				inJourney = strings.Contains(strings.ToLower(line), "journey-only areas")
			case inJourney && strings.HasPrefix(line, "- "):
				name, _, _ := strings.Cut(strings.TrimPrefix(line, "- "), ":")
				reasons[strings.Trim(strings.TrimSpace(name), "`")] = line
			case !inJourney && strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| Area "):
				cells := strings.Split(line, " | ")
				if len(cells) == 6 {
					if names := strings.Split(cells[4], ", "); len(names) > maxRedListed && !strings.Contains(cells[4], "more)") {
						cells[4] = capRed(names)
					}
					rows[strings.TrimPrefix(cells[0], "| ")] = strings.Join(cells, " | ")
				}
			}
		}
	}
	for _, r := range results {
		rows[r.brk.Area] = fmt.Sprintf("| %s | `%s` | %s | %s | %s | %s |", r.brk.Area, r.brk.File,
			strings.ReplaceAll(r.brk.Why, "|", "/"), r.base[:12], capRed(r.red), strings.TrimSpace(string(date)))
	}
	var b strings.Builder
	b.WriteString("# Map proof\n\n")
	b.WriteString("Each row is one catalogued break (`e2e/coverage/proof/breaks.json`) that turned at least one\n")
	b.WriteString("selected feature test red. `go run ./coverage/cmd/mapproof --repo .. --all --record ../docs/map-proof.md`\n")
	b.WriteString("(in `e2e/`) reruns the proof; a run with `--areas` updates only those rows. It selects tests with\n")
	b.WriteString("the same code the `e2e` job uses and leaves out `TestFeatureMapCoverage`, which checks the map\n")
	b.WriteString("and not the area. Rule M5 fails when an area has no break, no row here, or (if `journey-only`)\n")
	b.WriteString("no listing below.\n\n")
	b.WriteString("| Area | File | Break | Base | Red tests | Date |\n|---|---|---|---|---|---|\n")
	var areas []string
	for a := range rows {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		b.WriteString(rows[a] + "\n")
	}
	b.WriteString("\n## Journey-only areas\n\n")
	var names []string
	for n := range am.JourneyOnly {
		if am.JourneyOnly[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("None are marked.\n")
	}
	for _, n := range names {
		if kept, ok := reasons[n]; ok {
			b.WriteString(kept + "\n")
			continue
		}
		fmt.Fprintf(&b, "- %s: no headless e2e test can turn this area red; only the epic-close full verify covers it.\n", n)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// cleanEnv drops GIT_* so a hook environment cannot point git at another repo.
func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return env
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	return strings.Join(lines, "\n")
}
