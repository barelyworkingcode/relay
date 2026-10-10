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

const g4Scope = "chief-of-staff"

var (
	g4ConfigurerSpec = harness.CredentialSpec{Name: "configurer", Classes: []string{"configure"}}
	g4ReaderSpec     = harness.CredentialSpec{Name: "reader", Classes: []string{"read"}}
	g4RunnerSpec     = harness.CredentialSpec{Name: "runner", Classes: []string{"execute"}}
	g4ChiefSpec      = harness.CredentialSpec{Name: "chief", Classes: []string{"proxy", "execute"}}
)

// g4Row is one audit record as `relay audit --json` prints it.
type g4Row map[string]any

func (r g4Row) str(key string) string {
	s, _ := r[key].(string)
	return s
}

func (r g4Row) sub(key string) map[string]any {
	m, _ := r[key].(map[string]any)
	return m
}

func (r g4Row) actorStr(key string) string {
	s, _ := r.sub("actor")[key].(string)
	return s
}

func (r g4Row) argStr(key string) string {
	s, _ := r.sub("args")[key].(string)
	return s
}

// g4Rows reads audit rows, oldest first, through `relay audit --json`.
func g4Rows(t *testing.T, i *harness.Instance, filter ...string) []g4Row {
	t.Helper()
	args := append([]string{"audit", "--tail", "100000", "--json"}, filter...)
	res := i.MustCLI(args...)
	return g4ParseRows(t, res.Stdout)
}

func g4ParseRows(t *testing.T, raw []byte) []g4Row {
	t.Helper()
	var rows []g4Row
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r g4Row
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("decoding an audit line: %v\n%s", err, line)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning audit output: %v", err)
	}
	return rows
}

func g4OnTrace(rows []g4Row, trace string) []g4Row {
	var out []g4Row
	for _, r := range rows {
		if r.str("trace_id") == trace {
			out = append(out, r)
		}
	}
	return out
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

func g4Index(rows []g4Row, keep func(g4Row) bool) int {
	for n, r := range rows {
		if keep(r) {
			return n
		}
	}
	return -1
}

// g4Project creates a project through the CLI with a --file body.
func g4Project(t *testing.T, i *harness.Instance, name string, extra map[string]any) project {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range extra {
		body[k] = v
	}
	if body["kind"] == "remote" {
		delete(body, "path") // an access profile has no folder
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create for %s returned no id", name)
	}
	return p
}

func g4Token(t *testing.T, i *harness.Instance, projectID string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", projectID, "--json").JSON(t, &out)
	if out.Token == "" {
		t.Fatalf("project token for %s printed no token", projectID)
	}
	return out.Token
}

// g4ToolInstance boots an instance with one stdio fake MCP: acme_ok is the only
// tool a g4ToolProject grants, acme_echo is the one it refuses.
func g4ToolInstance(t *testing.T, o harness.Options) *harness.Instance {
	t.Helper()
	o.FakeMCPs = []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: readOnlyCatalogue()}}
	if o.Presence == nil {
		o.Presence = map[string]harness.Outcome{}
	}
	o.Presence["project.grant"] = harness.OutcomeApprove
	o.Presence["project.reveal_token"] = harness.OutcomeApprove
	o.Credentials = append(o.Credentials, g4ReaderSpec)
	i := harness.Start(t, o)
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)
	return i
}

func g4ToolProject(t *testing.T, i *harness.Instance, name string) (project, string) {
	t.Helper()
	p := g4Project(t, i, name, map[string]any{
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_ok"}},
		"access":          map[string]string{"acme-stdio": "write"},
	})
	return p, g4Token(t, i, p.ID)
}

func g4Call(i *harness.Instance, token, tool string) harness.Result {
	return i.CLI("mcp", "call", "--token", token, "--tool", tool, "--args", "{}")
}

func TestAuditCLIFilters(t *testing.T) {
	t.Parallel()
	i := g4ToolInstance(t, harness.Options{})
	a, tokenA := g4ToolProject(t, i, "acme-a")
	b, tokenB := g4ToolProject(t, i, "acme-b")

	if r := g4Call(i, tokenA, "acme_ok"); r.Code != 0 {
		t.Fatalf("an allowed call exited %d", r.Code)
	}
	if r := g4Call(i, tokenA, "acme_echo"); r.Code != 1 {
		t.Fatalf("a refused call exited %d, want 1", r.Code)
	}
	if r := g4Call(i, tokenB, "acme_ok"); r.Code != 0 {
		t.Fatalf("project B's allowed call exited %d", r.Code)
	}

	byEvent := g4Rows(t, i, "--event", "call_tool")
	if len(byEvent) < 3 {
		t.Fatalf("--event call_tool returned %d rows, want at least 3", len(byEvent))
	}
	var prev time.Time
	for _, r := range byEvent {
		if r.str("event") != "call_tool" {
			t.Fatalf("--event call_tool returned a %q row", r.str("event"))
		}
		ts, err := time.Parse(time.RFC3339Nano, r.str("ts"))
		if err != nil {
			t.Fatalf("row ts %q: %v", r.str("ts"), err)
		}
		if ts.Before(prev) {
			t.Fatalf("--json rows are not oldest first: %v before %v", ts, prev)
		}
		prev = ts
	}
	if len(g4Where(byEvent, func(r g4Row) bool { return r.str("outcome") == "ok" })) == 0 ||
		len(g4Where(byEvent, func(r g4Row) bool { return r.str("outcome") == "denied" })) == 0 {
		t.Fatalf("--event call_tool lacks an ok row or a denied row: %v", byEvent)
	}

	denied := g4Rows(t, i, "--outcome", "denied")
	if len(denied) == 0 {
		t.Fatalf("--outcome denied returned no rows")
	}
	for _, r := range denied {
		if r.str("outcome") != "denied" {
			t.Fatalf("--outcome denied returned a %q row", r.str("outcome"))
		}
	}

	forA := g4Rows(t, i, "--project", a.ID)
	if len(g4Where(forA, func(r g4Row) bool { return r.str("event") == "call_tool" })) < 2 {
		t.Fatalf("--project %s returned fewer than its 2 call_tool rows: %v", a.ID, forA)
	}
	for _, r := range forA {
		if r.actorStr("project_id") != a.ID {
			t.Fatalf("--project %s returned a row for project %q", a.ID, r.actorStr("project_id"))
		}
	}
	forB := g4Rows(t, i, "--project", b.ID, "--event", "call_tool")
	if len(forB) != 1 {
		t.Fatalf("--project %s --event call_tool returned %d rows, want 1", b.ID, len(forB))
	}
}

func TestAuditQueryHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{g4ReaderSpec}})
	reader := i.HTTP(i.Credential("reader"))
	// Each authorized request writes a control_decision row, so the log holds some.
	for n := 0; n < 3; n++ {
		if resp := reader.Do("GET", "/api/projects", nil); resp.Status != 200 {
			t.Fatalf("GET /api/projects answered %d", resp.Status)
		}
	}

	resp := reader.Do("GET", "/api/audit?event=control_decision", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/audit answered %d, want 200", resp.Status)
	}
	var rows []g4Row
	resp.JSON(t, &rows)
	if len(rows) < 3 {
		t.Fatalf("GET /api/audit?event=control_decision returned %d rows, want at least 3", len(rows))
	}
	for _, r := range rows {
		if r.str("event") != "control_decision" {
			t.Fatalf("the event filter returned a %q row", r.str("event"))
		}
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "audit.query", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
	if got, _ := ev["count"].(float64); int(got) != len(rows) {
		t.Fatalf("audit.query count %v, response held %d rows", ev["count"], len(rows))
	}

	if anon := i.Anonymous().Do("GET", "/api/audit", nil); anon.Status != 401 {
		t.Fatalf("GET /api/audit with no credential answered %d, want 401", anon.Status)
	}
}

func g4SHA(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func TestToolCallAuditRows(t *testing.T) {
	t.Parallel()
	i := g4ToolInstance(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		Presence: map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove},
	})
	_, token := g4ToolProject(t, i, "acme-local")

	ok := g4Call(i, token, "acme_ok")
	if ok.Code != 0 {
		t.Fatalf("an allowed call exited %d", ok.Code)
	}
	refused := g4Call(i, token, "acme_echo")
	if refused.Code != 1 {
		t.Fatalf("a refused call exited %d, want 1", refused.Code)
	}
	calls := g4Rows(t, i, "--event", "call_tool")
	if got := g4OnTrace(calls, ok.Trace); len(got) != 1 || got[0].str("outcome") != "ok" || got[0].str("phase") != "" {
		t.Fatalf("the allowed call left %v on its trace, want exactly one single-record ok row", got)
	}
	if got := g4OnTrace(calls, refused.Trace); len(got) != 1 || got[0].str("outcome") != "denied" {
		t.Fatalf("the refused call left %v on its trace, want exactly one denied row", got)
	}

	profile := g4Project(t, i, "acme-profile", map[string]any{
		"kind":            "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_ok"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	identity := harness.NewRemoteIdentity(t, "acme-laptop")
	out := filepath.Join(i.Dir, "signed")
	if r := i.CLIWith(harness.CLIOpts{Stdin: identity.CSRPEM()}, "enrol", "sign", "--client-id", "acme-client", "--csr", "-", "--grant", profile.ID, "--out", out); r.Code != 0 {
		t.Fatalf("enrol sign exited %d", r.Code)
	}
	conn := i.RemoteDial(identity, mustRead(t, filepath.Join(out, "client.crt")), mustRead(t, filepath.Join(out, "ca.crt")))
	reply, alive := conn.Send(map[string]any{
		"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{},
		"project_id": profile.ID, "args_sha256": g4SHA("{}"),
	})
	if !alive || reply["type"] != "Result" {
		t.Fatalf("remote CallTool answered alive=%v %v, want a Result", alive, reply)
	}

	remote := g4Rows(t, i, "--kind", "remote", "--event", "call_tool")
	if len(remote) != 2 {
		t.Fatalf("a remote call left %d call_tool rows, want an intent and a completion: %v", len(remote), remote)
	}
	intent, completion := remote[0], remote[1]
	if intent.str("phase") != "intent" || intent.str("outcome") != "pending" {
		t.Fatalf("first remote row phase %q outcome %q, want intent/pending", intent.str("phase"), intent.str("outcome"))
	}
	if completion.str("phase") != "completion" || completion.str("outcome") != "ok" {
		t.Fatalf("second remote row phase %q outcome %q, want completion/ok", completion.str("phase"), completion.str("outcome"))
	}
	if intent.str("id") == "" || intent.str("id") != completion.str("id") {
		t.Fatalf("intent id %q and completion id %q differ", intent.str("id"), completion.str("id"))
	}
}

func TestGateRowsCarryPresenceID(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{
		"project.grant":     harness.OutcomeApprove,
		"credential.mint":   harness.OutcomeApprove,
		"credential.revoke": harness.OutcomeApprove,
	}})

	created, _ := createProject(t, i, "acme-gated")
	mint := i.MustCLI("credential", "mint", "--name", "acme-reader", "--class", "read")
	var credID string
	for _, line := range strings.Split(string(mint.Stdout), "\n") {
		if rest, found := strings.CutPrefix(line, "  id:"); found {
			credID = strings.TrimSpace(rest)
		}
	}
	if credID == "" {
		t.Fatalf("credential mint printed no id line")
	}
	i.MustCLI("credential", "revoke", "--id", credID)

	rows := g4Rows(t, i)
	approval := func(op string) g4Row {
		got := g4Where(rows, func(r g4Row) bool {
			return r.str("event") == "control_decision" && r.str("method") == op && r.str("outcome") == "ok"
		})
		if len(got) != 1 || got[0].str("presence_id") == "" {
			t.Fatalf("%s left %d ok control_decision rows with presence ids: %v", op, len(got), got)
		}
		return got[0]
	}
	cases := []struct{ op, event, subject string }{
		{"project.grant", "config_change", created.ID},
		{"credential.mint", "credential_issued", credID},
		{"credential.revoke", "credential_revoked", credID},
	}
	for _, c := range cases {
		want := approval(c.op).str("presence_id")
		got := g4Where(rows, func(r g4Row) bool { return r.str("event") == c.event && r.str("subject") == c.subject })
		if len(got) != 1 {
			t.Fatalf("%s: %d %s rows for %s, want 1", c.op, len(got), c.event, c.subject)
		}
		if got[0].str("outcome") != "ok" || got[0].str("presence_id") != want {
			t.Fatalf("%s row outcome %q presence_id %q, want ok and %q", c.event, got[0].str("outcome"), got[0].str("presence_id"), want)
		}
	}
}

// g4SessionInstance boots an instance whose session host is up, with an
// execute credential and a project allowing the claude-code template.
func g4SessionInstance(t *testing.T, o harness.Options) (*harness.Instance, project) {
	t.Helper()
	o.Credentials = append(o.Credentials, g4RunnerSpec, g4ReaderSpec)
	if o.Presence == nil {
		o.Presence = map[string]harness.Outcome{}
	}
	o.Presence["project.grant"] = harness.OutcomeApprove
	i := harness.Start(t, o)
	i.WaitSessionHost(60 * time.Second)
	p := g4Project(t, i, "acme-sessions", map[string]any{"allowed_templates": []string{"claude-code", "shell"}})
	return i, p
}

func TestSessionLaunchRows(t *testing.T) {
	t.Parallel()
	i, p := g4SessionInstance(t, harness.Options{})
	runner := i.SocketHTTP(i.Credential("runner"))

	claude := runner.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "sonnet"})
	if claude.Status != 201 {
		t.Fatalf("POST /api/sessions answered %d, want 201", claude.Status)
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	claude.JSON(t, &session)
	shell := runner.Do("POST", "/api/terminals", map[string]any{"projectId": p.ID, "templateId": "shell"})
	if shell.Status != 201 {
		t.Fatalf("POST /api/terminals answered %d, want 201", shell.Status)
	}
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	shell.JSON(t, &term)

	launches := g4Rows(t, i, "--event", "session_launch")
	for _, c := range []struct{ id, trace, kind string }{
		{session.SessionID, claude.Trace, "claude"},
		{term.TerminalID, shell.Trace, ""},
	} {
		got := g4Where(launches, func(r g4Row) bool { return r.argStr("session_id") == c.id })
		if len(got) != 1 {
			t.Fatalf("launch %s left %d session_launch rows, want 1: %v", c.id, len(got), launches)
		}
		row := got[0]
		if row.str("outcome") != "ok" || row.actorStr("project_id") != p.ID {
			t.Fatalf("launch %s row outcome %q project %q, want ok and %s", c.id, row.str("outcome"), row.actorStr("project_id"), p.ID)
		}
		launchEvent := requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: c.trace, Fields: map[string]any{"status": "ok"}})
		kind := c.kind
		if kind == "" {
			kind = launchEvent.Str("kind")
		}
		if kind == "" || row.argStr("session_kind") != kind {
			t.Fatalf("launch %s row session_kind %q, want %q", c.id, row.argStr("session_kind"), kind)
		}
	}
}

func TestRefusalRows(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{g4RunnerSpec, g4ReaderSpec},
		Presence:    approveGrant,
	})
	i.WaitSessionHost(60 * time.Second)
	p, _ := createProject(t, i, "acme-narrow") // allows no template

	refused := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/terminals", map[string]any{"projectId": p.ID, "templateId": "shell"})
	if refused.Status != 403 {
		t.Fatalf("a launch of a template the project does not allow answered %d, want 403", refused.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: refused.Trace, Fields: map[string]any{"status": "denied", "reason": "template_not_allowed"}})

	launches := g4Rows(t, i, "--event", "session_launch", "--outcome", "denied")
	if len(launches) != 1 || launches[0].actorStr("project_id") != p.ID {
		t.Fatalf("the refused launch left %v, want one denied session_launch row for %s", launches, p.ID)
	}

	forbidden := i.HTTP(i.Credential("reader")).Do("POST", "/api/projects", map[string]any{"name": "acme-x", "path": filepath.Join(i.Home, "x")})
	if forbidden.Status != 403 {
		t.Fatalf("a read credential on POST /api/projects answered %d, want 403", forbidden.Status)
	}
	decisions := g4Where(g4Rows(t, i, "--event", "control_decision", "--outcome", "denied"), func(r g4Row) bool {
		return r.str("method") == "POST" && r.str("path") == "/api/projects"
	})
	if len(decisions) != 1 {
		t.Fatalf("the refused control-plane call left %d denied control_decision rows with its path, want 1", len(decisions))
	}
}

func TestApproverAnswerRowsBeforeAct(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	approved, _ := createProject(t, i, "acme-approved")

	rows := g4Rows(t, i)
	isGrant := func(outcome string) func(g4Row) bool {
		return func(r g4Row) bool {
			return r.str("event") == "control_decision" && r.str("method") == "project.grant" && r.str("outcome") == outcome
		}
	}
	answer := g4Index(rows, isGrant("ok"))
	act := g4Index(rows, func(r g4Row) bool { return r.str("event") == "config_change" && r.str("subject") == approved.ID })
	if answer < 0 || act < 0 || answer > act {
		t.Fatalf("approval row at %d and config_change at %d, want both present and the approval first", answer, act)
	}
	if rows[answer].str("presence_approver") == "" {
		t.Fatalf("the approval row has no presence_approver: %v", rows[answer])
	}

	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	dir := filepath.Join(i.Home, "work", "acme-denied")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if r := i.CLI("project", "create", "--name", "acme-denied", "--path", dir, "--json"); r.Code != 1 {
		t.Fatalf("a denied project create exited %d, want 1", r.Code)
	}
	after := g4Rows(t, i)
	refusal := g4Where(after, isGrant("denied"))
	if len(refusal) != 1 || refusal[0].str("presence_approver") == "" {
		t.Fatalf("the denied create left %v, want one denied control_decision with presence_approver", refusal)
	}
	if n := len(g4Where(after, func(r g4Row) bool { return r.str("event") == "config_change" })); n != 1 {
		t.Fatalf("after a denied create there are %d config_change rows, want only the approved one", n)
	}
}

func g4ChiefDo(i *harness.Instance, method, path string, body any) harness.Response {
	return i.SocketHTTP(i.Credential("chief")).Do(method, path, body, harness.ReqOpts{
		Header: http.Header{"X-Relay-Scope": {g4Scope}},
	})
}

func TestChiefOfStaffSendRows(t *testing.T) {
	t.Parallel()
	t.Run("recorded", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: []harness.CredentialSpec{g4ChiefSpec}})
		i.WaitSessionHost(60 * time.Second)
		sessionID := startAgentSession(t, i, "claude-code", "sonnet")

		resp := g4ChiefDo(i, "POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": sessionID, "text": "status please"})
		if resp.Status != 202 {
			t.Fatalf("a scoped send answered %d, want 202", resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.send", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "session_id": sessionID, "origin": g4Scope}})

		rows := g4Where(g4Rows(t, i, "--event", "session_message"), func(r g4Row) bool { return r.argStr("session_id") == sessionID })
		if len(rows) != 2 {
			t.Fatalf("a scoped send left %d session_message rows, want an intent and a completion: %v", len(rows), rows)
		}
		if rows[0].str("phase") != "intent" || rows[0].str("outcome") != "pending" {
			t.Fatalf("first row phase %q outcome %q, want intent/pending", rows[0].str("phase"), rows[0].str("outcome"))
		}
		if rows[1].str("phase") != "completion" || rows[1].str("outcome") != "ok" {
			t.Fatalf("second row phase %q outcome %q, want completion/ok", rows[1].str("phase"), rows[1].str("outcome"))
		}
		if rows[0].str("id") == "" || rows[0].str("id") != rows[1].str("id") {
			t.Fatalf("intent id %q and completion id %q differ", rows[0].str("id"), rows[1].str("id"))
		}
		if rows[0].argStr("origin") != g4Scope {
			t.Fatalf("intent origin %q, want %s", rows[0].argStr("origin"), g4Scope)
		}
	})

	t.Run("refused_unrecorded", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{
			Settings:    map[string]json.RawMessage{"audit": json.RawMessage(`{"enabled":false}`)},
			Credentials: []harness.CredentialSpec{g4ChiefSpec},
		})
		i.WaitSessionHost(60 * time.Second) // a 503 before the host registers would pass for the wrong cause
		resp := g4ChiefDo(i, "POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": "acme-session-1", "text": "status please"})
		if resp.Status != 503 {
			t.Fatalf("a scoped send with auditing off answered %d, want 503", resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.send", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "audit_unavailable"}})
	})
}

func TestChiefOfStaffStartRows(t *testing.T) {
	t.Parallel()
	const prompt = "list the open tasks"

	t.Run("local", func(t *testing.T) {
		t.Parallel()
		i, p := g4SessionInstance(t, harness.Options{Credentials: []harness.CredentialSpec{g4ChiefSpec}})
		resp := g4ChiefDo(i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": p.ID, "prompt": prompt, "model": "sonnet"})
		if resp.Status != 201 {
			t.Fatalf("a scoped start answered %d, want 201", resp.Status)
		}
		var started struct {
			SessionID string `json:"sessionId"`
		}
		resp.JSON(t, &started)
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "session_id": started.SessionID, "project_id": p.ID}})

		rows := g4Where(g4Rows(t, i, "--event", "session_launch"), func(r g4Row) bool { return r.argStr("session_id") == started.SessionID })
		if len(rows) != 1 {
			t.Fatalf("a scoped start left %d session_launch rows, want 1: %v", len(rows), rows)
		}
		args := rows[0].sub("args")
		if rows[0].str("outcome") != "ok" || args["origin"] != g4Scope || args["prompt_bytes"] != float64(len(prompt)) {
			t.Fatalf("launch row outcome %q args %v, want ok, origin %s, prompt_bytes %d", rows[0].str("outcome"), args, g4Scope, len(prompt))
		}
		if _, hosted := args["host_id"]; hosted {
			t.Fatalf("a console project's launch row carries host_id: %v", args)
		}
	})

	t.Run("hosted", func(t *testing.T) {
		t.Parallel()
		host := harness.StartSSHHost(t)
		i, _ := g4SessionInstance(t, harness.Options{Credentials: []harness.CredentialSpec{g4ChiefSpec, g4ConfigurerSpec}})
		i.TrustSSHHost(host)
		create := i.HTTP(i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{
			"name": "acme-box", "target": host.Target, "port": host.Port, "identity_file": host.IdentityFile,
		})
		if create.Status != 201 {
			t.Fatalf("POST /api/hosts answered %d, want 201", create.Status)
		}
		var h struct {
			ID string `json:"id"`
		}
		create.JSON(t, &h)
		hosted := g4Project(t, i, "acme-hosted", map[string]any{"host_id": h.ID, "allowed_templates": []string{"claude-code"}})

		resp := g4ChiefDo(i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": hosted.ID, "prompt": prompt, "model": "sonnet"})
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace})
		rows := g4Where(g4Rows(t, i, "--event", "session_launch"), func(r g4Row) bool { return r.argStr("host_id") == h.ID })
		if len(rows) != 1 {
			t.Fatalf("a hosted start (HTTP %d) left %d session_launch rows with host_id %s, want 1", resp.Status, len(rows), h.ID)
		}
		if rows[0].argStr("origin") != g4Scope || rows[0].sub("args")["prompt_bytes"] != float64(len(prompt)) {
			t.Fatalf("hosted launch row args %v, want origin %s and prompt_bytes %d", rows[0].sub("args"), g4Scope, len(prompt))
		}
	})

	t.Run("refused_unrecorded", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{
			Settings:    map[string]json.RawMessage{"audit": json.RawMessage(`{"enabled":false}`)},
			Credentials: []harness.CredentialSpec{g4ChiefSpec},
		})
		i.WaitSessionHost(60 * time.Second) // a 503 before the host registers would pass for the wrong cause
		resp := g4ChiefDo(i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": "acme-project-1", "prompt": prompt, "model": "sonnet"})
		if resp.Status != 503 {
			t.Fatalf("a scoped start with auditing off answered %d, want 503", resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "audit_unavailable"}})
	})
}

var g4ExportName = regexp.MustCompile(`^toolcalls-export-.+\.jsonl$`)

func TestAuditExport(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{g4ConfigurerSpec, g4ReaderSpec}})
	for n := 0; n < 3; n++ {
		if resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil); resp.Status != 200 {
			t.Fatalf("GET /api/projects answered %d", resp.Status)
		}
	}

	resp := i.HTTP(i.Credential("configurer")).Do("POST", "/api/audit/export", map[string]any{"event": "control_decision"})
	if resp.Status != 201 {
		t.Fatalf("POST /api/audit/export answered %d, want 201", resp.Status)
	}
	var out struct {
		Path string `json:"path"`
	}
	resp.JSON(t, &out)
	if !g4ExportName.MatchString(filepath.Base(out.Path)) {
		t.Fatalf("export path %q is not toolcalls-export-<timestamp>.jsonl", out.Path)
	}
	logPath := strings.TrimSpace(string(i.MustCLI("audit", "--path").Stdout))
	if filepath.Dir(out.Path) != filepath.Dir(logPath) {
		t.Fatalf("export %q is not beside the audit log %q", out.Path, logPath)
	}
	exported := g4ParseRows(t, mustRead(t, out.Path))
	if len(exported) < 3 {
		t.Fatalf("the export holds %d rows, want at least the 3 reads", len(exported))
	}
	for _, r := range exported {
		if r.str("event") != "control_decision" {
			t.Fatalf("the filtered export holds a %q row", r.str("event"))
		}
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "audit.export", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
	if got, _ := ev["count"].(float64); int(got) != len(exported) {
		t.Fatalf("audit.export count %v, the file holds %d rows", ev["count"], len(exported))
	}
}

func TestAuditRevealPath(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{g4ReaderSpec}})
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/audit/log", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/audit/log answered %d, want 200", resp.Status)
	}
	var out struct {
		Path string `json:"path"`
	}
	resp.JSON(t, &out)
	if out.Path == "" {
		t.Fatalf("GET /api/audit/log answered no path")
	}
	if _, err := os.Stat(out.Path); err != nil {
		t.Fatalf("the audit file %s does not exist: %v", out.Path, err)
	}
	if cli := strings.TrimSpace(string(i.MustCLI("audit", "--path").Stdout)); cli != out.Path {
		t.Fatalf("relay audit --path printed %q, the route answered %q", cli, out.Path)
	}
	requireEvent(t, i, harness.EventQuery{Key: "audit.path.get", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
}
