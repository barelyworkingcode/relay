package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// featureAreas reads the journeys: list of every area in FEATURES.md's
// fenced Areas block.
func featureAreas(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/FEATURES.md")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(raw), "```yaml\nareas:\n")
	if !ok {
		t.Fatal("FEATURES.md has no ```yaml areas: block")
	}
	block, _, _ = strings.Cut(block, "```")
	areas := map[string][]string{}
	area := ""
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":"):
			area = strings.TrimSuffix(strings.TrimSpace(line), ":")
			areas[area] = nil
		case strings.HasPrefix(strings.TrimSpace(line), "journeys:"):
			list := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "journeys:")), "[]")
			for _, id := range strings.Split(list, ",") {
				if id = strings.TrimSpace(id); id != "" {
					areas[area] = append(areas[area], id)
				}
			}
		}
	}
	if len(areas) == 0 {
		t.Fatal("parsed no areas from FEATURES.md")
	}
	return areas
}

func TestJourneyAreasMatchFeatureMap(t *testing.T) {
	listed := featureAreas(t)
	named := map[string][]string{}
	for _, j := range journeys {
		for _, a := range j.Areas {
			if _, ok := listed[a]; !ok {
				t.Errorf("%s names area %q, which FEATURES.md does not define", j.ID, a)
			}
			named[a] = append(named[a], j.ID)
		}
	}
	for a, ids := range listed {
		got, want := slices.Sorted(slices.Values(ids)), slices.Sorted(slices.Values(named[a]))
		if !slices.Equal(got, want) {
			t.Errorf("area %s: FEATURES.md lists %v, journeys naming it are %v", a, got, want)
		}
	}
}
