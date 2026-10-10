package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"relaye2e/harness"
)

func cliWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}},
		Projects: []Project{
			{ID: "p_acme", Name: "Acme", Mode: "work"},
			{ID: "p_zenith", Name: "Zenith", Mode: "home"},
		},
	}
}

func TestCLIGrantWithRecords(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    cliWorld(),
		Body: func(r *Run) {
			r.CLI("grant", "--json")
			r.CLI("grant", "--json", "--project", "Acme")
			r.CLI("grant", "--json", "--project", "p_missing")
		},
	})
}

func TestCLIGrantWithoutRecords(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    Spec{Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}}},
		Body: func(r *Run) {
			r.CLI("grant", "--json")
		},
	})
}

func TestCLINoServer(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    cliWorld(),
		Body: func(r *Run) {
			r.Target.I.Stop()
			r.CLI("mcp", "list")
			r.CLI("grant", "--json")
		},
	})
}

var createdID = regexp.MustCompile(`\(([^()]+)\)\s*$`)

// writeBody writes a JSON body under the instance's made directory.
func writeBody(r *Run, name string, body any) string {
	r.T.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		r.T.Fatalf("encoding %s: %v", name, err)
	}
	p := filepath.Join(r.Target.I.Dir, "made", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.T.Fatalf("mkdir for %s: %v", p, err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		r.T.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestCLIProjectCreate(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
		},
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.CLIWith(harness.CLIOpts{Trace: tr}, "project", "create", "--name", "Acme Two", "--path", newProjectDir(r, "acme2"), "--json")
			r.Event(harness.EventQuery{Key: "project.create", Trace: tr})

			file := writeBody(r, "acme3.json", map[string]any{"name": "Acme Three", "path": newProjectDir(r, "acme3"), "mode": "work"})
			r.CLI("project", "create", "--file", file, "--json")

			stdin, _ := json.Marshal(map[string]any{"name": "Acme Four", "path": newProjectDir(r, "acme4"), "mode": "work"})
			r.CLIWith(harness.CLIOpts{Stdin: stdin}, "project", "create", "--file", "-", "--json")

			res := r.Target.I.CLI("project", "create", "--name", "Acme Five", "--path", newProjectDir(r, "acme5"))
			m := createdID.FindSubmatch(res.Stdout)
			if res.Code == 0 && m != nil {
				r.Learn(string(m[1]))
			}
			r.Note("text form", map[string]any{"code": res.Code, "has_id": m != nil})

			r.CLI("project", "create")
			r.CLI("project", "create", "--name", "Acme Six", "--path", newProjectDir(r, "acme6"), "--file", file)

			r.Target.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
			res = r.Target.I.CLI("project", "create", "--name", "Acme Seven", "--path", newProjectDir(r, "acme7"))
			r.Note("denied create", map[string]any{
				"code": res.Code, "refused": strings.Contains(string(res.Stderr), "presence was refused"),
			})
			r.HTTP("ops", "GET", "/api/projects", nil)
		},
	})
}

func TestCLIProjectEdit(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
			MCPs: []MCP{{
				ID: "m_acme",
				Catalogue: harness.Catalogue{Tools: []json.RawMessage{
					json.RawMessage(`{"name":"acme_ok","description":"Say ok","inputSchema":{"type":"object","properties":{}}}`),
				}},
			}},
		},
		Body: func(r *Run) {
			rename := writeBody(r, "rename.json", map[string]any{"name": "Acme Renamed"})
			r.CLI("project", "edit", "--id", "p_acme", "--file", rename)
			r.CLI("project", "edit", "--id", "p_acme", "--file", rename, "--json")

			widen := writeBody(r, "widen.json", map[string]any{"allowed_mcp_ids": []string{"m_acme"}})
			r.Target.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
			res := r.Target.I.CLI("project", "edit", "--id", "p_acme", "--file", widen)
			r.Note("denied widening", map[string]any{
				"code": res.Code, "refused": strings.Contains(string(res.Stderr), "presence was refused"),
			})
			r.Target.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeApprove})
			r.CLI("project", "edit", "--id", "p_acme", "--file", widen, "--json")

			r.CLI("project", "edit", "--id", "p_missing", "--file", rename)
			r.CLI("project", "edit", "--file", rename)
			r.CLI("project", "edit", "--id", "p_acme")
		},
	})
}
