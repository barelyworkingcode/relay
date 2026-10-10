package coverage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type checker struct {
	root    string
	doors   DoorsDoc
	fm      FeatureMap
	pending map[int]bool
	events  map[string]bool
	tests   map[string]bool
	byRef   map[string]Door
}

func (x *checker) rules() []Finding {
	var fs []Finding
	add := func(rule, where, format string, a ...any) {
		fs = append(fs, Finding{rule, where, fmt.Sprintf(format, a...)})
	}

	// R7: IDs.
	seen := map[string]bool{}
	rows := map[string]Row{}
	for _, r := range x.fm.Rows {
		where := fmt.Sprintf("%s (FEATURES.md:%d)", r.ID, r.Line)
		switch {
		case !idPattern.MatchString(r.ID):
			add("R7", where, "ID does not match %s", idPattern)
		case seen[r.ID]:
			add("R7", where, "ID is used by another row")
		case x.fm.Retired[r.ID]:
			add("R7", where, "ID is on the Retired IDs line")
		}
		seen[r.ID] = true
		rows[r.ID] = r
	}

	covered := map[string]bool{}
	for _, r := range x.fm.Rows {
		where := r.ID
		if len(plain(r.Power)) == 0 {
			add("syntax", where, "Power door is empty; write the door, n/a or exception: <why>")
		}
		isCI := x.fm.CIRows[r.ID]
		for _, d := range r.Doors {
			covered[d.String()] = true
		}

		// R2: every catalogue ref names a door that exists.
		for _, ref := range x.catRefs(r) {
			if _, ok := x.byRef[ref.String()]; !ok {
				add("R2", where, "%s is not in relay doors", ref)
			}
		}

		x.testRules(&fs, r, isCI)

		// R5, R6, R8's input, R9.
		livePending := false
		for _, t := range r.Tests {
			if t.Kind == "pending" && x.pending[t.Pending] {
				livePending = true
			}
		}
		for _, p := range r.Proof {
			if p.Kind == "event" && !x.events[p.Key] {
				add("R6", where, "event %q is not in docs/events.md section 7", p.Key)
			}
		}
		if !isCI {
			observes := false
			for _, p := range r.Proof {
				observes = observes || p.Observes()
			}
			if !observes {
				add("R9", where, "Proof has no event:, audit: or out: item")
			}
		}
		if len(r.Gate) == 0 {
			for _, d := range r.Doors {
				if door, ok := x.byRef[d.String()]; ok && door.OwnerGated {
					add("R5", where, "Gate is none but door %s is owner-gated (%s)", d, strings.Join(door.Gates, ", "))
				}
			}
		}
		if len(r.Gate) > 0 {
			if !hasRefusal(r) {
				add("R5", where, "gated row has no refusal proof (event or audit =denied, code 401/403 or a non-zero exit)")
			}
			if !livePending {
				for _, d := range r.Doors {
					door, ok := x.byRef[d.String()]
					if !ok || !door.OwnerGated || (d.Kind != "http" && d.Kind != "cli") {
						continue
					}
					if !hasDeny(r, d) {
						add("R5", where, "gated door %s has no deny test item (deny e2e:TestX@%s)", d, d)
					}
				}
			}
		}
	}

	// R1: every catalogue door is named by a row.
	for _, d := range x.doors.Doors {
		if !covered[d.Ref()] {
			add("R1", d.Ref(), "no feature row names this door")
		}
	}

	// R4: tests and e2e items agree.
	named := map[string]bool{}
	for _, r := range x.fm.Rows {
		for _, t := range r.Tests {
			if t.Kind == "e2e" {
				named[t.Name] = true
			}
		}
	}
	for name := range named {
		if !x.tests[name] {
			add("R4", name, "an e2e: item names a test function that does not exist in e2e/features")
		}
	}
	var orphans []string
	for name := range x.tests {
		if name != SelfTest && !named[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		add("R4", name, "no feature row names this test")
	}

	// R8: every promise maps to rows with a refusal proof.
	for _, p := range x.fm.Promises {
		where := fmt.Sprintf("%s (FEATURES.md:%d)", p.ID, p.Line)
		if !promiseIDRe.MatchString(p.ID) {
			add("R8", where, "promise ID does not match %s", promiseIDRe)
		}
		ok := false
		for _, id := range p.Rows {
			r, found := rows[id]
			if !found {
				add("R8", where, "names row %s, which does not exist", id)
				continue
			}
			ok = ok || hasRefusal(r)
		}
		if !ok {
			add("R8", where, "no mapped row has a refusal proof")
		}
	}
	if len(x.fm.Promises) == 0 {
		add("R8", "FEATURES.md", "no threat-model promise table found")
	}
	if len(x.fm.Rows) == 0 {
		add("R3", "FEATURES.md", "no feature table with the exact header was found")
	}
	return fs
}

// catRefs collects every catalogue-kind door ref a row mentions anywhere.
func (x *checker) catRefs(r Row) []DoorRef {
	var out []DoorRef
	for _, d := range r.Doors {
		if d.Catalogue() {
			out = append(out, d)
		}
	}
	for _, p := range r.Proof {
		if p.Door.Kind != "" && p.Door.Catalogue() {
			out = append(out, p.Door)
		}
	}
	for _, t := range r.Tests {
		if t.At != nil && t.At.Catalogue() {
			out = append(out, *t.At)
		}
	}
	return out
}

// testRules is R3: the row has a test the check accepts.
func (x *checker) testRules(fs *[]Finding, r Row, isCI bool) {
	add := func(format string, a ...any) {
		*fs = append(*fs, Finding{"R3", r.ID, fmt.Sprintf(format, a...)})
	}
	ok := false
	journeyOnly := false
	for _, t := range r.Tests {
		switch t.Kind {
		case "e2e":
			ok = true
		case "pending":
			if x.pending[t.Pending] {
				ok = true
			} else {
				add("pending:#%d no longer counts: it is not listed in e2e/coverage/pending.txt", t.Pending)
			}
		case "journey":
			if r.Exception() {
				ok = true
			} else {
				journeyOnly = true
			}
		case "screen-only":
			if r.Exception() {
				ok = true
				*fs = append(*fs, Finding{"info", r.ID, "screen-only: no e2e test reaches this row"})
			} else {
				add("screen-only is allowed only on rows whose Power door is exception:")
			}
		case "ci":
			switch {
			case !isCI:
				add("ci: is allowed only on rows listed under Rows proven by CI")
			default:
				if _, err := os.Stat(filepath.Join(x.root, filepath.FromSlash(t.Name))); err != nil {
					add("ci script %s does not exist", t.Name)
				} else {
					ok = true
				}
			}
		}
	}
	if !ok && journeyOnly {
		add("journey: alone proves only an exception: row")
	}
	if !ok {
		add("row has no accepted test (e2e:, a live pending:#N, or what its kind allows)")
	}
}

func hasRefusal(r Row) bool {
	for _, p := range r.Proof {
		if p.Refusal() {
			return true
		}
	}
	return false
}

func hasDeny(r Row, d DoorRef) bool {
	for _, t := range r.Tests {
		if t.Deny && t.At != nil && *t.At == d && t.Kind == "e2e" {
			return true
		}
	}
	return false
}
