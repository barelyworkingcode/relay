package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g2tgHomeWarning = `read_write "~" is a whole home directory`

// g2tgDeadline bounds the wait for an audit row: Record is an async queue and
// no door reports a row written, so the test re-reads relay audit (each read
// is a process spawn, which paces the loop) until the deadline.
const g2tgDeadline = 30 * time.Second

type g2tgView struct {
	ID        string `json:"id"`
	Templates []struct {
		ID       string   `json:"id"`
		Sandbox  bool     `json:"sandbox"`
		Warnings []string `json:"warnings"`
	} `json:"templates"`
}

// g2tgRowsUntil polls the ok session_launch rows until pred holds for them.
func g2tgRowsUntil(t *testing.T, i *harness.Instance, what string, pred func(rows []map[string]any) bool) []map[string]any {
	t.Helper()
	end := time.Now().Add(g2tgDeadline)
	for {
		rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "ok"})
		if pred(rows) {
			return rows
		}
		if time.Now().After(end) {
			t.Fatalf("no session_launch row appeared for %s within %v; rows: %v", what, g2tgDeadline, rows)
		}
	}
}

func g2tgWarnings(row map[string]any) ([]any, bool) {
	args, _ := row["args"].(map[string]any)
	w, has := args["grant_warnings"]
	if !has {
		return nil, false
	}
	list, _ := w.([]any)
	return list, true
}

func TestGrantFlagsHomeTemplateFolder(t *testing.T) {
	t.Parallel()
	tmpl := json.RawMessage(`[
 {"id":"shell","name":"Shell","command":"/bin/sh","sandbox":true,"read_write":["~"]},
 {"id":"acme-up","name":"Acme up","command":"/bin/sh","sandbox":true,"read":["${PROJECT_PATH}/.."]},
 {"id":"acme-sub","name":"Acme sub","command":"/bin/sh","sandbox":true,"read_write":["${PROJECT_PATH}/sub"]},
 {"id":"acme-denied","name":"Acme deny","command":"/bin/sh","sandbox":true,"deny":["~"]},
 {"id":"acme-hidden","name":"Acme hidden","command":"/bin/sh","sandbox":true,"read_write":["~"]}
]`)
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	})
	// The project sits directly in the home directory, so ${PROJECT_PATH}/..
	// is the home directory.
	direct := filepath.Join(i.Home, "acme-direct")
	if err := os.MkdirAll(direct, 0o700); err != nil {
		t.Fatalf("creating %s: %v", direct, err)
	}
	p := g1tCreate(t, i, map[string]any{
		"name": "acme-direct", "path": direct,
		"allowed_templates": []string{"shell", "acme-up", "acme-sub", "acme-denied"},
	})

	res := i.MustCLI("grant", "--project", p.ID, "--json")
	var views []g2tgView
	res.JSON(t, &views)
	if len(views) != 1 {
		t.Fatalf("grant --project printed %d records, want 1: %s", len(views), res.Stdout)
	}
	warnings := map[string][]string{}
	for _, tm := range views[0].Templates {
		warnings[tm.ID] = tm.Warnings
		if !tm.Sandbox {
			t.Fatalf("template %s reads sandbox=false, want true", tm.ID)
		}
	}
	if len(views[0].Templates) != 4 {
		t.Fatalf("grant lists %d templates, want the 4 the project allows: %s", len(views[0].Templates), res.Stdout)
	}
	if _, listed := warnings["acme-hidden"]; listed {
		t.Fatalf("grant lists a template outside allowed_templates")
	}
	if got := warnings["shell"]; len(got) != 1 || got[0] != g2tgHomeWarning {
		t.Fatalf("shell warnings are %q, want [%q]", got, g2tgHomeWarning)
	}
	if got := warnings["acme-up"]; len(got) != 1 || !strings.HasPrefix(got[0], `read "${PROJECT_PATH}/.." is `) {
		t.Fatalf("acme-up warnings are %q, want one for the ${PROJECT_PATH}/.. folder", got)
	}
	if got := warnings["acme-sub"]; len(got) != 0 {
		t.Fatalf("acme-sub warnings are %q, want none for ${PROJECT_PATH}/sub", got)
	}
	if got := warnings["acme-denied"]; len(got) != 0 {
		t.Fatalf("acme-denied warnings are %q, want none: deny is never flagged", got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.view", Trace: res.Trace, Fields: map[string]any{"status": "ok"}})

	text := i.MustCLI("grant", "--project", p.ID)
	if !bytes.Contains(text.Stdout, []byte("!!")) {
		t.Fatalf("grant text output carries no !! mark: %s", text.Stdout)
	}
}

func TestLaunchAuditFlagsHomeFolder(t *testing.T) {
	t.Parallel()
	tmpl := json.RawMessage(`[
 {"id":"acme-home","name":"Acme home","command":"/usr/bin/true","sandbox":true,"read_write":["~"]},
 {"id":"acme-narrow","name":"Acme narrow","command":"/usr/bin/true","sandbox":true,"read_write":["~/.acme-narrow"]}
]`)
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	})
	i.WaitSessionHost(g1tHostDeadline)
	if err := os.MkdirAll(filepath.Join(i.Home, ".acme-narrow"), 0o700); err != nil {
		t.Fatalf("creating the narrow folder: %v", err)
	}
	g1tProject(t, i, "acme-audit-home", map[string]any{"allowed_templates": []string{"acme-home", "acme-narrow"}})
	dir := g1tDir(i, "acme-audit-home")

	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "sandbox", "acme-home").Wait(); res.Code != 0 {
		t.Fatalf("relay sandbox of the home template exited %d, want 0", res.Code)
	}
	rows := g2tgRowsUntil(t, i, "the home template", func(rows []map[string]any) bool { return len(rows) >= 1 })
	warned, has := g2tgWarnings(rows[len(rows)-1])
	if !has || len(warned) != 1 || warned[0] != g2tgHomeWarning {
		t.Fatalf("the home template's row args.grant_warnings is %v (present %v), want [%q]", warned, has, g2tgHomeWarning)
	}
	text := i.MustCLI("audit", "--event", "session_launch", "--tail", "5")
	if !bytes.Contains(text.Stdout, []byte("GRANT BREADTH:")) {
		t.Fatalf("relay audit text carries no GRANT BREADTH: prefix: %s", text.Stdout)
	}

	before := len(rows)
	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "sandbox", "acme-narrow").Wait(); res.Code != 0 {
		t.Fatalf("relay sandbox of the narrow template exited %d, want 0", res.Code)
	}
	rows = g2tgRowsUntil(t, i, "the narrow template", func(rows []map[string]any) bool { return len(rows) > before })
	if _, has := g2tgWarnings(rows[len(rows)-1]); has {
		t.Fatalf("the narrow template's row carries args.grant_warnings: %v", rows[len(rows)-1])
	}
}
