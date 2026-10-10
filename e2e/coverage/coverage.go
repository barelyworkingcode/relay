package coverage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one miss. Rule is R1..R9, Q6, H1..H3, syntax or pending; the
// rule "info" is a note and never a failure.
type Finding struct {
	Rule  string
	Where string // row ID, door, file:line or test name
	Msg   string
}

func (f Finding) String() string { return fmt.Sprintf("%s %s: %s", f.Rule, f.Where, f.Msg) }

// SelfTest is the one feature test exempt from R4: it checks the map and
// proves no row.
const SelfTest = "TestFeatureMapCoverage"

// Failures drops the informational findings.
func Failures(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Rule != "info" {
			out = append(out, f)
		}
	}
	return out
}

// maxPending is the ceiling of pending.txt. The file only shrinks, so a
// number outside this set is a gate that was widened.
var maxPending = map[int]bool{289: true, 290: true, 291: true, 292: true}

// Check runs every rule against the repository at repoRoot and the door list
// of a live test-build instance. The error is for unreadable or unparseable
// inputs only; a rule miss is a Finding.
func Check(repoRoot string, doors DoorsDoc) ([]Finding, error) {
	if doors.Schema != 1 {
		return nil, fmt.Errorf("doors document schema is %d, expected 1", doors.Schema)
	}
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", rel, err)
		}
		return string(b), nil
	}
	features, err := read("docs/FEATURES.md")
	if err != nil {
		return nil, err
	}
	events, err := read("docs/events.md")
	if err != nil {
		return nil, err
	}
	routes, err := read("docs/routes.md")
	if err != nil {
		return nil, err
	}
	cli, err := read("docs/cli.md")
	if err != nil {
		return nil, err
	}
	pendingText, err := read("e2e/coverage/pending.txt")
	if err != nil {
		return nil, err
	}
	pending, pfs, err := parsePending(pendingText)
	if err != nil {
		return nil, err
	}
	src, err := scanGo(filepath.Join(repoRoot, "e2e"))
	if err != nil {
		return nil, err
	}
	eventKeys, err := eventCatalogue(events)
	if err != nil {
		return nil, err
	}

	fm, fs := parseFeatureMap(features)
	fs = append(fs, pfs...)
	x := &checker{
		root: repoRoot, doors: doors, fm: fm, pending: pending,
		events: eventKeys, tests: src.featureTests, byRef: map[string]Door{},
	}
	for _, d := range doors.Doors {
		x.byRef[d.Ref()] = d
	}
	fs = append(fs, x.rules()...)
	fs = append(fs, referenceHeadings(doors, routes, cli)...)
	fs = append(fs, src.findings...)

	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Rule != fs[j].Rule {
			return fs[i].Rule < fs[j].Rule
		}
		if fs[i].Where != fs[j].Where {
			return fs[i].Where < fs[j].Where
		}
		return fs[i].Msg < fs[j].Msg
	})
	return fs, nil
}

// referenceHeadings is Q6: every http door has a heading in routes.md whose
// text is the door name, every cli door a heading in cli.md that carries the
// name, with or without its leading "relay ".
func referenceHeadings(doors DoorsDoc, routes, cli string) []Finding {
	routeHeads := map[string]bool{}
	for _, h := range headings(routes) {
		routeHeads[plain(h.Text)] = true
	}
	cliHeads := map[string]bool{}
	for _, h := range headings(cli) {
		cliHeads[plain(h.Text)] = true
		for _, s := range codeSpans(h.Text) {
			cliHeads[s] = true
		}
	}
	var fs []Finding
	for _, d := range doors.Doors {
		switch d.Kind {
		case "http":
			if !routeHeads[d.Name] {
				fs = append(fs, Finding{"Q6", d.Ref(), "docs/routes.md has no heading with this door name"})
			}
		case "cli":
			if !cliHeads[d.Name] && !cliHeads[strings.TrimPrefix(d.Name, "relay ")] {
				fs = append(fs, Finding{"Q6", d.Ref(), "docs/cli.md has no heading naming this verb"})
			}
		}
	}
	return fs
}

// parsePending reads pending.txt: one issue number per line, # comments.
func parsePending(text string) (map[int]bool, []Finding, error) {
	live := map[int]bool{}
	var fs []Finding
	for n, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var num int
		if _, err := fmt.Sscanf(line, "%d", &num); err != nil || fmt.Sprint(num) != line {
			return nil, nil, fmt.Errorf("pending.txt line %d: %q is not an issue number", n+1, line)
		}
		if !maxPending[num] {
			fs = append(fs, Finding{"pending", fmt.Sprintf("pending.txt:%d", n+1), fmt.Sprintf("#%d may not be listed: the list only shrinks from 289, 290, 291, 292", num)})
			continue
		}
		live[num] = true
	}
	return live, fs, nil
}

// eventCatalogue returns the first-column code spans of every table between
// the "## 7." heading and the next level-2 heading of docs/events.md.
func eventCatalogue(doc string) (map[string]bool, error) {
	keys := map[string]bool{}
	in := false
	inFence := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if lvl, txt, ok := heading(line); ok && lvl == 2 {
			in = strings.HasPrefix(txt, "7.")
			continue
		}
		if !in {
			continue
		}
		cells, ok := splitRow(line)
		if !ok || len(cells) == 0 || isSeparatorRow(cells) {
			continue
		}
		for _, s := range codeSpans(cells[0]) {
			keys[s] = true
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("docs/events.md has no event keys between the \"## 7.\" heading and the next level-2 heading")
	}
	return keys, nil
}
