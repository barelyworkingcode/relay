package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

var (
	g9TokenLine = regexp.MustCompile(`(?m)^  token:\s+([0-9a-f]{64})\s*$`)
	g9IDLine    = regexp.MustCompile(`(?m)^  id:\s+(\S+)\s*$`)
)

// g9Minted is what `relay credential mint` printed. The verb has no JSON form,
// so a script reads the two labelled lines (docs/cli.md).
type g9Minted struct{ ID, Token string }

func g9ParseMint(t *testing.T, stdout []byte) g9Minted {
	t.Helper()
	tok := g9TokenLine.FindSubmatch(stdout)
	id := g9IDLine.FindSubmatch(stdout)
	if tok == nil || id == nil {
		t.Fatalf("credential mint printed no token line or no id line (stdout is %d bytes)", len(stdout))
	}
	return g9Minted{ID: string(id[1]), Token: string(tok[1])}
}

// g9ListRow is one data row of `relay credential list`: ID NAME CLASSES CREATED EXPIRES.
type g9ListRow struct {
	ID, Name, Classes, Created, Expires string
}

func g9ParseList(stdout []byte) []g9ListRow {
	var rows []g9ListRow
	for n, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		f := strings.Fields(line)
		if n == 0 || len(f) < 5 {
			continue
		}
		rows = append(rows, g9ListRow{ID: f[0], Name: f[1], Classes: f[2], Created: f[3], Expires: strings.Join(f[4:], " ")})
	}
	return rows
}

func g9ListRows(t *testing.T, i *harness.Instance) []g9ListRow {
	t.Helper()
	return g9ParseList(i.MustCLI("credential", "list").Stdout)
}

// g9DeniedRows returns the denied control_decision rows whose method is method.
func g9DeniedRows(i *harness.Instance, method string) []map[string]any {
	var out []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == method {
			out = append(out, row)
		}
	}
	return out
}

func g9RequireDenied(t *testing.T, i *harness.Instance, trace, key string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	if len(g9DeniedRows(i, key)) == 0 {
		t.Fatalf("no denied control_decision row with method %s", key)
	}
}

func TestCredentialMint(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"credential.mint": harness.OutcomeApprove}})
	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "mint", "--name", "acme", "--class", "read")
	if r.Code != 0 {
		t.Fatalf("an approved mint exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	m := g9ParseMint(t, r.Stdout)

	if got := i.HTTP(harness.Credential{Token: m.Token}).Do("GET", "/api/projects", nil).Status; got != 200 {
		t.Fatalf("the minted token answered GET /api/projects with %d, want 200", got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "credential.mint", Trace: tr, Fields: map[string]any{"status": "ok", "credential_id": m.ID}})
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued", Outcome: "ok"}) {
		found = found || row["subject"] == m.ID
	}
	if !found {
		t.Fatalf("no credential_issued ok row names credential %s", m.ID)
	}
	list := i.MustCLI("credential", "list")
	classes := ""
	for _, line := range strings.Split(string(list.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == m.ID {
			classes = f[2]
		}
	}
	if classes != "read" {
		t.Fatalf("credential list shows classes %q for %s, want read\n%s", classes, m.ID, list.Stdout)
	}
}

func TestCredentialMintDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"credential.mint": harness.OutcomeDeny},
		Credentials: readOnly,
	})
	before := g9ListRows(t, i)
	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "mint", "--name", "acme", "--class", "read")
	if r.Code != 1 {
		t.Fatalf("a denied mint exited %d, want 1", r.Code)
	}
	if len(bytes.TrimSpace(r.Stdout)) != 0 {
		t.Fatalf("a denied mint printed %d bytes on stdout, want none", len(r.Stdout))
	}
	g9RequireDenied(t, i, tr, "credential.mint")
	after := g9ListRows(t, i)
	if len(after) != len(before) {
		t.Fatalf("credential list changed from %d to %d rows after a denied mint", len(before), len(after))
	}
	if n := len(i.Audit(harness.AuditQuery{Event: "credential_issued"})); n != 0 {
		t.Fatalf("a denied mint left %d credential_issued rows", n)
	}
}

func TestCredentialListHidesToken(t *testing.T) {
	t.Parallel()
	expires := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{
		{Name: "acme-view", Classes: []string{"read", "configure"}, Expires: expires},
		{Name: "acme-forever", Classes: []string{"execute"}},
	}})
	view, forever := i.Credential("acme-view"), i.Credential("acme-forever")

	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "list")
	if r.Code != 0 {
		t.Fatalf("credential list exited %d", r.Code)
	}
	rows := g9ParseList(r.Stdout)
	byID := map[string]g9ListRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := byID[view.ID]; got.Classes != "read,configure" || got.Expires != expires.Format(time.RFC3339) {
		t.Fatalf("row for the planted credential is %+v, want classes read,configure and expiry %s", got, expires.Format(time.RFC3339))
	}
	if got := byID[forever.ID]; got.Classes != "execute" || got.Expires != "never" {
		t.Fatalf("row for the credential with no expiry is %+v, want classes execute and expiry never", got)
	}
	for _, c := range []harness.Credential{view, forever} {
		if bytes.Contains(r.Stdout, []byte(c.Token)) {
			t.Fatalf("credential list printed the token of %s", c.Name)
		}
	}
	requireEvent(t, i, harness.EventQuery{Key: "credential.list", Trace: tr, Fields: map[string]any{"status": "ok", "count": len(rows)}})
}

func TestCredentialRevoke(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"credential.revoke": harness.OutcomeApprove},
		Credentials: []harness.CredentialSpec{{Name: "acme-doomed", Classes: []string{"read"}}},
	})
	doomed := i.Credential("acme-doomed")
	if got := i.HTTP(doomed).Do("GET", "/api/projects", nil).Status; got != 200 {
		t.Fatalf("the planted token answered %d before the revoke, want 200", got)
	}
	tr := harness.NewTrace(t)
	if r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "revoke", "--id", doomed.ID); r.Code != 0 {
		t.Fatalf("an approved revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	if got := i.HTTP(doomed).Do("GET", "/api/projects", nil).Status; got != 401 {
		t.Fatalf("the revoked token answered %d, want 401", got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "credential.revoke", Trace: tr, Fields: map[string]any{"status": "ok", "credential_id": doomed.ID}})
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_revoked", Outcome: "ok"}) {
		found = found || row["subject"] == doomed.ID
	}
	if !found {
		t.Fatalf("no credential_revoked ok row names credential %s", doomed.ID)
	}
}

func TestCredentialRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"credential.revoke": harness.OutcomeDeny},
		Credentials: []harness.CredentialSpec{{Name: "acme-keeper", Classes: []string{"read"}}},
	})
	keeper := i.Credential("acme-keeper")
	tr := harness.NewTrace(t)
	if r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "revoke", "--id", keeper.ID); r.Code != 1 {
		t.Fatalf("a denied revoke exited %d, want 1", r.Code)
	}
	g9RequireDenied(t, i, tr, "credential.revoke")
	if got := i.HTTP(keeper).Do("GET", "/api/projects", nil).Status; got != 200 {
		t.Fatalf("the token answered %d after a denied revoke, want 200", got)
	}
	if n := len(i.Audit(harness.AuditQuery{Event: "credential_revoked"})); n != 0 {
		t.Fatalf("a denied revoke left %d credential_revoked rows", n)
	}
}

func TestClassEnforcement(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: readOnly})
	decisions := func(method, path, outcome string) int {
		n := 0
		for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: outcome}) {
			if row["method"] == method && row["path"] == path {
				n++
			}
		}
		return n
	}
	allBefore := len(i.Audit(harness.AuditQuery{Event: "control_decision"}))

	if got := i.Anonymous().Do("GET", "/api/projects", nil).Status; got != 401 {
		t.Fatalf("GET /api/projects with no credential answered %d, want 401", got)
	}
	if got := len(i.Audit(harness.AuditQuery{Event: "control_decision"})); got != allBefore {
		t.Fatalf("a 401 wrote %d control_decision rows, want none", got-allBefore)
	}

	body := map[string]any{"name": "Acme", "path": i.Home}
	if got := i.HTTP(i.Credential("reader")).Do("POST", "/api/projects", body).Status; got != 403 {
		t.Fatalf("a read credential on POST /api/projects answered %d, want 403", got)
	}
	if n := decisions("POST", "/api/projects", "denied"); n != 1 {
		t.Fatalf("%d denied control_decision rows name POST /api/projects, want 1", n)
	}
	if n := len(listProjects(t, i)); n != 0 {
		t.Fatalf("the refused POST left %d projects", n)
	}
}

func g9Lines(t *testing.T, out []byte) []harness.Event {
	t.Helper()
	var evs []harness.Event
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev harness.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("a logs --json line does not decode: %v", err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestLogsFilterAndFollow(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: readOnly})
	c := i.HTTP(i.Credential("reader"))
	first, second := harness.NewTrace(t), harness.NewTrace(t)

	if got := c.Do("GET", "/api/projects", nil, harness.ReqOpts{Trace: first}).Status; got != 200 {
		t.Fatalf("first GET /api/projects answered %d", got)
	}
	// A process start separates the two calls by more than the log's millisecond.
	i.MustCLI("credential", "list")
	if got := c.Do("GET", "/api/projects", nil, harness.ReqOpts{Trace: second}).Status; got != 200 {
		t.Fatalf("second GET /api/projects answered %d", got)
	}

	t.Run("event_and_trace", func(t *testing.T) {
		r := i.CLI("logs", "--json", "--event", "project.list", "--trace", first)
		if r.Code != 0 {
			t.Fatalf("logs exited %d", r.Code)
		}
		evs := g9Lines(t, r.Stdout)
		if len(evs) == 0 {
			t.Fatalf("logs printed no line for event project.list and trace %s", first)
		}
		for _, ev := range evs {
			if ev.Str("event") != "project.list" || ev.Str("trace_id") != first {
				t.Fatalf("logs printed a line with event %q trace %q, want project.list and %s", ev.Str("event"), ev.Str("trace_id"), first)
			}
		}
	})

	t.Run("since", func(t *testing.T) {
		secondEv := i.Events(harness.EventQuery{Key: "project.list", Trace: second})
		firstEv := i.Events(harness.EventQuery{Key: "project.list", Trace: first})
		if len(secondEv) == 0 || len(firstEv) == 0 {
			t.Fatalf("project.list lines found: first %d, second %d", len(firstEv), len(secondEv))
		}
		cut := secondEv[0].Str("ts")
		if cut <= firstEv[0].Str("ts") {
			t.Fatalf("the second call's ts %s is not after the first's %s", cut, firstEv[0].Str("ts"))
		}
		r := i.CLI("logs", "--json", "--event", "project.list", "--since", cut)
		if r.Code != 0 {
			t.Fatalf("logs --since exited %d", r.Code)
		}
		seen := map[string]bool{}
		for _, ev := range g9Lines(t, r.Stdout) {
			seen[ev.Str("trace_id")] = true
		}
		if !seen[second] || seen[first] {
			t.Fatalf("logs --since %s printed traces %v, want the second call's and not the first's", cut, seen)
		}
	})

	t.Run("follow", func(t *testing.T) {
		tf := harness.NewTrace(t)
		p := i.StartCLI(harness.CLIOpts{}, "logs", "--follow", "--event", "credential.list", "--trace", tf, "--timeout", "30s", "--json")
		listed := i.CLIWith(harness.CLIOpts{Trace: tf}, "credential", "list")
		if listed.Code != 0 {
			t.Fatalf("credential list exited %d", listed.Code)
		}
		res := p.Wait()
		if res.Code != 0 {
			t.Fatalf("logs --follow exited %d, want 0 after a credential.list event\nstderr: %s", res.Code, res.Stderr)
		}
		evs := g9Lines(t, res.Stdout)
		if len(evs) == 0 || evs[0].Str("event") != "credential.list" || evs[0].Str("trace_id") != tf {
			t.Fatalf("logs --follow printed %d lines, want a credential.list line with trace %s: %s", len(evs), tf, res.Stdout)
		}
	})
}

func TestDoorsNameClassAndGates(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	type door struct {
		Kind       string   `json:"kind"`
		Name       string   `json:"name"`
		Credential string   `json:"credential"`
		OwnerGated bool     `json:"owner_gated"`
		Gates      []string `json:"gates"`
	}
	var doc struct {
		Doors []door `json:"doors"`
	}
	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "doors", "--json")
	if r.Code != 0 {
		t.Fatalf("doors --json exited %d", r.Code)
	}
	r.JSON(t, &doc)

	find := func(kind, name string) door {
		for _, d := range doc.Doors {
			if d.Kind == kind && d.Name == name {
				return d
			}
		}
		t.Fatalf("doors lists no %s door %q", kind, name)
		return door{}
	}
	post := find("http", "POST /api/projects")
	if post.Credential != "configure" || len(post.Gates) != 1 || post.Gates[0] != "project.grant" {
		t.Fatalf("POST /api/projects is class %q gates %v, want configure and [project.grant]", post.Credential, post.Gates)
	}
	if mint := find("cli", "relay credential mint"); !mint.OwnerGated {
		t.Fatalf("relay credential mint is not owner_gated")
	}
	requireEvent(t, i, harness.EventQuery{Key: "doors.list", Trace: tr, Fields: map[string]any{"status": "ok", "count": len(doc.Doors)}})
}

func TestTraceFlagNamesEvents(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: readOnly})

	cliTrace := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: cliTrace}, "grant", "--json")
	if r.Code != 0 {
		t.Fatalf("grant --json exited %d", r.Code)
	}
	if got := requireEvent(t, i, harness.EventQuery{Key: "grant.view", Trace: cliTrace}).Str("trace_id"); got != cliTrace {
		t.Fatalf("grant.view carries trace_id %q, want %q", got, cliTrace)
	}

	httpTrace := harness.NewTrace(t)
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil, harness.ReqOpts{Trace: httpTrace})
	if resp.Status != 200 {
		t.Fatalf("GET /api/projects answered %d", resp.Status)
	}
	if got := requireEvent(t, i, harness.EventQuery{Key: "project.list", Trace: httpTrace}).Str("trace_id"); got != httpTrace {
		t.Fatalf("project.list carries trace_id %q, want %q", got, httpTrace)
	}
}

func TestOperatorVerbRefusedInSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		sandbox bool
	}{{"sandboxed", true}, {"unsandboxed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := json.Marshal([]map[string]any{{
				"id": "opverb", "name": "Opverb", "command": harness.BundlePaths().Relay, "sandbox": tc.sandbox,
			}})
			if err != nil {
				t.Fatalf("encoding the template: %v", err)
			}
			i := harness.Start(t, harness.Options{
				Presence: approveGrant,
				Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
			})
			i.WaitSessionHost(60 * time.Second)
			dir := filepath.Join(i.Home, "work", "acme")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("creating %s: %v", dir, err)
			}
			body, _ := json.Marshal(map[string]any{"name": "Acme opverb", "path": dir, "allowed_templates": []string{"opverb"}})
			var p project
			i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)

			var term struct {
				TerminalID string `json:"terminalId"`
			}
			// The CLI strips a bare --config-dir argument before the verb parses, so each
			// extra argument is written in its --extra-arg=VALUE form (docs/cli.md).
			i.MustCLI("terminal", "start", "--project", p.ID, "--template", "opverb",
				"--extra-arg=--config-dir", "--extra-arg="+i.ConfigDir, "--extra-arg=status", "--json").JSON(t, &term)
			i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, 60*time.Second)

			var list struct {
				Terminals []struct {
					ID       string `json:"id"`
					ExitCode *int   `json:"exitCode"`
				} `json:"terminals"`
			}
			i.MustCLI("terminal", "list", "--json").JSON(t, &list)
			code := -1
			for _, x := range list.Terminals {
				if x.ID == term.TerminalID {
					code = 0
					if x.ExitCode != nil {
						code = *x.ExitCode
					}
				}
			}
			if code != 1 {
				t.Fatalf("the status verb run by a terminal exited %d, want 1", code)
			}
			// A refused bridge admin op is a control_decision row with method admin_op
			// and the op's name as path.
			refused := 0
			for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
				if row["method"] == "admin_op" && row["path"] == "status.view" {
					refused++
				}
			}
			if refused != 1 {
				t.Fatalf("%d denied control_decision rows name admin_op status.view, want 1", refused)
			}
		})
	}
}
