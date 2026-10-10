package features

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g9Deadline = 60 * time.Second

var (
	g9TokenLine = regexp.MustCompile(`(?m)^\s+token:\s+([0-9a-f]{64})\s*$`)
	g9IDLine    = regexp.MustCompile(`(?m)^\s+id:\s+(\S+)\s*$`)
	g9AnyToken  = regexp.MustCompile(`[0-9a-f]{64}`)
)

// g9Minted is what `relay credential mint` printed.
type g9Minted struct {
	ID, Token string
}

// g9Mint mints a credential through the CLI and parses the id and the token
// from the lines a script reads.
func g9Mint(t *testing.T, i *harness.Instance, name string, extra ...string) (g9Minted, harness.Result) {
	t.Helper()
	args := append([]string{"credential", "mint", "--name", name}, extra...)
	r := i.CLI(args...)
	if r.Code != 0 {
		t.Fatalf("credential mint exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	tok, id := g9TokenLine.FindSubmatch(r.Stdout), g9IDLine.FindSubmatch(r.Stdout)
	if tok == nil || id == nil {
		t.Fatalf("credential mint printed no id and token line: %q", r.Stdout)
	}
	return g9Minted{ID: string(id[1]), Token: string(tok[1])}, r
}

func (m g9Minted) credential(name string) harness.Credential {
	return harness.Credential{ID: m.ID, Name: name, Token: m.Token}
}

func g9Status(t *testing.T, c *harness.Client, method, path string, body any) int {
	t.Helper()
	return c.Do(method, path, body).Status
}

// g9Rows filters audit rows by a string field.
func g9Rows(rows []map[string]any, field, want string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if s, _ := r[field].(string); s == want {
			out = append(out, r)
		}
	}
	return out
}

func g9Lines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("output line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCredentialMint(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: map[string]harness.Outcome{"credential.mint": harness.OutcomeApprove},
	})
	m, r := g9Mint(t, i, "acme-viewer", "--class", "read", "--ttl", "1h")

	ev := requireEvent(t, i, harness.EventQuery{Key: "credential.mint", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if ev.Str("credential_id") != m.ID {
		t.Fatalf("credential.mint credential_id %q, the CLI printed id %q", ev.Str("credential_id"), m.ID)
	}

	issued := g9Rows(i.Audit(harness.AuditQuery{Event: "credential_issued", Outcome: "ok"}), "subject_name", "acme-viewer")
	if len(issued) != 1 {
		t.Fatalf("%d credential_issued ok rows name acme-viewer, want 1", len(issued))
	}
	grants, _ := issued[0]["grants"].([]any)
	if len(grants) != 1 || grants[0] != "read" {
		t.Fatalf("credential_issued grants %v, want [read]", issued[0]["grants"])
	}

	c := i.HTTP(m.credential("acme-viewer"))
	if got := g9Status(t, c, "GET", "/api/projects", nil); got != http.StatusOK {
		t.Fatalf("GET /api/projects with the minted read token answered %d, want 200", got)
	}
	if got := g9Status(t, c, "POST", "/api/projects", map[string]any{"name": "acme", "path": i.Home}); got != http.StatusForbidden {
		t.Fatalf("POST /api/projects with a read-only minted token answered %d, want 403", got)
	}
}

func TestCredentialMintDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: map[string]harness.Outcome{"credential.mint": harness.OutcomeDeny},
	})
	r := i.CLI("credential", "mint", "--name", "acme-refused", "--class", "read")
	if r.Code != 1 {
		t.Fatalf("a mint the owner refused exited %d, want 1", r.Code)
	}
	if g9AnyToken.Match(r.Stdout) || g9TokenLine.Match(r.Stdout) {
		t.Fatalf("a refused mint printed a token: %q", r.Stdout)
	}

	requireEvent(t, i, harness.EventQuery{Key: "credential.mint", Trace: r.Trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	denied := g9Rows(i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}), "method", "credential.mint")
	if len(denied) == 0 {
		t.Fatalf("no control_decision denied row names credential.mint")
	}
	if rows := g9Rows(i.Audit(harness.AuditQuery{Event: "credential_issued"}), "subject_name", "acme-refused"); len(rows) != 0 {
		t.Fatalf("a refused mint left %d credential_issued rows", len(rows))
	}
	if list := i.CLI("credential", "list", "--include-expired"); bytes.Contains(list.Stdout, []byte("acme-refused")) {
		t.Fatalf("a refused mint left a credential in the list: %q", list.Stdout)
	}
}

func TestCredentialListHidesToken(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: map[string]harness.Outcome{"credential.mint": harness.OutcomeApprove},
	})
	m, _ := g9Mint(t, i, "acme-lister", "--class", "read", "--class", "configure", "--ttl", "1h")

	list := i.CLI("credential", "list")
	if list.Code != 0 {
		t.Fatalf("credential list exited %d\nstderr: %s", list.Code, list.Stderr)
	}
	sum := sha256.Sum256([]byte(m.Token))
	for _, secret := range []string{m.Token, hex.EncodeToString(sum[:])} {
		if bytes.Contains(list.Stdout, []byte(secret)) || bytes.Contains(list.Stderr, []byte(secret)) {
			t.Fatalf("credential list printed the token or its hash")
		}
	}
	var row string
	for _, line := range strings.Split(string(list.Stdout), "\n") {
		if strings.Contains(line, m.ID) {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("credential list has no row for %s: %q", m.ID, list.Stdout)
	}
	for _, want := range []string{"acme-lister", "read,configure"} {
		if !strings.Contains(row, want) {
			t.Fatalf("the list row %q lacks %q", row, want)
		}
	}
	if strings.Contains(row, "never") {
		t.Fatalf("the row of a credential minted with --ttl shows no expiry: %q", row)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "credential.list", Trace: list.Trace, Fields: map[string]any{"status": "ok"}})
	if count, _ := ev["count"].(float64); count < 1 {
		t.Fatalf("credential.list count %v, want at least 1", ev["count"])
	}
}

func TestCredentialRevoke(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{
			{Name: "victim", Classes: []string{"read"}},
			{Name: "bystander", Classes: []string{"read"}},
		},
		Presence: map[string]harness.Outcome{"credential.revoke": harness.OutcomeApprove},
	})
	victim, bystander := i.Credential("victim"), i.Credential("bystander")
	if got := g9Status(t, i.HTTP(victim), "GET", "/api/projects", nil); got != http.StatusOK {
		t.Fatalf("the credential answered %d before the revoke, want 200", got)
	}

	r := i.CLI("credential", "revoke", "--id", victim.ID)
	if r.Code != 0 {
		t.Fatalf("an approved revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	if got := g9Status(t, i.HTTP(victim), "GET", "/api/projects", nil); got != http.StatusUnauthorized {
		t.Fatalf("the revoked token answered %d, want 401", got)
	}
	if got := g9Status(t, i.HTTP(bystander), "GET", "/api/projects", nil); got != http.StatusOK {
		t.Fatalf("another credential answered %d after the revoke, want 200", got)
	}

	ev := requireEvent(t, i, harness.EventQuery{Key: "credential.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if ev.Str("credential_id") != victim.ID {
		t.Fatalf("credential.revoke credential_id %q, want %q", ev.Str("credential_id"), victim.ID)
	}
	if rows := g9Rows(i.Audit(harness.AuditQuery{Event: "credential_revoked", Outcome: "ok"}), "subject_name", "victim"); len(rows) != 1 {
		t.Fatalf("%d credential_revoked ok rows name victim, want 1", len(rows))
	}
}

func TestCredentialRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "keeper", Classes: []string{"read"}}},
		Presence:    map[string]harness.Outcome{"credential.revoke": harness.OutcomeDeny},
	})
	keeper := i.Credential("keeper")

	r := i.CLI("credential", "revoke", "--id", keeper.ID)
	if r.Code != 1 {
		t.Fatalf("a revoke the owner refused exited %d, want 1", r.Code)
	}
	if got := g9Status(t, i.HTTP(keeper), "GET", "/api/projects", nil); got != http.StatusOK {
		t.Fatalf("the token answered %d after a refused revoke, want 200", got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "credential.revoke", Trace: r.Trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	if len(g9Rows(i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}), "method", "credential.revoke")) == 0 {
		t.Fatalf("no control_decision denied row names credential.revoke")
	}
	if rows := i.Audit(harness.AuditQuery{Event: "credential_revoked"}); len(rows) != 0 {
		t.Fatalf("a refused revoke left %d credential_revoked rows", len(rows))
	}
}

func TestClassEnforcement(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{
			{Name: "reader", Classes: []string{"read"}},
			{Name: "runner", Classes: []string{"execute"}},
		},
	})
	body := map[string]any{"name": "acme", "path": i.Home}
	projectDenials := func() []map[string]any {
		var out []map[string]any
		for _, r := range g9Rows(i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}), "path", "/api/projects") {
			if r["method"] == "POST" {
				out = append(out, r)
			}
		}
		return out
	}

	if got := g9Status(t, i.Anonymous(), "GET", "/api/projects", nil); got != http.StatusUnauthorized {
		t.Fatalf("GET /api/projects with no credential answered %d, want 401", got)
	}
	if got := g9Status(t, i.Anonymous(), "POST", "/api/projects", body); got != http.StatusUnauthorized {
		t.Fatalf("POST /api/projects with no credential answered %d, want 401", got)
	}
	wrong := harness.Credential{Name: "wrong", Token: strings.Repeat("0", 64)}
	if got := g9Status(t, i.HTTP(wrong), "POST", "/api/projects", body); got != http.StatusUnauthorized {
		t.Fatalf("POST /api/projects with an unknown token answered %d, want 401", got)
	}
	if rows := projectDenials(); len(rows) != 0 {
		t.Fatalf("a 401 wrote %d control_decision denied rows, want none", len(rows))
	}

	for _, name := range []string{"reader", "runner"} {
		if got := g9Status(t, i.HTTP(i.Credential(name)), "POST", "/api/projects", body); got != http.StatusForbidden {
			t.Fatalf("POST /api/projects with the %s credential answered %d, want 403", name, got)
		}
	}
	rows := projectDenials()
	if len(rows) != 2 {
		t.Fatalf("%d control_decision denied rows for POST /api/projects after two 403s, want 2", len(rows))
	}
	for _, row := range rows {
		actor, _ := row["actor"].(map[string]any)
		cred, _ := actor["cred_id"].(string)
		if cred != i.Credential("reader").ID && cred != i.Credential("runner").ID {
			t.Fatalf("the denied row names credential %q, want one of the two callers", cred)
		}
	}
	if got := g9Status(t, i.HTTP(i.Credential("reader")), "GET", "/api/projects", nil); got != http.StatusOK {
		t.Fatalf("GET /api/projects with a read credential answered %d, want 200", got)
	}
}

func TestLogsFilterAndFollow(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	traceA, traceB, traceC := harness.NewTrace(t), harness.NewTrace(t), harness.NewTrace(t)
	i.CLIWith(harness.CLIOpts{Trace: traceA}, "credential", "list")
	i.CLIWith(harness.CLIOpts{Trace: traceB}, "doors", "--json")

	byKey := i.CLI("logs", "--json", "--event", "credential.list", "--trace", traceA)
	if byKey.Code != 0 {
		t.Fatalf("logs --event --trace exited %d\nstderr: %s", byKey.Code, byKey.Stderr)
	}
	lines := g9Lines(t, byKey.Stdout)
	if len(lines) == 0 {
		t.Fatalf("logs --event credential.list printed nothing")
	}
	for _, l := range lines {
		if l["event"] != "credential.list" || l["trace_id"] != traceA {
			t.Fatalf("a filtered line has event %v trace_id %v, want credential.list and %s", l["event"], l["trace_id"], traceA)
		}
	}

	byTrace := i.CLI("logs", "--json", "--trace", traceA)
	if byTrace.Code != 0 {
		t.Fatalf("logs --trace exited %d", byTrace.Code)
	}
	for _, l := range g9Lines(t, byTrace.Stdout) {
		if l["trace_id"] != traceA || l["event"] == "doors.list" {
			t.Fatalf("logs --trace %s printed a line of trace %v event %v", traceA, l["trace_id"], l["event"])
		}
	}

	if r := i.CLI("logs", "--json", "--event", "doors.list", "--trace", traceA); r.Code != 1 || len(bytes.TrimSpace(r.Stdout)) != 0 {
		t.Fatalf("logs with filters that match nothing exited %d, want 1 and no output", r.Code)
	}
	if r := i.CLI("logs", "--json", "--event", "doors.list", "--trace", traceB, "--since", "1h"); r.Code != 0 {
		t.Fatalf("logs --since 1h exited %d, want 0 for a line just written", r.Code)
	}
	if r := i.CLI("logs", "--json", "--event", "doors.list", "--trace", traceB, "--since", "2099-01-01T00:00:00Z"); r.Code != 1 {
		t.Fatalf("logs --since a future time exited %d, want 1", r.Code)
	}

	follow := i.StartCLI(harness.CLIOpts{Deadline: 2 * g9Deadline}, "logs", "--follow", "--json", "--event", "credential.list", "--trace", traceC, "--timeout", "60s")
	i.CLIWith(harness.CLIOpts{Trace: traceC}, "credential", "list")
	var got map[string]any
	if err := json.Unmarshal(follow.FirstLine(g9Deadline), &got); err != nil {
		t.Fatalf("the first followed line is not JSON: %v", err)
	}
	if got["event"] != "credential.list" || got["trace_id"] != traceC {
		t.Fatalf("followed line has event %v trace_id %v, want credential.list and %s", got["event"], got["trace_id"], traceC)
	}
	if res := follow.Wait(); res.Code != 0 {
		t.Fatalf("logs --follow --event exited %d after its match, want 0", res.Code)
	}

	idle := harness.NewTrace(t)
	if r := i.CLIWith(harness.CLIOpts{}, "logs", "--follow", "--timeout", "1s", "--event", "doors.list", "--trace", idle); r.Code != 1 {
		t.Fatalf("logs --follow with no match exited %d at its timeout, want 1", r.Code)
	}
}

func TestDoorsNameClassAndGates(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	type door struct {
		Kind       string   `json:"kind"`
		Name       string   `json:"name"`
		Listeners  []string `json:"listeners"`
		Credential string   `json:"credential"`
		OwnerGated bool     `json:"owner_gated"`
		Gates      []string `json:"gates"`
		Calls      []string `json:"calls"`
	}
	var doc struct {
		Doors []door `json:"doors"`
	}
	r := i.MustCLI("doors", "--json")
	r.JSON(t, &doc)

	index := map[string]door{}
	kinds := map[string]bool{}
	for _, d := range doc.Doors {
		index[d.Kind+" "+d.Name] = d
		kinds[d.Kind] = true
		if d.Credential == "" {
			t.Fatalf("door %s %s names no credential class", d.Kind, d.Name)
		}
	}
	for _, k := range []string{"http", "ipc", "bridge", "cli"} {
		if !kinds[k] {
			t.Fatalf("the catalogue lists no %s door", k)
		}
	}
	get := func(kind, name string) door {
		d, ok := index[kind+" "+name]
		if !ok {
			t.Fatalf("the catalogue lacks %s door %q", kind, name)
		}
		return d
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}

	if d := get("http", "GET /api/projects"); d.Credential != "read" || d.OwnerGated {
		t.Fatalf("GET /api/projects is %+v, want class read and ungated", d)
	}
	if d := get("http", "POST /api/projects"); d.Credential != "configure" || !d.OwnerGated || !has(d.Gates, "project.grant") {
		t.Fatalf("POST /api/projects is %+v, want class configure gated by project.grant", d)
	}
	if d := get("http", "GET /ws/files"); d.Credential != "execute" || len(d.Listeners) != 1 || d.Listeners[0] != "socket" {
		t.Fatalf("GET /ws/files is %+v, want class execute on the socket only", d)
	}
	for verb, gate := range map[string]string{"relay credential mint": "credential.mint", "relay credential revoke": "credential.revoke"} {
		if d := get("cli", verb); !d.OwnerGated || !has(d.Gates, gate) {
			t.Fatalf("%s is %+v, want gated by %s", verb, d, gate)
		}
	}
	if d := get("cli", "relay status"); d.Credential != "operator" || d.OwnerGated || !has(d.Calls, "admin_op:status.view") {
		t.Fatalf("relay status is %+v, want class operator, ungated, reaching admin_op:status.view", d)
	}

	ev := requireEvent(t, i, harness.EventQuery{Key: "doors.list", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if count, _ := ev["count"].(float64); int(count) != len(doc.Doors) {
		t.Fatalf("doors.list count %v, the document lists %d doors", ev["count"], len(doc.Doors))
	}
}

func TestTraceFlagNamesEvents(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{{Name: "reader", Classes: []string{"read"}}}})

	t.Run("cli", func(t *testing.T) {
		t.Parallel()
		trace := harness.NewTrace(t)
		r := i.CLIWith(harness.CLIOpts{Trace: trace}, "credential", "list")
		if r.Code != 0 {
			t.Fatalf("credential list exited %d", r.Code)
		}
		requireEvent(t, i, harness.EventQuery{Key: "credential.list", Trace: trace, Fields: map[string]any{"status": "ok"}})
		logs := i.CLI("logs", "--json", "--trace", trace)
		lines := g9Lines(t, logs.Stdout)
		if logs.Code != 0 || len(lines) == 0 {
			t.Fatalf("logs --trace %s exited %d with %d lines", trace, logs.Code, len(lines))
		}
		for _, l := range lines {
			if l["trace_id"] != trace {
				t.Fatalf("logs --trace %s printed a line of trace %v", trace, l["trace_id"])
			}
		}
	})

	t.Run("header", func(t *testing.T) {
		t.Parallel()
		trace := harness.NewTrace(t)
		resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil, harness.ReqOpts{Trace: trace})
		if resp.Status != http.StatusOK {
			t.Fatalf("GET /api/projects answered %d", resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "project.list", Trace: trace, Fields: map[string]any{"status": "ok"}})
	})

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		if r := i.CLI("--trace", "short", "credential", "list"); r.Code != 1 {
			t.Fatalf("a trace of 5 characters exited %d, want 1", r.Code)
		}
	})
}

// g9SessionScript runs relay verbs the way a process inside a terminal
// session would, and writes each exit code to a file. The test waits on the
// session's exit event, which follows the last file write.
const g9SessionScript = `relay=$1; cfg=$2; out=$3
env -u RELAY_SESSION_ID "$relay" --config-dir "$cfg" status >/dev/null 2>&1
echo $? > "$out/status.code"
env -u RELAY_SESSION_ID "$relay" --config-dir "$cfg" project create --name acme-intruder --path "$out" >/dev/null 2>&1
echo $? > "$out/create.code"`

func TestOperatorVerbRefusedInSession(t *testing.T) {
	t.Parallel()
	for _, sandboxed := range []bool{false, true} {
		name := "unsandboxed"
		if sandboxed {
			name = "sandboxed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := json.Marshal([]map[string]any{{
				"id": "acme-probe", "name": "Acme probe", "command": "/bin/sh",
				"args": []string{"-c", g9SessionScript, "acme"}, "sandbox": sandboxed,
			}})
			if err != nil {
				t.Fatalf("encoding the template: %v", err)
			}
			i := harness.Start(t, harness.Options{
				Credentials: []harness.CredentialSpec{{Name: "reader", Classes: []string{"read"}}},
				Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
				Settings:    map[string]json.RawMessage{"terminal_templates": tmpl},
			})
			i.WaitSessionHost(g9Deadline)

			dir := filepath.Join(i.Home, "work", "acme-probe")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("creating %s: %v", dir, err)
			}
			createBody, err := json.Marshal(map[string]any{"name": "acme-probe", "path": dir, "allowed_templates": []string{"acme-probe"}})
			if err != nil {
				t.Fatalf("encoding the project body: %v", err)
			}
			var host project
			i.CLIWith(harness.CLIOpts{Stdin: createBody}, "project", "create", "--file", "-", "--json").JSON(t, &host)
			if host.ID == "" {
				t.Fatalf("project create printed no id")
			}

			start := i.MustCLI("terminal", "start", "--project", host.ID, "--template", "acme-probe",
				"--extra-arg", harness.BundlePaths().Relay, "--extra-arg", i.ConfigDir,
				"--extra-arg", dir, "--json")
			launch := requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: start.Trace, Fields: map[string]any{"status": "ok"}})
			i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": launch.Str("session_id")}}, g9Deadline)

			for _, f := range []string{"status.code", "create.code"} {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatalf("reading %s: %v", f, err)
				}
				if strings.TrimSpace(string(b)) == "0" {
					t.Fatalf("%s: the verb run inside a session exited 0", f)
				}
			}
			denied := 0
			for _, row := range g9Rows(i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}), "path", "status.view") {
				if row["method"] == "admin_op" && row["class"] == "operator" {
					denied++
				}
			}
			if denied != 1 {
				t.Fatalf("%d denied admin_op status.view rows, want 1", denied)
			}
			if got := i.Events(harness.EventQuery{Key: "status.view"}); len(got) != 0 {
				t.Fatalf("a refused status verb still wrote %d status.view events", len(got))
			}
			for _, p := range listProjects(t, i) {
				if p.Name == "acme-intruder" {
					t.Fatalf("a project create run inside a session created a project")
				}
			}
		})
	}
}
