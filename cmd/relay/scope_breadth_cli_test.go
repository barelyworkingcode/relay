package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

// The coordinates stay printed; the warning is appended beside them — the
// operator is entitled to both.
func TestAuditAuthorityLine_NamesAnUnrestrictedScope(t *testing.T) {
	allowExternal := false
	line, ok := auditAuthorityLine(audit.AuditEvent{
		Access:        config.AccessWrite,
		AllowExternal: &allowExternal,
		Scope:         map[string]json.RawMessage{"allowed_dirs": json.RawMessage(`["/"]`)},
	})
	if !ok {
		t.Fatal("no authority line for a record that has one")
	}
	if !strings.Contains(line, `allowed_dirs=["/"]`) {
		t.Errorf("the authority line stopped printing the real value: %q", line)
	}
	if !strings.Contains(line, "unrestricted") {
		t.Errorf("the authority line does not flag the filesystem root: %q", line)
	}

	bounded, _ := auditAuthorityLine(audit.AuditEvent{
		Access:        config.AccessWrite,
		AllowExternal: &allowExternal,
		Scope:         map[string]json.RawMessage{"allowed_dirs": json.RawMessage(`["/Users/me/project"]`)},
	})
	if strings.Contains(bounded, "unrestricted") {
		t.Errorf("a bounded grant was flagged: %q", bounded)
	}
}

func TestGrantView_ShowsTheRealValueAndFlagsTheRoot(t *testing.T) {
	s := &config.Settings{
		Version:      1,
		ExternalMcps: []config.ExternalMcp{{ID: "fsmcp", DisplayName: "fsMCP"}},
	}
	profile := config.Project{
		ID: "probe", Name: "Probe", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{"fsmcp"},
		AllowedTools:  map[string][]string{"fsmcp": {"fs_*"}},
		Context: map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/"]}`),
		},
	}
	var out strings.Builder
	printGrantViews(&out, []grantView{newGrantView(s, profile)})
	got := out.String()

	// disclose never governs the operator surface — this is the operator's own
	// machine and their own grant.
	if !strings.Contains(got, `allowed_dirs = ["/"]`) {
		t.Errorf("the operator surface did not print the real value:\n%s", got)
	}
	if !strings.Contains(got, "UNRESTRICTED (THE WHOLE FILESYSTEM)") {
		t.Errorf("the operator surface did not flag the filesystem root:\n%s", got)
	}
	// The two asymmetric defaults are resolved through StoredToken's own
	// methods, so this command cannot drift from what the router decides.
	if !strings.Contains(got, "access=read") || !strings.Contains(got, "outbound=blocked") {
		t.Errorf("an access profile's defaults were not resolved:\n%s", got)
	}
}

func TestGrantView_ABoundedGrantCarriesNoWarning(t *testing.T) {
	s := &config.Settings{Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "fsmcp"}}}
	local := config.Project{
		ID: "proj", Name: "Proj", Path: "/Users/me/project",
		AllowedMcpIDs: []string{"fsmcp"},
		Context: map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/Users/me/project"]}`),
		},
	}
	var out strings.Builder
	printGrantViews(&out, []grantView{newGrantView(s, local)})
	got := out.String()
	if strings.Contains(got, "**") {
		t.Errorf("a one-folder grant was flagged:\n%s", got)
	}
	if !strings.Contains(got, "access=write") || !strings.Contains(got, "outbound=allowed") {
		t.Errorf("a local project's defaults were not resolved:\n%s", got)
	}
	if !strings.Contains(got, "all tools") {
		t.Errorf("a local project with no allowlist should hold every tool:\n%s", got)
	}
}

func TestSelectGrantRecords_ResolvesByIdAndByName(t *testing.T) {
	projects := []config.Project{
		{ID: "b-id", Name: "A name"},
		{ID: "a-id", Name: "B name"},
	}
	if got := selectGrantRecords(projects, "a-id"); len(got) != 1 || got[0].ID != "a-id" {
		t.Errorf("selecting by id returned %+v", got)
	}
	if got := selectGrantRecords(projects, "A name"); len(got) != 1 || got[0].ID != "b-id" {
		t.Errorf("selecting by name returned %+v", got)
	}
	if got := selectGrantRecords(projects, "nope"); got != nil {
		t.Errorf("an unknown selector returned %+v", got)
	}
	// Everything, in name order, so a run-it-over-the-whole-machine sweep
	// reads the same twice.
	all := selectGrantRecords(projects, "")
	if len(all) != 2 || all[0].Name != "A name" {
		t.Errorf("the unfiltered listing is not in name order: %+v", all)
	}
}
