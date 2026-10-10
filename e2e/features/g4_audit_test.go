package features

import (
	"bufio"
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

const g4Deadline = 60 * time.Second

var g4Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "runner", Classes: []string{"execute"}},
	{Name: "cos", Classes: []string{"proxy", "execute"}},
}

var g4Approve = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.rotate_token": harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
	"credential.mint":      harness.OutcomeApprove,
	"credential.revoke":    harness.OutcomeApprove,
	"mcp.register":         harness.OutcomeApprove,
	"enrolment.sign":       harness.OutcomeApprove,
	"enrolment.revoke":     harness.OutcomeApprove,
}

const g4Remote = `{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true,"enrolment_listen":"127.0.0.1:0"}`

// g4Row is one audit record as `relay audit --json` prints it.
type g4Row map[string]any

func (r g4Row) str(k string) string { s, _ := r[k].(string); return s }

func (r g4Row) obj(k string) g4Row { m, _ := r[k].(map[string]any); return g4Row(m) }

func (r g4Row) actorProject() string { return r.obj("actor").str("project_id") }

func g4Parse(t *testing.T, b []byte) []g4Row {
	t.Helper()
	var rows []g4Row
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r g4Row
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("audit line does not decode: %v\n%s", err, line)
		}
		rows = append(rows, r)
	}
	return rows
}

// g4Audit runs `relay audit --json` with the filters and fails t unless it exits 0.
func g4Audit(t *testing.T, i *harness.Instance, flags ...string) []g4Row {
	t.Helper()
	res := i.CLI(append([]string{"audit", "--tail", "100000", "--json"}, flags...)...)
	if res.Code != 0 {
		t.Fatalf("relay audit %v exited %d\nstderr: %s", flags, res.Code, res.Stderr)
	}
	return g4Parse(t, res.Stdout)
}

// g4Wait reads the audit log until ok accepts the rows. A row of a finished
// call is handed to the log's writer after the call answers, so a read may
// precede it; each pass costs one process, and the deadline is the bound.
func g4Wait(t *testing.T, i *harness.Instance, ok func([]g4Row) bool, flags ...string) []g4Row {
	t.Helper()
	deadline := time.Now().Add(g4Deadline)
	for {
		res := i.CLI(append([]string{"audit", "--tail", "100000", "--json"}, flags...)...)
		var rows []g4Row
		if res.Code == 0 {
			rows = g4Parse(t, res.Stdout)
		}
		if ok(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("no audit rows satisfied the wait for %v; last read had %d rows", flags, len(rows))
		}
	}
}

func g4Where(rows []g4Row, keep func(g4Row) bool) []g4Row {
	var out []g4Row
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func g4Has(keep func(g4Row) bool) func([]g4Row) bool {
	return func(rows []g4Row) bool { return len(g4Where(rows, keep)) > 0 }
}

func g4Num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func g4Catalogue() harness.Catalogue {
	return harness.Catalogue{Tools: []json.RawMessage{
		json.RawMessage(`{"name":"acme_echo","description":"Echo the arguments","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true}}`),
		json.RawMessage(`{"name":"acme_ok","description":"Say ok","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true,"openWorldHint":false}}`),
	}}
}

func g4MCPs() []harness.FakeMCPSpec {
	return []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: g4Catalogue()}}
}

func g4WaitMCP(i *harness.Instance) {
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, g4Deadline)
}

// g4Project creates a local project (or, with extra, anything project create
// takes) through the CLI and returns its id.
func g4Project(t *testing.T, i *harness.Instance, name string, extra map[string]any) string {
	t.Helper()
	dir := filepath.Join(i.Home, "work", strings.ReplaceAll(strings.ToLower(name), " ", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir, "allowed_templates": []string{"*"}}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project: %v", err)
	}
	res := i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", res.Stdout)
	}
	return p.ID
}

func g4Token(t *testing.T, i *harness.Instance, projectID string) string {
	t.Helper()
	res := i.MustCLI("project", "token", "--id", projectID, "--json")
	var tok struct {
		Token string `json:"token"`
	}
	res.JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}
	return tok.Token
}

func g4Call(i *harness.Instance, token, tool string) harness.Result {
	return i.CLI("mcp", "call", "--token", token, "--tool", tool, "--args", "{}")
}

// g4Seed makes two projects on the fake MCP and three local calls: project A
// calls acme_ok (ok) and acme_echo (disabled for A, so denied); project B
// calls acme_ok (ok). It returns once all three rows are in the log.
func g4Seed(t *testing.T, i *harness.Instance) (a, b string) {
	t.Helper()
	g4WaitMCP(i)
	a = g4Project(t, i, "Acme A", map[string]any{
		"allowed_mcp_ids": []string{"acme-stdio"},
		"disabled_tools":  map[string][]string{"acme-stdio": {"acme_echo"}},
	})
	b = g4Project(t, i, "Acme B", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	ta, tb := g4Token(t, i, a), g4Token(t, i, b)
	if r := g4Call(i, ta, "acme_ok"); r.Code != 0 {
		t.Fatalf("an allowed call exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	if r := g4Call(i, ta, "acme_echo"); r.Code == 0 {
		t.Fatalf("a call to a disabled tool exited 0")
	}
	if r := g4Call(i, tb, "acme_ok"); r.Code != 0 {
		t.Fatalf("an allowed call exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	g4Wait(t, i, func(rows []g4Row) bool { return len(rows) >= 3 }, "--event", "call_tool")
	return a, b
}

func g4Mint(t *testing.T, i *harness.Instance, name string, classes ...string) (id, token string) {
	t.Helper()
	args := []string{"credential", "mint", "--name", name}
	for _, c := range classes {
		args = append(args, "--class", c)
	}
	res := i.CLI(args...)
	if res.Code != 0 {
		t.Fatalf("credential mint exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "id:" {
			id = f[1]
		}
		if len(f) == 2 && f[0] == "token:" {
			token = f[1]
		}
	}
	if id == "" || token == "" {
		t.Fatalf("credential mint printed no id or token")
	}
	return id, token
}

func g4Times(t *testing.T, rows []g4Row) []time.Time {
	t.Helper()
	var out []time.Time
	for _, r := range rows {
		ts, err := time.Parse(time.RFC3339Nano, r.str("ts"))
		if err != nil {
			t.Fatalf("row ts %q does not parse: %v", r.str("ts"), err)
		}
		out = append(out, ts)
	}
	return out
}

func g4IDs(rows []g4Row) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.str("id")] = true
	}
	return m
}

func TestAuditCLIFilters(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, FakeMCPs: g4MCPs()})
	a, b := g4Seed(t, i)

	calls := g4Audit(t, i, "--event", "call_tool")
	if len(calls) != 3 {
		t.Fatalf("--event call_tool printed %d rows, want 3", len(calls))
	}
	for _, r := range calls {
		if r.str("event") != "call_tool" {
			t.Fatalf("--event call_tool printed a %q row", r.str("event"))
		}
	}
	times := g4Times(t, calls)
	for n := 1; n < len(times); n++ {
		if times[n].Before(times[n-1]) {
			t.Fatalf("--json rows are not oldest first: row %d is before row %d", n, n-1)
		}
	}

	denied := g4Audit(t, i, "--event", "call_tool", "--outcome", "denied")
	if len(denied) != 1 || denied[0].str("tool") != "acme_echo" || denied[0].actorProject() != a {
		t.Fatalf("--outcome denied printed %v, want the one refused acme_echo call of project A", denied)
	}

	byProject := g4Audit(t, i, "--event", "call_tool", "--project", a)
	if len(byProject) != 2 {
		t.Fatalf("--project A printed %d call rows, want 2", len(byProject))
	}
	for _, r := range byProject {
		if r.actorProject() != a {
			t.Fatalf("--project A printed a row of project %q", r.actorProject())
		}
	}
	if only := g4Audit(t, i, "--event", "call_tool", "--project", b); len(only) != 1 {
		t.Fatalf("--project B printed %d call rows, want 1", len(only))
	}

	byTool := g4Audit(t, i, "--grep", "acme_echo", "--event", "call_tool")
	if len(byTool) != 1 || byTool[0].str("tool") != "acme_echo" {
		t.Fatalf("--grep acme_echo printed %v, want the one acme_echo call", byTool)
	}
	if byMCP := g4Audit(t, i, "--mcp", "acme-stdio", "--outcome", "ok"); len(byMCP) != 2 {
		t.Fatalf("--mcp acme-stdio --outcome ok printed %d rows, want 2", len(byMCP))
	}

	res := i.CLI("audit", "--json", "--tail", "1", "--event", "call_tool")
	if res.Code != 0 {
		t.Fatalf("--tail 1 exited %d", res.Code)
	}
	last := g4Parse(t, res.Stdout)
	if len(last) != 1 || last[0].str("id") != calls[len(calls)-1].str("id") {
		t.Fatalf("--tail 1 printed %v, want the newest call row", last)
	}

	for _, flags := range [][]string{{"--event", "call_tool", "--grep", "no-such-text-acme"}, {"--outcome", "no-such-outcome"}} {
		none := i.CLI(append([]string{"audit", "--json"}, flags...)...)
		if none.Code != 0 || len(g4Parse(t, none.Stdout)) != 0 {
			t.Fatalf("relay audit %v exited %d with %q, want exit 0 and no rows", flags, none.Code, none.Stdout)
		}
	}
	if text := i.CLI("audit", "--event", "call_tool"); text.Code != 0 {
		t.Fatalf("relay audit in table mode exited %d", text.Code)
	}
}

func TestAuditQueryHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, FakeMCPs: g4MCPs(), Credentials: g4Creds})
	a, _ := g4Seed(t, i)
	reader := i.HTTP(i.Credential("reader"))

	query := func(path string) ([]g4Row, string) {
		trace := harness.NewTrace(t)
		resp := reader.Do("GET", path, nil, harness.ReqOpts{Trace: trace})
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s answered %d, want 200; body: %s", path, resp.Status, resp.Body)
		}
		var rows []g4Row
		resp.JSON(t, &rows)
		return rows, trace
	}

	rows, trace := query("/api/audit?event=call_tool")
	if len(rows) != 3 {
		t.Fatalf("event=call_tool answered %d rows, want 3", len(rows))
	}
	times := g4Times(t, rows)
	for n := 1; n < len(times); n++ {
		if times[n].After(times[n-1]) {
			t.Fatalf("rows are not newest first: row %d is after row %d", n, n-1)
		}
	}
	i.WaitEvent(harness.EventQuery{Key: "audit.query", Trace: trace, Fields: map[string]any{"status": "ok", "count": 3}}, g4Deadline)

	denied, _ := query("/api/audit?event=call_tool&outcome=denied&project_id=" + a)
	if len(denied) != 1 || denied[0].str("tool") != "acme_echo" {
		t.Fatalf("outcome=denied&project_id=A answered %v, want the one refused acme_echo call", denied)
	}
	if limited, _ := query("/api/audit?event=call_tool&limit=1"); len(limited) != 1 {
		t.Fatalf("limit=1 answered %d rows, want 1", len(limited))
	}

	cli := g4Audit(t, i, "--event", "call_tool", "--project", a)
	http2, _ := query("/api/audit?event=call_tool&project_id=" + a)
	want, got := g4IDs(cli), g4IDs(http2)
	if len(want) != len(got) {
		t.Fatalf("the CLI printed %d rows and the route answered %d for the same filter", len(want), len(got))
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("row %s is in the CLI answer and not in the route's", id)
		}
	}

	for _, bad := range []string{"/api/audit?outcome=no-such-outcome", "/api/audit?limit=many", "/api/audit?kind=no-such-kind"} {
		if resp := reader.Do("GET", bad, nil); resp.Status != http.StatusBadRequest {
			t.Fatalf("GET %s answered %d, want 400", bad, resp.Status)
		}
	}
	if resp := i.Anonymous().Do("GET", "/api/audit", nil); resp.Status != http.StatusUnauthorized {
		t.Fatalf("GET /api/audit with no credential answered %d, want 401", resp.Status)
	}
}

func TestToolCallAuditRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: g4Approve,
		FakeMCPs: g4MCPs(),
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(g4Remote)},
	})
	g4WaitMCP(i)

	t.Run("local", func(t *testing.T) {
		p := g4Project(t, i, "Acme Local", map[string]any{
			"allowed_mcp_ids": []string{"acme-stdio"},
			"disabled_tools":  map[string][]string{"acme-stdio": {"acme_echo"}},
		})
		tok := g4Token(t, i, p)
		if r := g4Call(i, tok, "acme_ok"); r.Code != 0 {
			t.Fatalf("an allowed call exited %d\nstderr: %s", r.Code, r.Stderr)
		}
		if r := g4Call(i, tok, "acme_echo"); r.Code == 0 {
			t.Fatalf("a call to a disabled tool exited 0")
		}
		forProject := func(r g4Row) bool { return r.str("event") == "call_tool" && r.actorProject() == p }
		rows := g4Wait(t, i, func(rows []g4Row) bool { return len(g4Where(rows, forProject)) >= 2 }, "--event", "call_tool")
		rows = g4Where(rows, forProject)
		if len(rows) != 2 {
			t.Fatalf("two local calls wrote %d rows, want 2", len(rows))
		}
		for _, r := range rows {
			if r.str("phase") != "" || r.str("outcome") == "pending" {
				t.Fatalf("a local call wrote a phased or pending row: %v", r)
			}
		}
		byTool := map[string]string{}
		for _, r := range rows {
			byTool[r.str("tool")] = r.str("outcome")
		}
		if byTool["acme_ok"] != "ok" || byTool["acme_echo"] != "denied" {
			t.Fatalf("local rows by tool %v, want acme_ok ok and acme_echo denied", byTool)
		}
	})

	t.Run("remote", func(t *testing.T) {
		profile := g4Profile(t, i)
		identity := harness.NewRemoteIdentity(t, "acme-laptop")
		out := filepath.Join(i.Dir, "signed")
		sign := i.CLIWith(harness.CLIOpts{Stdin: identity.CSRPEM()}, "enrol", "sign", "--client-id", "acme-client", "--csr", "-", "--grant", profile, "--out", out)
		if sign.Code != 0 {
			t.Fatalf("enrol sign exited %d\nstderr: %s", sign.Code, sign.Stderr)
		}
		certPEM, err := os.ReadFile(filepath.Join(out, "client.crt"))
		if err != nil {
			t.Fatalf("reading client.crt: %v", err)
		}
		caPEM, err := os.ReadFile(filepath.Join(out, "ca.crt"))
		if err != nil {
			t.Fatalf("reading ca.crt: %v", err)
		}
		conn := i.RemoteDial(identity, certPEM, caPEM)
		defer conn.Close()

		sum := sha256.Sum256([]byte("{}"))
		call := func(tool string) map[string]any {
			reply, ok := conn.Send(map[string]any{
				"type": "CallTool", "name": tool, "arguments": map[string]any{},
				"project_id": profile, "args_sha256": hex.EncodeToString(sum[:]),
			})
			if !ok {
				t.Fatalf("relay closed the connection on a %s call", tool)
			}
			return reply
		}
		if reply := call("acme_ok"); reply["type"] != "Result" {
			t.Fatalf("an allowed remote call answered %v, want a Result", reply)
		}
		if reply := call("acme_echo"); reply["type"] != "Error" {
			t.Fatalf("a remote call to a tool outside the read grant answered %v, want an Error", reply)
		}

		remote := func(tool string) func(g4Row) bool {
			return func(r g4Row) bool {
				return r.str("event") == "call_tool" && r.obj("actor").str("kind") == "remote" && r.str("tool") == tool
			}
		}
		rows := g4Wait(t, i, func(rows []g4Row) bool {
			return len(g4Where(rows, remote("acme_ok"))) >= 2 && len(g4Where(rows, remote("acme_echo"))) >= 1
		}, "--event", "call_tool", "--kind", "remote")

		pair := g4Where(rows, remote("acme_ok"))
		if len(pair) != 2 {
			t.Fatalf("one remote call wrote %d rows, want an intent and a completion", len(pair))
		}
		intent, completion := pair[0], pair[1]
		if intent.str("phase") != "intent" || intent.str("outcome") != "pending" {
			t.Fatalf("the first remote row is %v, want phase intent, outcome pending", intent)
		}
		if completion.str("phase") != "completion" || completion.str("outcome") != "ok" {
			t.Fatalf("the second remote row is %v, want phase completion, outcome ok", completion)
		}
		if intent.str("id") == "" || intent.str("id") != completion.str("id") {
			t.Fatalf("intent id %q and completion id %q differ", intent.str("id"), completion.str("id"))
		}
		refused := g4Where(rows, remote("acme_echo"))
		sawDenied := false
		for _, r := range refused {
			if r.str("outcome") == "ok" {
				t.Fatalf("a refused remote call wrote an ok row: %v", r)
			}
			sawDenied = sawDenied || r.str("outcome") == "denied"
		}
		if !sawDenied {
			t.Fatalf("a refused remote call wrote no denied row: %v", refused)
		}
	})
}

// g4Profile creates a read-only access profile on the fake MCP and returns its id.
func g4Profile(t *testing.T, i *harness.Instance) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": "acme-profile", "kind": "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_*"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	if err != nil {
		t.Fatalf("encoding the access profile: %v", err)
	}
	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create (profile) exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	return p.ID
}

func TestGateRowsCarryPresenceID(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve})

	g4Project(t, i, "Acme Gate", nil)
	credID, _ := g4Mint(t, i, "acme-minted", "read")
	i.MustCLI("credential", "revoke", "--id", credID)
	rotateTarget := g4Project(t, i, "Acme Rotate", nil)
	i.MustCLI("project", "rotate-token", "--id", rotateTarget, "--json")
	command, args := i.FakeMCPCommand(harness.FakeMCPSpec{ID: "acme-reg", Transport: "stdio", Catalogue: g4Catalogue()})
	reg := []string{"mcp", "register", "--id", "acme-reg", "--name", "Acme Reg", "--command", command}
	for _, a := range args {
		reg = append(reg, "--args", a)
	}
	i.MustCLI(reg...)

	approvals := func(op string) func(g4Row) bool {
		return func(r g4Row) bool {
			return r.str("event") == "control_decision" && r.str("outcome") == "ok" && r.str("method") == op && r.str("presence_id") != ""
		}
	}
	rows := g4Wait(t, i, func(rows []g4Row) bool {
		for _, op := range []string{"project.grant", "credential.mint", "credential.revoke", "project.rotate_token", "mcp.register"} {
			if len(g4Where(rows, approvals(op))) == 0 {
				return false
			}
		}
		return len(g4Where(rows, g4Has2("config_change"))) >= 3 && len(g4Where(rows, g4Has2("credential_revoked"))) >= 1
	})

	cases := []struct {
		op, event, credential string
	}{
		{"project.grant", "config_change", ""},
		{"credential.mint", "credential_issued", "api_credential"},
		{"credential.revoke", "credential_revoked", "api_credential"},
		{"project.rotate_token", "credential_issued", "project_token"},
		{"mcp.register", "config_change", ""},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			approved := g4Where(rows, approvals(tc.op))
			if len(approved) == 0 {
				t.Fatalf("no approval row for %s", tc.op)
			}
			joined := false
			for _, ap := range approved {
				pid := ap.str("presence_id")
				for _, r := range rows {
					if r.str("event") == tc.event && r.str("presence_id") == pid && r.str("outcome") == "ok" &&
						(tc.credential == "" || r.str("credential") == tc.credential) {
						joined = true
					}
				}
			}
			if !joined {
				t.Fatalf("no %s row (credential %q) carries the presence id of a %s approval", tc.event, tc.credential, tc.op)
			}
		})
	}
}

func g4Has2(event string) func(g4Row) bool {
	return func(r g4Row) bool { return r.str("event") == event && r.str("presence_id") != "" }
}

func TestSessionLaunchRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	i.WaitSessionHost(g4Deadline)
	p := g4Project(t, i, "Acme Launch", nil)
	runner := i.SocketHTTP(i.Credential("runner"))

	trace := harness.NewTrace(t)
	resp := runner.Do("POST", "/api/sessions", map[string]string{"projectId": p, "model": "sonnet"}, harness.ReqOpts{Trace: trace})
	if resp.Status != http.StatusCreated {
		t.Fatalf("POST /api/sessions answered %d, want 201; body: %s", resp.Status, resp.Body)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	resp.JSON(t, &sess)

	resp = runner.Do("POST", "/api/terminals", map[string]string{"projectId": p, "templateId": "shell"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != http.StatusCreated {
		t.Fatalf("POST /api/terminals answered %d, want 201; body: %s", resp.Status, resp.Body)
	}
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	resp.JSON(t, &term)

	rows := g4Wait(t, i, func(rows []g4Row) bool { return len(rows) >= 2 }, "--event", "session_launch", "--project", p)
	bySession := map[string]g4Row{}
	for _, r := range rows {
		bySession[r.obj("args").str("session_id")] = r
	}
	s, ok := bySession[sess.SessionID]
	if !ok {
		t.Fatalf("no session_launch row names session %s: %v", sess.SessionID, rows)
	}
	if s.str("outcome") != "ok" || s.obj("args").str("session_kind") != "claude" || s.actorProject() != p {
		t.Fatalf("the session row is %v, want outcome ok, session_kind claude, project %s", s, p)
	}
	tm, ok := bySession[term.TerminalID]
	if !ok {
		t.Fatalf("no session_launch row names terminal %s: %v", term.TerminalID, rows)
	}
	if tm.str("outcome") != "ok" || tm.obj("args").str("session_kind") != "pty" || tm.actorProject() != p {
		t.Fatalf("the terminal row is %v, want outcome ok, session_kind pty, project %s", tm, p)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: trace, Fields: map[string]any{"status": "ok"}}, g4Deadline)
}

func TestRefusalRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	i.WaitSessionHost(g4Deadline)
	p := g4Project(t, i, "Acme Refusals", nil)
	narrow := g4Project(t, i, "Acme Narrow", map[string]any{
		"allowed_models":    []string{"haiku"},
		"allowed_templates": []string{"claude-code"},
	})
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})

	t.Run("launch", func(t *testing.T) {
		runner := i.SocketHTTP(i.Credential("runner"))
		refuse := func(path string, body map[string]string) string {
			trace := harness.NewTrace(t)
			resp := runner.Do("POST", path, body, harness.ReqOpts{Trace: trace})
			if resp.Status < 400 || resp.Status >= 500 {
				t.Fatalf("POST %s %v answered %d, want a 4xx", path, body, resp.Status)
			}
			i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: trace, Fields: map[string]any{"status": "denied"}}, g4Deadline)
			return trace
		}
		refuse("/api/sessions", map[string]string{"projectId": narrow, "model": "sonnet"})
		refuse("/api/terminals", map[string]string{"projectId": narrow, "templateId": "shell"})

		rows := g4Wait(t, i, func(rows []g4Row) bool {
			return len(g4Where(rows, func(r g4Row) bool { return r.actorProject() == narrow })) >= 2
		}, "--event", "session_launch", "--outcome", "denied")
		refused := g4Where(rows, func(r g4Row) bool { return r.actorProject() == narrow })
		kinds := map[string]bool{}
		for _, r := range refused {
			if r.str("event") != "session_launch" || r.str("outcome") != "denied" || r.str("error") == "" {
				t.Fatalf("a refused launch row is %v, want session_launch, outcome denied and a reason", r)
			}
			kinds[r.obj("args").str("session_kind")] = true
		}
		if !kinds["claude"] || !kinds["pty"] {
			t.Fatalf("refused launch rows hold kinds %v, want claude and pty", kinds)
		}

		// A blank model is a refusal of the request itself, recorded with a reason.
		refuse("/api/sessions", map[string]string{"projectId": p, "model": ""})
		blank := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.actorProject() == p && r.str("outcome") != "ok" }), "--event", "session_launch")
		b := g4Where(blank, func(r g4Row) bool { return r.actorProject() == p })[0]
		if b.str("error") == "" {
			t.Fatalf("the blank-model launch row carries no reason: %v", b)
		}
	})

	t.Run("class", func(t *testing.T) {
		reader := i.Credential("reader")
		resp := i.HTTP(reader).Do("POST", "/api/projects", map[string]string{"name": "Acme Nope", "path": i.Home})
		if resp.Status != http.StatusForbidden {
			t.Fatalf("POST /api/projects with a read credential answered %d, want 403", resp.Status)
		}
		rows := g4Wait(t, i, g4Has(func(r g4Row) bool {
			return r.obj("actor").str("cred_id") == reader.ID && r.str("outcome") == "denied"
		}), "--event", "control_decision", "--outcome", "denied")
		r := g4Where(rows, func(r g4Row) bool { return r.obj("actor").str("cred_id") == reader.ID })[0]
		if r.str("method") != "POST" || r.str("path") != "/api/projects" || r.str("class") != "configure" || r.str("error") == "" {
			t.Fatalf("the class refusal row is %v, want POST /api/projects, class configure and a reason", r)
		}
	})

	t.Run("presence", func(t *testing.T) {
		create := i.CLIWith(harness.CLIOpts{Stdin: []byte(`{"name":"Acme Denied","path":"` + i.Home + `"}`)}, "project", "create", "--file", "-", "--json")
		if create.Code == 0 {
			t.Fatalf("project create with the prompt denied exited 0")
		}
		rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.str("method") == "project.grant" && r.str("via") == "cli" }),
			"--event", "control_decision", "--outcome", "denied")
		r := g4Where(rows, func(r g4Row) bool { return r.str("method") == "project.grant" && r.str("via") == "cli" })[0]
		if r.str("outcome") != "denied" || r.str("error") == "" {
			t.Fatalf("the presence refusal row is %v, want outcome denied and a reason", r)
		}
	})
}

func TestApproverAnswerRowsBeforeAct(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{
		"project.grant":   harness.OutcomeApprove,
		"credential.mint": harness.OutcomeDeny,
	}})

	p := g4Project(t, i, "Acme Approver", nil)
	if res := i.CLI("credential", "mint", "--name", "acme-denied", "--class", "read"); res.Code == 0 {
		t.Fatalf("credential mint with the op denied in the outcome file exited 0")
	}
	if res := i.CLI("project", "rotate-token", "--id", p); res.Code == 0 {
		t.Fatalf("project rotate-token with the op absent from the outcome file exited 0")
	}

	i.WaitEvent(harness.EventQuery{Key: "debug.presence.answer", Fields: map[string]any{"gated_op": "project.grant", "answer": "approve", "source": "file"}}, g4Deadline)

	rows := g4Wait(t, i, func(rows []g4Row) bool {
		return len(g4Where(rows, func(r g4Row) bool {
			return r.str("event") == "config_change" && r.str("presence_id") != ""
		})) > 0 && len(g4Where(rows, func(r g4Row) bool {
			return r.str("event") == "control_decision" && r.str("outcome") == "denied" && r.str("method") == "project.rotate_token"
		})) > 0
	})

	approvalAt, actAt := -1, -1
	var approval g4Row
	for n, r := range rows {
		if r.str("event") == "control_decision" && r.str("method") == "project.grant" && r.str("outcome") == "ok" && r.str("presence_approver") != "" {
			approvalAt, approval = n, r
		}
	}
	if approvalAt < 0 {
		t.Fatalf("no approval row with presence_approver for project.grant")
	}
	if approval.str("presence_approver") != "testapprover" || approval.str("presence_id") == "" || approval.str("via") != "cli" {
		t.Fatalf("the approval row is %v, want presence_approver testapprover, a presence_id and via cli", approval)
	}
	for n, r := range rows {
		if r.str("event") == "config_change" && r.str("presence_id") == approval.str("presence_id") {
			actAt = n
		}
	}
	if actAt < 0 {
		t.Fatalf("no config_change row carries the approval's presence id")
	}
	if approvalAt > actAt {
		t.Fatalf("the approval row is at position %d, after the act's row at %d", approvalAt, actAt)
	}

	for _, op := range []string{"credential.mint", "project.rotate_token"} {
		refused := g4Where(rows, func(r g4Row) bool {
			return r.str("event") == "control_decision" && r.str("outcome") == "denied" && r.str("method") == op
		})
		if len(refused) == 0 {
			t.Fatalf("no denied control_decision row for %s", op)
		}
		if refused[0].str("presence_approver") != "testapprover" || refused[0].str("via") != "cli" {
			t.Fatalf("the %s refusal row is %v, want presence_approver testapprover and via cli", op, refused[0])
		}
	}
	if issued := g4Where(rows, func(r g4Row) bool { return r.str("event") == "credential_issued" }); len(issued) != 0 {
		t.Fatalf("a denied op still wrote %d credential_issued rows", len(issued))
	}
}

var g4ExportName = regexp.MustCompile(`^toolcalls-export-.+\.jsonl$`)

func TestAuditExport(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	g4Mint(t, i, "acme-one", "read")
	g4Mint(t, i, "acme-two", "read")
	issued := g4Wait(t, i, func(rows []g4Row) bool { return len(rows) >= 2 }, "--event", "credential_issued")

	configurer := i.HTTP(i.Credential("configurer"))
	trace := harness.NewTrace(t)
	resp := configurer.Do("POST", "/api/audit/export", map[string]string{"event": "credential_issued"}, harness.ReqOpts{Trace: trace})
	if resp.Status != http.StatusCreated {
		t.Fatalf("POST /api/audit/export answered %d, want 201; body: %s", resp.Status, resp.Body)
	}
	var out struct {
		Path string `json:"path"`
	}
	resp.JSON(t, &out)
	if !g4ExportName.MatchString(filepath.Base(out.Path)) {
		t.Fatalf("export file %q is not toolcalls-export-<timestamp>.jsonl", filepath.Base(out.Path))
	}
	logPath := strings.TrimSpace(string(i.MustCLI("audit", "--path").Stdout))
	if filepath.Dir(out.Path) != filepath.Dir(logPath) {
		t.Fatalf("export dir %q is not the audit log's dir %q", filepath.Dir(out.Path), filepath.Dir(logPath))
	}
	data, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	exported := g4Parse(t, data)
	if len(exported) != len(issued) {
		t.Fatalf("the export holds %d rows, want the %d credential_issued rows", len(exported), len(issued))
	}
	for _, r := range exported {
		if r.str("event") != "credential_issued" {
			t.Fatalf("the filtered export holds a %q row", r.str("event"))
		}
	}
	i.WaitEvent(harness.EventQuery{Key: "audit.export", Trace: trace, Fields: map[string]any{"status": "ok", "count": len(exported)}}, g4Deadline)

	if r := i.HTTP(i.Credential("reader")).Do("POST", "/api/audit/export", map[string]string{}); r.Status != http.StatusForbidden {
		t.Fatalf("export with a read credential answered %d, want 403", r.Status)
	}
	if r := i.Anonymous().Do("POST", "/api/audit/export", map[string]string{}); r.Status != http.StatusUnauthorized {
		t.Fatalf("export with no credential answered %d, want 401", r.Status)
	}
}

func TestAuditRevealPath(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	g4Mint(t, i, "acme-reveal", "read")
	g4Wait(t, i, func(rows []g4Row) bool { return len(rows) > 0 }, "--event", "credential_issued")

	trace := harness.NewTrace(t)
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/audit/log", nil, harness.ReqOpts{Trace: trace})
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /api/audit/log answered %d, want 200; body: %s", resp.Status, resp.Body)
	}
	var out struct {
		Path string `json:"path"`
	}
	resp.JSON(t, &out)
	want := strings.TrimSpace(string(i.MustCLI("audit", "--path").Stdout))
	if out.Path == "" || out.Path != want {
		t.Fatalf("the route answered path %q, relay audit --path printed %q", out.Path, want)
	}
	if _, err := os.Stat(out.Path); err != nil {
		t.Fatalf("the audit file the route names does not exist: %v", err)
	}
	i.WaitEvent(harness.EventQuery{Key: "audit.path.get", Trace: trace, Fields: map[string]any{"status": "ok"}}, g4Deadline)
	if r := i.Anonymous().Do("GET", "/api/audit/log", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("GET /api/audit/log with no credential answered %d, want 401", r.Status)
	}
}

func g4CoSScope() harness.ReqOpts {
	return harness.ReqOpts{Header: http.Header{"X-Relay-Scope": {"chief-of-staff"}}}
}

func TestChiefOfStaffSendRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	i.WaitSessionHost(g4Deadline)
	p := g4Project(t, i, "Acme Send", nil)

	var s struct {
		SessionID string `json:"sessionId"`
	}
	i.MustCLI("session", "start", "--project", p, "--model", "sonnet", "--json").JSON(t, &s)
	i.MustCLI("session", "message", "--id", s.SessionID, "--text", "hi", "--json")

	const text = "status please"
	opts := g4CoSScope()
	opts.Trace = harness.NewTrace(t)
	resp := i.SocketHTTP(i.Credential("cos")).Do("POST", "/api/chief-of-staff/messages",
		map[string]string{"sessionId": s.SessionID, "text": text}, opts)
	if resp.Status != http.StatusAccepted {
		t.Fatalf("POST /api/chief-of-staff/messages answered %d, want 202; body: %s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.send", Trace: opts.Trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}}, g4Deadline)

	rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.str("phase") == "completion" }), "--event", "session_message")
	if len(rows) != 2 {
		t.Fatalf("one Chief of Staff send (and one typed message) wrote %d session_message rows, want 2", len(rows))
	}
	intent, completion := rows[0], rows[1]
	if intent.str("phase") != "intent" || intent.str("outcome") != "pending" {
		t.Fatalf("the first row is %v, want phase intent, outcome pending", intent)
	}
	if completion.str("phase") != "completion" || completion.str("outcome") != "ok" {
		t.Fatalf("the second row is %v, want phase completion, outcome ok", completion)
	}
	if intent.str("id") == "" || intent.str("id") != completion.str("id") {
		t.Fatalf("intent id %q and completion id %q differ", intent.str("id"), completion.str("id"))
	}
	for _, r := range rows {
		a := r.obj("args")
		if a.str("session_id") != s.SessionID || a.str("origin") != "chief-of-staff" || g4Num(a["text_bytes"]) != len(text) {
			t.Fatalf("row args are %v, want session %s, origin chief-of-staff, text_bytes %d", a, s.SessionID, len(text))
		}
	}
	if intent.obj("args").str("text") != text {
		t.Fatalf("the intent row holds text %q, want %q (audit.log_args is on)", intent.obj("args").str("text"), text)
	}
	if _, ok := completion.obj("args")["text"]; ok {
		t.Fatalf("the completion row carries the message text")
	}
}

func TestChiefOfStaffStartRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g4Approve, Credentials: g4Creds})
	i.WaitSessionHost(g4Deadline)
	p := g4Project(t, i, "Acme Start", nil)
	cos := i.SocketHTTP(i.Credential("cos"))

	start := func(body map[string]string) (harness.Response, string) {
		opts := g4CoSScope()
		opts.Trace = harness.NewTrace(t)
		return cos.Do("POST", "/api/chief-of-staff/sessions", body, opts), opts.Trace
	}

	t.Run("local", func(t *testing.T) {
		resp, trace := start(map[string]string{"projectId": p, "prompt": "  hello team  ", "model": "sonnet"})
		if resp.Status != http.StatusCreated {
			t.Fatalf("POST /api/chief-of-staff/sessions answered %d, want 201; body: %s", resp.Status, resp.Body)
		}
		var s struct {
			SessionID string `json:"sessionId"`
		}
		resp.JSON(t, &s)
		i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.start", Trace: trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}}, g4Deadline)

		rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.obj("args").str("session_id") == s.SessionID }), "--event", "session_launch")
		r := g4Where(rows, func(r g4Row) bool { return r.obj("args").str("session_id") == s.SessionID })[0]
		a := r.obj("args")
		if r.str("outcome") != "ok" || a.str("origin") != "chief-of-staff" || g4Num(a["prompt_bytes"]) != len("hello team") {
			t.Fatalf("the launch row is %v, want outcome ok, origin chief-of-staff, prompt_bytes %d", r, len("hello team"))
		}
		if _, ok := a["host_id"]; ok {
			t.Fatalf("a local start's row carries host_id: %v", a)
		}
		if raw, _ := json.Marshal(r); bytes.Contains(raw, []byte("hello team")) {
			t.Fatalf("the launch row carries the prompt text: %s", raw)
		}
		g4Wait(t, i, func(rows []g4Row) bool {
			return len(g4Where(rows, func(r g4Row) bool {
				return r.obj("args").str("session_id") == s.SessionID && r.str("phase") == "completion"
			})) > 0
		}, "--event", "session_message")
	})

	t.Run("refused", func(t *testing.T) {
		resp, trace := start(map[string]string{"projectId": "no-such-project", "prompt": "hello", "model": "sonnet"})
		if resp.Status != http.StatusForbidden {
			t.Fatalf("a start in an unknown project answered %d, want 403; body: %s", resp.Status, resp.Body)
		}
		i.WaitEvent(harness.EventQuery{Key: "chief_of_staff.start", Trace: trace, Fields: map[string]any{"status": "denied"}}, g4Deadline)
		rows := g4Wait(t, i, g4Has(func(r g4Row) bool {
			return r.str("outcome") == "denied" && r.obj("args").str("origin") == "chief-of-staff"
		}),
			"--event", "session_launch", "--outcome", "denied")
		r := g4Where(rows, func(r g4Row) bool { return r.obj("args").str("origin") == "chief-of-staff" })[0]
		if g4Num(r.obj("args")["prompt_bytes"]) != len("hello") {
			t.Fatalf("the refusal row is %v, want prompt_bytes %d", r, len("hello"))
		}
	})

	t.Run("hosted", func(t *testing.T) {
		host := harness.StartSSHHost(t)
		i.TrustSSHHost(host)
		create := i.HTTP(i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{
			"name": "acme-box", "target": host.Target, "port": host.Port, "identity_file": host.IdentityFile,
		})
		if create.Status != http.StatusCreated {
			t.Fatalf("POST /api/hosts answered %d, want 201; body: %s", create.Status, create.Body)
		}
		var h struct {
			ID string `json:"id"`
		}
		create.JSON(t, &h)
		hosted := g4Project(t, i, "Acme Hosted", map[string]any{"path": "/srv/acme", "host_id": h.ID})

		resp, _ := start(map[string]string{"projectId": hosted, "prompt": "hello", "model": "sonnet", "mode": "terminal"})
		if resp.Status != http.StatusBadRequest {
			t.Fatalf("a terminal start on a hosted project answered %d, want 400; body: %s", resp.Status, resp.Body)
		}
		rows := g4Wait(t, i, g4Has(func(r g4Row) bool { return r.obj("args").str("host_id") == h.ID }), "--event", "session_launch")
		r := g4Where(rows, func(r g4Row) bool { return r.obj("args").str("host_id") == h.ID })[0]
		if r.str("outcome") != "denied" || r.obj("args").str("origin") != "chief-of-staff" {
			t.Fatalf("the hosted refusal row is %v, want outcome denied, origin chief-of-staff and host_id %s", r, h.ID)
		}
	})

	t.Run("audit_off", func(t *testing.T) {
		off := harness.Start(t, harness.Options{
			Settings:    map[string]json.RawMessage{"audit": json.RawMessage(`{"enabled": false}`)},
			Credentials: g4Creds,
		})
		opts := g4CoSScope()
		opts.Trace = harness.NewTrace(t)
		resp := off.SocketHTTP(off.Credential("cos")).Do("POST", "/api/chief-of-staff/sessions",
			map[string]string{"projectId": "acme-project", "prompt": "hello", "model": "sonnet"}, opts)
		if resp.Status != http.StatusServiceUnavailable {
			t.Fatalf("a start with auditing off answered %d, want 503; body: %s", resp.Status, resp.Body)
		}
		var body struct {
			Error string `json:"error"`
		}
		resp.JSON(t, &body)
		if body.Error != "audit_unavailable" {
			t.Fatalf("error %q, want audit_unavailable", body.Error)
		}
		off.WaitEvent(harness.EventQuery{Key: "chief_of_staff.start", Trace: opts.Trace,
			Fields: map[string]any{"status": "denied", "reason": "audit_unavailable"}}, g4Deadline)
	})
}
