package coverage

import (
	"fmt"
	"strings"
)

var featureHeader = []string{"ID", "Feature", "Claim", "Simple door", "Power door", "Doors", "Gate", "Proof", "Test"}
var promiseHeader = []string{"Promise", "Attacker", "Asset", "Promise (short quote)", "Rows"}

// Row is one parsed feature row.
type Row struct {
	ID    string
	Line  int
	Power string
	Doors []DoorRef
	Gate  []string
	Proof []Proof
	Tests []TestItem
}

// Exception reports whether the Power door cell is an `exception:` entry.
func (r Row) Exception() bool { return strings.HasPrefix(plain(r.Power), "exception:") }

// Promise is one row of the threat-model promise table.
type Promise struct {
	ID   string
	Line int
	Rows []string
}

// FeatureMap is the parsed docs/FEATURES.md.
type FeatureMap struct {
	Rows     []Row
	Promises []Promise
	Retired  map[string]bool
	CIRows   map[string]bool
}

// parseFeatureMap reads every table with the exact feature header, the
// promise table, the retired IDs and the CI row list. Findings are syntax
// faults; the rules run afterwards on what parsed.
func parseFeatureMap(doc string) (FeatureMap, []Finding) {
	fm := FeatureMap{Retired: map[string]bool{}, CIRows: map[string]bool{}}
	var fs []Finding
	lines := strings.Split(doc, "\n")
	inFence := false
	section := ""    // normalised text of the current heading
	paragraph := ""  // "retired" or "ci" while a keyed paragraph runs
	var table string // "", "feature", "promise"
	headerSeen := 0  // 0 none, 1 header read (separator expected next)
	for n, line := range lines {
		ln := n + 1
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			table = ""
			continue
		}
		if inFence {
			continue
		}
		if _, txt, ok := heading(line); ok {
			section = strings.ToLower(plain(txt))
			paragraph = ""
			table = ""
			continue
		}
		trimmed := strings.TrimSpace(plain(strings.ReplaceAll(line, "**", "")))
		low := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(low, "rows proven by ci"):
			paragraph = "ci"
		case strings.HasPrefix(low, "retired ids"):
			paragraph = "retired"
		case trimmed == "":
			paragraph = ""
		}
		if strings.HasPrefix(section, "retired ids") && trimmed != "" {
			paragraph = "retired"
		}
		if strings.HasPrefix(section, "rows proven by ci") && trimmed != "" {
			paragraph = "ci"
		}
		cells, isRow := splitRow(line)
		if !isRow {
			table = ""
			headerSeen = 0
			switch paragraph {
			case "retired":
				for _, s := range codeSpans(line) {
					fm.Retired[s] = true
				}
			case "ci":
				for _, s := range codeSpans(line) {
					if idPattern.MatchString(s) {
						fm.CIRows[s] = true
					}
				}
			}
			continue
		}
		switch {
		case table == "" && sameCells(cells, featureHeader):
			table, headerSeen = "feature", 1
		case table == "" && sameCells(cells, promiseHeader):
			table, headerSeen = "promise", 1
		case table != "" && headerSeen == 1:
			headerSeen = 2
			if !isSeparatorRow(cells) {
				fs = append(fs, Finding{"syntax", fmt.Sprintf("FEATURES.md:%d", ln), "the header row is not followed by a separator row"})
			}
		case table == "feature":
			row, rfs := parseRow(cells, ln)
			fm.Rows = append(fm.Rows, row)
			fs = append(fs, rfs...)
		case table == "promise":
			if len(cells) != len(promiseHeader) {
				fs = append(fs, Finding{"syntax", fmt.Sprintf("FEATURES.md:%d", ln), fmt.Sprintf("promise row has %d cells, expected %d", len(cells), len(promiseHeader))})
				continue
			}
			fm.Promises = append(fm.Promises, Promise{ID: plain(cells[0]), Line: ln, Rows: codeSpans(cells[4])})
		}
	}
	return fm, fs
}

func sameCells(cells, want []string) bool {
	if len(cells) != len(want) {
		return false
	}
	for i := range cells {
		if strings.TrimSpace(plain(cells[i])) != want[i] {
			return false
		}
	}
	return true
}

func parseRow(cells []string, ln int) (Row, []Finding) {
	var fs []Finding
	where := fmt.Sprintf("FEATURES.md:%d", ln)
	row := Row{Line: ln}
	if len(cells) != len(featureHeader) {
		return row, []Finding{{"syntax", where, fmt.Sprintf("row has %d cells, expected %d", len(cells), len(featureHeader))}}
	}
	if ids := codeSpans(cells[0]); len(ids) == 1 {
		row.ID = ids[0]
	} else {
		row.ID = plain(cells[0])
	}
	where = fmt.Sprintf("%s (FEATURES.md:%d)", row.ID, ln)
	row.Power = cells[4]
	bad := func(col, msg string) { fs = append(fs, Finding{"syntax", where, col + ": " + msg}) }

	for _, s := range codeSpans(cells[5]) {
		ref, err := parseDoorRef(s)
		if err != nil {
			bad("Doors", err.Error())
			continue
		}
		row.Doors = append(row.Doors, ref)
	}
	if len(row.Doors) == 0 && plain(cells[5]) != "none" {
		bad("Doors", `must be "none" or one or more code spans`)
	}
	for _, s := range codeSpans(cells[6]) {
		if s != "webauthn" && !gateRe.MatchString(s) {
			bad("Gate", fmt.Sprintf("%q is not a gate name", s))
			continue
		}
		row.Gate = append(row.Gate, s)
	}
	if len(row.Gate) == 0 && plain(cells[6]) != "none" {
		bad("Gate", `must be "none" or one or more code spans`)
	}
	for _, s := range codeSpans(cells[7]) {
		p, err := parseProof(s)
		if err != nil {
			bad("Proof", err.Error())
			continue
		}
		row.Proof = append(row.Proof, p)
	}
	if len(codeSpans(cells[7])) == 0 {
		bad("Proof", "needs at least one code span")
	}
	for _, s := range codeSpans(cells[8]) {
		t, err := parseTestItem(s)
		if err != nil {
			bad("Test", err.Error())
			continue
		}
		row.Tests = append(row.Tests, t)
	}
	if len(codeSpans(cells[8])) == 0 {
		bad("Test", "needs at least one code span; a row with no test yet reads pending:#N")
	}
	return row, fs
}
