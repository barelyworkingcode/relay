package main

// grant_cmd.go's mount-rendering half: newGrantView/printGrantViews must
// show mount grants beneath a record's MCP rows, mark a home-directory-
// breadth mount loudly, and carry mounts in --json only when there are any.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func mountProjectView(t *testing.T, mounts ...config.MountGrant) grantView {
	t.Helper()
	proj := config.Project{
		ID: "p1", Name: "Agent VM", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{}, Mounts: mounts,
	}
	s := &config.Settings{}
	return newGrantView(s, proj)
}

// A project with mounts but zero granted MCPs must print the mount rows and
// the narrower "no MCPs granted" line — never the "reaches nothing" line,
// which would misdescribe a record that does reach something.
func TestPrintGrantViews_MountsWithZeroMcpsPrintsMountRowsNotReachesNothing(t *testing.T) {
	view := mountProjectView(t, config.MountGrant{ID: "mail", Path: "/tmp/mail-dir"})

	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	out := buf.String()

	if strings.Contains(out, "reaches nothing") {
		t.Fatalf("output claims the record reaches nothing despite a mount grant:\n%s", out)
	}
	if !strings.Contains(out, "no MCPs granted") {
		t.Fatalf("output does not say no MCPs were granted:\n%s", out)
	}
	if !strings.Contains(out, "mail") || !strings.Contains(out, "/tmp/mail-dir") {
		t.Fatalf("output does not print the mount row (id/path):\n%s", out)
	}
}

// A project with neither MCPs nor mounts prints the "reaches nothing" line.
func TestPrintGrantViews_NeitherMcpsNorMountsPrintsReachesNothing(t *testing.T) {
	view := mountProjectView(t) // no mounts, no MCPs

	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	out := buf.String()

	if !strings.Contains(out, "reaches nothing") {
		t.Fatalf("output does not say the record reaches nothing:\n%s", out)
	}
}

// A mount whose path is a whole home directory gets a warning marker in the
// printed output, the same "!!" treatment scope breadth gets for MCP scope
// values.
func TestPrintGrantViews_HomeDirectoryBreadthMountIsMarkedWithAWarning(t *testing.T) {
	view := mountProjectView(t, config.MountGrant{ID: "home", Path: "/Users/someone"})

	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	out := buf.String()

	if !strings.Contains(out, "!!") {
		t.Fatalf("output does not mark the home-directory-breadth mount with a warning:\n%s", out)
	}
	if !strings.Contains(out, "whole home directory") {
		t.Fatalf("output does not name the specific breadth finding:\n%s", out)
	}
}

// An ordinary bounded mount carries no warning marker — the positive
// control for the breadth test above.
func TestPrintGrantViews_OrdinaryMountIsNotMarkedWithAWarning(t *testing.T) {
	view := mountProjectView(t, config.MountGrant{ID: "proj", Path: "/tmp/some/bounded/dir"})

	var buf bytes.Buffer
	printGrantViews(&buf, []grantView{view})
	out := buf.String()

	if strings.Contains(out, "!!") {
		t.Fatalf("an ordinary bounded mount must not carry a breadth warning marker:\n%s", out)
	}
}

// grantView's JSON carries a "mounts" key only when the project actually has
// mounts — mirroring "mcps" always being present but "mounts" being
// omitempty (grantView's own doc comment): a profile with none must not
// gain a new key in --json output it did not have before this feature.
func TestGrantView_JSONMountsKeyOnlyWhenNonEmpty(t *testing.T) {
	withoutMounts := mountProjectView(t)
	data, err := json.Marshal(withoutMounts)
	assertNoErr(t, err, "marshal grantView with no mounts")
	if strings.Contains(string(data), `"mounts"`) {
		t.Fatalf("grantView JSON carries a \"mounts\" key for a project with none: %s", data)
	}

	withMounts := mountProjectView(t, config.MountGrant{ID: "mail", Path: "/tmp/mail-dir"})
	data, err = json.Marshal(withMounts)
	assertNoErr(t, err, "marshal grantView with mounts")
	if !strings.Contains(string(data), `"mounts"`) {
		t.Fatalf("grantView JSON does not carry a \"mounts\" key for a project with one: %s", data)
	}

	var decoded grantView
	assertNoErr(t, json.Unmarshal(data, &decoded), "unmarshal grantView")
	if len(decoded.Mounts) != 1 || decoded.Mounts[0].ID != "mail" || decoded.Mounts[0].Path != "/tmp/mail-dir" {
		t.Fatalf("decoded Mounts = %+v, want the one stored mount", decoded.Mounts)
	}
}

// newGrantView resolves a mount's access through AccessMode(), not the raw
// stored string: an absent Access must render as "read", matching what the
// mount actually behaves as at attach time.
func TestNewGrantView_MountAccessIsResolvedThroughAccessMode(t *testing.T) {
	view := mountProjectView(t, config.MountGrant{ID: "mail", Path: "/tmp/mail-dir"}) // Access absent
	if len(view.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly 1", view.Mounts)
	}
	if view.Mounts[0].Access != config.AccessRead {
		t.Fatalf("Access = %q, want %q for an absent stored Access", view.Mounts[0].Access, config.AccessRead)
	}
}
