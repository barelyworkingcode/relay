package coverage

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Reserved area names. every-test selects the whole feature package; no-test
// selects nothing.
const (
	AreaEveryTest = "every-test"
	AreaNoTest    = "no-test"
)

// IsReserved reports whether name is a reserved area.
func IsReserved(name string) bool { return name == AreaEveryTest || name == AreaNoTest }

var (
	areaNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	goalIDRe   = regexp.MustCompile(`^(G[1-9][0-9]?)\b`)
)

// AreaMap is the parsed area map of FEATURES.md: the code globs of each area,
// the areas no headless test can turn red, and the areas each goal lists.
type AreaMap struct {
	Code        map[string][]string
	JourneyOnly map[string]bool
	GoalAreas   map[string][]string // key is the goal ID, "G1"
}

// ParseAreaMap reads the yaml block under the "## Areas" heading and the
// "Areas:" line of every goal. It is a reader for the fixed shape the file
// documents, not a YAML parser. Findings are syntax faults.
func ParseAreaMap(featuresMD string) (AreaMap, []Finding) {
	m := AreaMap{Code: map[string][]string{}, JourneyOnly: map[string]bool{}, GoalAreas: map[string][]string{}}
	var fs []Finding
	bad := func(where, format string, a ...any) {
		fs = append(fs, Finding{"syntax", where, fmt.Sprintf(format, a...)})
	}

	lines := strings.Split(featuresMD, "\n")
	inFence := false
	fenceLang := ""
	section := ""
	goal := ""
	cur := ""        // area being read
	inAreas := false // the "areas:" key was seen in the block
	blockSeen := false
	type goalLine struct {
		goal, where string
		names       []string
	}
	var goalLines []goalLine

	for n, line := range lines {
		where := fmt.Sprintf("FEATURES.md:%d", n+1)
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			if inFence {
				inFence = false
				cur, inAreas = "", false
			} else {
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trim, "```"))
			}
			continue
		}
		if inFence {
			if section != "areas" || fenceLang != "yaml" {
				continue
			}
			blockSeen = true
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " "))
			key, val, ok := strings.Cut(trim, ":")
			if !ok {
				bad(where, "expected key: value, got %q", trim)
				continue
			}
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			switch indent {
			case 0:
				if key != "areas" || val != "" {
					bad(where, "the block starts with \"areas:\", got %q", trim)
				}
				inAreas = true
			case 2:
				cur = ""
				if !inAreas {
					bad(where, "area %q sits outside the areas: key", key)
					continue
				}
				if !areaNameRe.MatchString(key) {
					bad(where, "area name %q does not match %s", key, areaNameRe)
					continue
				}
				if _, dup := m.Code[key]; dup {
					bad(where, "area %q is defined twice", key)
					continue
				}
				if val != "" {
					bad(where, "area %q takes keys on the lines below, not a value", key)
				}
				cur = key
				m.Code[key] = nil
			case 4:
				if cur == "" {
					bad(where, "key %q sits outside an area", key)
					continue
				}
				switch key {
				case "code":
					items, err := flowList(val)
					if err != nil {
						bad(where, "area %s code: %v", cur, err)
						continue
					}
					for _, g := range items {
						if err := validGlob(g); err != nil {
							bad(where, "area %s: glob %q: %v", cur, g, err)
						}
					}
					m.Code[cur] = items
				case "journeys":
					if _, err := flowList(val); err != nil {
						bad(where, "area %s journeys: %v", cur, err)
					}
				case "journey-only":
					switch val {
					case "true":
						m.JourneyOnly[cur] = true
					case "false":
					default:
						bad(where, "area %s journey-only: want true or false, got %q", cur, val)
					}
				default:
					bad(where, "area %s: unknown key %q (known: code, journeys, journey-only)", cur, key)
				}
			default:
				bad(where, "unexpected indent in the areas block")
			}
			continue
		}
		if _, txt, ok := heading(line); ok {
			t := strings.ToLower(plain(txt))
			if lvl, _, _ := heading(line); lvl == 2 {
				section = t
			}
			goal = ""
			if gm := goalIDRe.FindStringSubmatch(plain(txt)); gm != nil {
				goal = gm[1]
			}
			continue
		}
		if goal != "" && strings.HasPrefix(trim, "Areas:") {
			var names []string
			for _, s := range strings.Split(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trim, "Areas:")), "."), ",") {
				if s = strings.TrimSpace(s); s != "" {
					names = append(names, s)
				}
			}
			goalLines = append(goalLines, goalLine{goal, where, names})
		}
	}

	if !blockSeen {
		bad("FEATURES.md", "no yaml block under the \"## Areas\" heading")
	}
	for name, globs := range m.Code {
		if len(globs) == 0 {
			bad("FEATURES.md", "area %s has no code globs", name)
		}
	}
	for _, gl := range goalLines {
		if _, dup := m.GoalAreas[gl.goal]; dup {
			bad(gl.where, "goal %s has more than one Areas: line", gl.goal)
		}
		if len(gl.names) == 0 {
			bad(gl.where, "goal %s: Areas: line names no area", gl.goal)
		}
		for _, a := range gl.names {
			if _, ok := m.Code[a]; !ok {
				bad(gl.where, "goal %s: Areas: line names %q, which is not defined in the areas block", gl.goal, a)
			}
		}
		m.GoalAreas[gl.goal] = gl.names
	}
	return m, fs
}

// flowList reads "[a, b, c]". An empty list is valid here; the caller decides
// whether it may be.
func flowList(val string) ([]string, error) {
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, fmt.Errorf("want a flow list [a, b], got %q", val)
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, s := range strings.Split(inner, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("empty item in %q", val)
		}
		out = append(out, s)
	}
	return out, nil
}

// validGlob checks a glob without matching a path.
func validGlob(glob string) error {
	_, err := GlobMatch(glob, "x")
	return err
}

// GlobMatch matches a repo-relative, slash-separated path against a glob.
// path.Match rules apply to the whole path, so "*" never crosses "/". "**" is
// allowed only as the last segment and means everything under the directory
// before it; a bare "**" is refused because it would map the whole tree.
func GlobMatch(glob, p string) (bool, error) {
	if glob == "" {
		return false, fmt.Errorf("empty glob")
	}
	if strings.HasPrefix(glob, "/") || strings.Contains(glob, "..") || strings.Contains(glob, `\`) {
		return false, fmt.Errorf("a glob is repo-relative, slash-separated and has no \"..\"")
	}
	dir, deep := strings.CutSuffix(glob, "/**")
	if deep && dir == "" {
		return false, fmt.Errorf("\"/**\" has no directory")
	}
	if strings.Contains(dir, "**") || (!deep && strings.Contains(glob, "**")) {
		return false, fmt.Errorf("\"**\" is allowed only as the last segment, after a directory")
	}
	if !deep {
		ok, err := path.Match(glob, p)
		if err != nil {
			return false, fmt.Errorf("bad pattern: %w", err)
		}
		return ok, nil
	}
	gs, ps := strings.Split(dir, "/"), strings.Split(p, "/")
	for _, g := range gs {
		if _, err := path.Match(g, ""); err != nil {
			return false, fmt.Errorf("bad pattern: %w", err)
		}
	}
	if len(ps) <= len(gs) {
		return false, nil
	}
	for i, g := range gs {
		if ok, _ := path.Match(g, ps[i]); !ok {
			return false, nil
		}
	}
	return true, nil
}

// AreasOf returns the sorted names of the areas whose globs match p.
func (m AreaMap) AreasOf(p string) []string {
	var out []string
	for name, globs := range m.Code {
		for _, g := range globs {
			if ok, err := GlobMatch(g, p); err == nil && ok {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsTestOnly reports whether a change to p can alter no shipped behaviour:
// test files, markdown, fixtures. A path with ".." is never test-only, so a
// crafted path cannot hide behind a fixture prefix.
func IsTestOnly(p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	if strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".md") {
		return true
	}
	for _, dir := range []string{"testdata/", "test/fixtures/"} {
		if strings.HasPrefix(p, dir) || strings.Contains(p, "/"+dir) {
			return true
		}
	}
	return false
}
