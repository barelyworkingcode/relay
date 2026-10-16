package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

var g10Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "granter", Classes: []string{"grant"}},
	{Name: "executer", Classes: []string{"execute"}},
}

func g10Catalogue() harness.Catalogue {
	tool := func(name string) json.RawMessage {
		return json.RawMessage(`{"name":"` + name + `","description":"Say ok","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true,"openWorldHint":false}}`)
	}
	return harness.Catalogue{Tools: []json.RawMessage{tool("acme_ok"), tool("acme_note"), tool("acme_hidden")}}
}

// g10Rig is an instance with the remote listener on and enrolment windows approvable, one fake MCP and one
// access profile that allows acme_ok and acme_note but not acme_hidden.
type g10Rig struct {
	i       *harness.Instance
	profile string
}

func g10Start(t *testing.T, presence map[string]harness.Outcome, mut ...func(*harness.Options)) *g10Rig {
	t.Helper()
	p := map[string]harness.Outcome{"project.grant": harness.OutcomeApprove, "enrolment.window.open": harness.OutcomeApprove}
	for k, v := range presence {
		p[k] = v
	}
	o := harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		Credentials: g10Creds,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: g10Catalogue()}},
		Presence:    p,
	}
	for _, m := range mut {
		m(&o)
	}
	i := harness.Start(t, o)
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	body, err := json.Marshal(map[string]any{
		"name": "acme-profile", "kind": "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_ok", "acme_note"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	if err != nil {
		t.Fatalf("encoding the access profile: %v", err)
	}
	var prof project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &prof)
	if prof.ID == "" {
		t.Fatalf("project create returned no profile id")
	}
	return &g10Rig{i: i, profile: prof.ID}
}

// g10Client is an enrolled remote client: its key and the certificates relay signed for it.
type g10Client struct {
	id       *harness.RemoteIdentity
	clientID string
	cert, ca []byte
	out      string
}

// g10Sign signs a CSR for clientID against the rig's profile. The instance must approve enrolment.sign.
func g10Sign(t *testing.T, r *g10Rig, clientID string, extra ...string) g10Client {
	t.Helper()
	id := harness.NewRemoteIdentity(t, clientID)
	out := filepath.Join(r.i.Dir, "signed-"+clientID)
	args := append([]string{"enrol", "sign", "--client-id", clientID, "--csr", "-", "--grant", r.profile, "--out", out}, extra...)
	if res := r.i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, args...); res.Code != 0 {
		t.Fatalf("enrol sign for %s exited %d\nstderr: %s", clientID, res.Code, res.Stderr)
	}
	return g10Client{id: id, clientID: clientID, out: out,
		cert: mustRead(t, filepath.Join(out, "client.crt")), ca: mustRead(t, filepath.Join(out, "ca.crt"))}
}

func (r *g10Rig) dial(c g10Client) *harness.RemoteConn {
	return r.i.RemoteDial(c.id, c.cert, c.ca)
}

// g10ListTools asks for the profile's tools; ok is false when the server closed the connection.
func g10ListTools(t *testing.T, conn *harness.RemoteConn, profile string) (names []string, ok bool) {
	t.Helper()
	reply, ok := conn.Send(map[string]any{"type": "ListTools", "project_id": profile})
	if !ok {
		return nil, false
	}
	if reply["type"] != "Tools" {
		t.Fatalf("ListTools answered %v, want a Tools reply", reply)
	}
	names = toolNamesOf(t, reply["tools"])
	sort.Strings(names)
	return names, true
}

func g10Code(reply map[string]any) float64 {
	c, _ := reply["code"].(float64)
	return c
}

// g10RequireDenied checks a refused presence prompt: the event line and the control_decision audit row.
func g10RequireDenied(t *testing.T, i *harness.Instance, trace, eventKey, gate, via string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: eventKey, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == gate && row["via"] == via {
			return
		}
	}
	t.Fatalf("no control_decision denied row for %s via %s", gate, via)
}

func g10RequireRefusedCLI(t *testing.T, i *harness.Instance, res harness.Result, eventKey, gate string) {
	t.Helper()
	if res.Code != 1 {
		t.Fatalf("a refused %s exited %d, want 1\nstderr: %s", gate, res.Code, res.Stderr)
	}
	g10RequireDenied(t, i, res.Trace, eventKey, gate, "cli")
}

type g10Enrolment struct {
	ClientID    string   `json:"client_id"`
	Fingerprint string   `json:"fingerprint"`
	ProjectIDs  []string `json:"project_ids"`
	Budget      struct {
		MaxCalls int `json:"max_calls"`
	} `json:"budget"`
}

func g10GetEnrolment(t *testing.T, i *harness.Instance, clientID string) (g10Enrolment, harness.Response) {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/enrolments/"+clientID, nil)
	var e g10Enrolment
	if resp.Status == 200 {
		resp.JSON(t, &e)
	}
	return e, resp
}

// g10Lodged is one request lodged on its own connection: the connection also
// carries the polls, and relay drops the request when it closes.
type g10Lodged struct {
	conn      *harness.EnrolConn
	id        *harness.RemoteIdentity
	sas       *harness.SASClient
	requestID string
	spki      string
	result    map[string]any
	opened    bool
}

// g10LodgeOn lodges a request with a comparison commitment on conn and returns it.
func g10LodgeOn(t *testing.T, conn *harness.EnrolConn, label string) *g10Lodged {
	t.Helper()
	l := &g10Lodged{conn: conn, id: harness.NewRemoteIdentity(t, label), sas: harness.NewSASClient(t)}
	reply, ok := conn.Send(harness.Lodge(l.id, label, l.sas))
	if !ok || reply["type"] != "Result" {
		t.Fatalf("lodging %s answered ok=%v %v, want a Result", label, ok, reply)
	}
	l.result, _ = reply["result"].(map[string]any)
	l.requestID, _ = l.result["request_id"].(string)
	l.spki, _ = l.result["spki_sha256"].(string)
	if l.requestID == "" || l.spki == "" {
		t.Fatalf("the Result for %s lacks request_id or spki_sha256: %v", label, l.result)
	}
	return l
}

// g10OpenAndLodge opens a window on the rig's profile, dials its address and lodges one request.
func g10OpenAndLodge(t *testing.T, r *g10Rig, label string) *g10Lodged {
	t.Helper()
	addr := r.i.EnrolOpen(r.profile)
	return g10LodgeOn(t, r.i.EnrolDial(addr), label)
}

// poll asks about the request on the connection it was lodged on, without
// opening the comparison commitment.
func (l *g10Lodged) poll(t *testing.T) map[string]any {
	t.Helper()
	return l.pollWith(t, false)
}

// pollOpen is poll with the opening of the comparison commitment, which an
// approval needs. Only the first poll carries it.
func (l *g10Lodged) pollOpen(t *testing.T) map[string]any {
	t.Helper()
	return l.pollWith(t, true)
}

func (l *g10Lodged) pollWith(t *testing.T, open bool) map[string]any {
	t.Helper()
	frame := map[string]any{"type": "EnrolmentRequestPoll", "request_id": l.requestID}
	if open && !l.opened {
		frame["sas_open"] = l.sas.Open()
		l.opened = true
	}
	reply, ok := l.conn.Send(frame)
	if !ok || reply["type"] != "Result" {
		t.Fatalf("polling %s answered ok=%v %v, want a Result", l.requestID, ok, reply)
	}
	result, _ := reply["result"].(map[string]any)
	return result
}

type g10WindowView struct {
	Open        bool   `json:"open"`
	Target      string `json:"target"`
	ProfileID   string `json:"profile_id"`
	ProfileName string `json:"profile_name"`
	Expires     string `json:"expires"`
	Listen      string `json:"listen"`
}

type g10RemoteView struct {
	Enabled            bool          `json:"enabled"`
	Listen             string        `json:"listen"`
	Effective          string        `json:"effective"`
	EnrolmentListen    string        `json:"enrolment_listen"`
	EnrolmentEffective string        `json:"enrolment_effective"`
	EnrolmentWindow    g10WindowView `json:"enrolment_window"`
}

func g10RemoteShow(t *testing.T, i *harness.Instance) g10RemoteView {
	t.Helper()
	var v g10RemoteView
	i.MustCLI("remote", "show", "--json").JSON(t, &v)
	return v
}

func TestRemoteShow(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		Credentials: g10Creds,
	})

	res := i.MustCLI("remote", "show", "--json")
	var cli g10RemoteView
	res.JSON(t, &cli)
	if !cli.Enabled || cli.Listen != "127.0.0.1:0" || cli.Effective == "" {
		t.Fatalf("remote show reports %+v, want enabled, the configured listen address and an effective one", cli)
	}
	if cli.EnrolmentWindow.Open {
		t.Fatalf("remote show reports an open enrolment window on a fresh instance: %+v", cli.EnrolmentWindow)
	}
	var raw map[string]json.RawMessage
	res.JSON(t, &raw)
	if _, has := raw["enrolment_requests"]; has {
		t.Fatalf("remote show --json still carries enrolment_requests: %s", res.Stdout)
	}
	if _, has := raw["enrolment_window"]; !has {
		t.Fatalf("remote show --json has no enrolment_window: %s", res.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.config.get", Trace: res.Trace, Fields: map[string]any{"status": "ok"}})

	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/remote", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/remote answered %d, want 200", resp.Status)
	}
	var web g10RemoteView
	resp.JSON(t, &web)
	if web != cli {
		t.Fatalf("GET /api/remote reports %+v, remote show reports %+v", web, cli)
	}
	var webRaw map[string]json.RawMessage
	resp.JSON(t, &webRaw)
	if _, has := webRaw["enrolment_requests"]; has {
		t.Fatalf("GET /api/remote still carries enrolment_requests: %s", resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.config.get", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestRemoteShowReportsWindow(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	addr := i.EnrolOpen(r.profile)

	v := g10RemoteShow(t, i)
	w := v.EnrolmentWindow
	if !w.Open || w.Target != "profile" || w.ProfileID != r.profile || w.ProfileName != "acme-profile" || w.Listen != addr || w.Expires == "" {
		t.Fatalf("remote show reports the window %+v, want it open for %s at %s", w, r.profile, addr)
	}
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/remote", nil)
	var web g10RemoteView
	resp.JSON(t, &web)
	if web != v {
		t.Fatalf("GET /api/remote reports %+v while remote show reports %+v", web, v)
	}
}

// g10WidenBody turns the remote listener on, which widens what a remote
// machine reaches.
const g10WidenBody = `{"enabled":true,"listen":"127.0.0.1:0"}`

func g10RemoteOffStart(t *testing.T, presence map[string]harness.Outcome) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":false,"listen":"127.0.0.1:0"}`)},
		Credentials: g10Creds,
		Presence:    presence,
	})
}

func TestRemoteSet(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffStart(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeApprove})
	if g10RemoteShow(t, i).Enabled {
		t.Fatalf("the instance starts with the remote listener on")
	}

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(g10WidenBody)}, "remote", "set", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("an approved remote set exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.configure", Trace: res.Trace, Fields: map[string]any{"status": "ok", "enabled": true}})
	if !g10RemoteShow(t, i).Enabled {
		t.Fatalf("remote show still reports the remote listener off after an approved set")
	}
}

func TestRemoteSetRefusesEnrolmentRequests(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0"}`)},
		Credentials: g10Creds,
		Presence:    map[string]harness.Outcome{"remote.configure": harness.OutcomeApprove},
	})
	before := g10RemoteShow(t, i)
	body := []byte(`{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true}`)

	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "remote", "set", "--file", "-", "--json")
	if res.Code != 1 {
		t.Fatalf("remote set with enrolment_requests exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	if after := g10RemoteShow(t, i); after != before {
		t.Fatalf("a refused remote set changed the settings from %+v to %+v", before, after)
	}

	resp := i.SocketHTTP(i.Credential("executer")).Do("PUT", "/api/remote", body)
	if resp.Status != 400 {
		t.Fatalf("PUT /api/remote with enrolment_requests answered %d, want 400\n%s", resp.Status, resp.Body)
	}
	if after := g10RemoteShow(t, i); after != before {
		t.Fatalf("a refused PUT /api/remote changed the settings from %+v to %+v", before, after)
	}
	if got := i.Events(harness.EventQuery{Key: "remote.configure", Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
		t.Fatalf("a refused body still wrote %d ok remote.configure events", len(got))
	}
}

func TestRemoteSetDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffStart(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
	before := g10RemoteShow(t, i)

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(g10WidenBody)}, "remote", "set", "--file", "-", "--json")
	g10RequireRefusedCLI(t, i, res, "remote.configure", "remote.configure")
	if after := g10RemoteShow(t, i); after != before {
		t.Fatalf("a refused remote set changed the settings from %+v to %+v", before, after)
	}
}

func TestRemoteSetDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffStart(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
	before := g10RemoteShow(t, i)

	resp := i.SocketHTTP(i.Credential("executer")).Do("PUT", "/api/remote", []byte(g10WidenBody))
	if resp.Status == 200 {
		t.Fatalf("a refused PUT /api/remote answered 200")
	}
	g10RequireDenied(t, i, resp.Trace, "remote.configure", "remote.configure", "http")
	if after := g10RemoteShow(t, i); after != before {
		t.Fatalf("a refused PUT /api/remote changed the settings from %+v to %+v", before, after)
	}
}

func TestEnrolmentLodgeAndThrottle(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i

	first := g10OpenAndLodge(t, r, "acme-lodge-1")
	if st, _ := first.poll(t)["status"].(string); st != "pending" {
		t.Fatalf("a poll of a lodged request answered status %q, want pending", st)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "ok", "request_id": first.requestID}})

	// A second lodge from the same source inside ten seconds is throttled.
	quick := harness.NewRemoteIdentity(t, "acme-lodge-quick")
	conn := i.EnrolDial(i.EnrolOpen(r.profile))
	reply, ok := conn.Send(harness.Lodge(quick, "acme-lodge-quick", harness.NewSASClient(t)))
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32000 {
		t.Fatalf("a second lodge inside ten seconds answered ok=%v %v, want Error -32000", ok, reply)
	}
	if res, _ := reply["result"].(map[string]any); res == nil || res["retry_after_seconds"] == nil {
		t.Fatalf("the per-source throttle carries no retry_after_seconds: %v", reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "denied", "reason": "throttled"}})

	// Fill the table to its eight rows, moving the clock past the per-source interval each time.
	for n := 2; n <= 8; n++ {
		i.ClockAdvance(11 * time.Second)
		g10OpenAndLodge(t, r, "acme-lodge-"+string(rune('0'+n)))
	}
	i.ClockAdvance(11 * time.Second)
	ninth := harness.NewRemoteIdentity(t, "acme-lodge-9")
	conn = i.EnrolDial(i.EnrolOpen(r.profile))
	reply, ok = conn.Send(harness.Lodge(ninth, "acme-lodge-9", harness.NewSASClient(t)))
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32000 {
		t.Fatalf("the ninth lodge answered ok=%v %v, want Error -32000", ok, reply)
	}
	if res := reply["result"]; res != nil {
		t.Fatalf("a lodge into a full table carries a result: %v", res)
	}
	var pending []struct {
		RequestID string `json:"request_id"`
	}
	i.MustCLI("enrol", "requests", "--json").JSON(t, &pending)
	if len(pending) != 8 {
		t.Fatalf("enrol requests lists %d requests after the table filled, want 8", len(pending))
	}

	// The listener has no approve door.
	reply, ok = first.conn.Send(map[string]any{"type": "EnrolmentApprove", "request_id": first.requestID})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32601 {
		t.Fatalf("an EnrolmentApprove frame answered ok=%v %v, want Error -32601", ok, reply)
	}
	if st, _ := first.poll(t)["status"].(string); st != "pending" {
		t.Fatalf("the request reads %q after an approve frame, want pending", st)
	}
}

func TestEnrolmentRequestsList(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")

	res := i.MustCLI("enrol", "requests", "--json")
	var pending []struct {
		RequestID   string `json:"request_id"`
		Label       string `json:"label"`
		SPKI        string `json:"spki_sha256"`
		ProfileID   string `json:"profile_id"`
		ProfileName string `json:"profile_name"`
	}
	res.JSON(t, &pending)
	if len(pending) != 1 || pending[0].RequestID != l.requestID || pending[0].Label != "acme-laptop" || pending[0].SPKI != l.spki {
		t.Fatalf("enrol requests lists %+v, want the one request %s labelled acme-laptop with key %s", pending, l.requestID, l.spki)
	}
	if pending[0].ProfileID != r.profile || pending[0].ProfileName != "acme-profile" {
		t.Fatalf("enrol requests names the profile %q (%q), want %s (acme-profile)", pending[0].ProfileID, pending[0].ProfileName, r.profile)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pending[0].SPKI) {
		t.Fatalf("spki_sha256 %q is not 64 hex characters", pending[0].SPKI)
	}
	var raw []map[string]json.RawMessage
	res.JSON(t, &raw)
	for _, k := range []string{"requested_profile", "is_legacy_request"} {
		if _, has := raw[0][k]; has {
			t.Fatalf("enrol requests --json still carries %s: %s", k, res.Stdout)
		}
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.list", Trace: res.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestEnrolmentRefuse(t *testing.T) {
	t.Parallel()
	r := g10Start(t, nil)
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")

	res := i.MustCLI("enrol", "refuse", "--id", l.requestID)
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.refuse", Trace: res.Trace, Fields: map[string]any{"status": "ok", "request_id": l.requestID}})
	if st, _ := l.poll(t)["status"].(string); st != "refused" {
		t.Fatalf("a poll of a refused request answered %q, want refused", st)
	}
	if !l.conn.Closed() {
		t.Fatalf("relay left the lodging connection open after a refused answer")
	}
	var pending []json.RawMessage
	i.MustCLI("enrol", "requests", "--json").JSON(t, &pending)
	if len(pending) != 0 {
		t.Fatalf("enrol requests lists %d requests after the refusal, want none", len(pending))
	}
	if r := i.CLI("enrol", "refuse", "--id", "req_unknown"); r.Code != 1 {
		t.Fatalf("refusing an unknown request exited %d, want 1", r.Code)
	}
}

func g10Approve(i *harness.Instance, requestID, clientID string) harness.Result {
	return i.CLI("enrol", "approve", "--id", requestID, "--client-id", clientID)
}

func TestEnrolmentApprove(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")
	if st, _ := l.pollOpen(t)["status"].(string); st != "pending" {
		t.Fatalf("a poll before the approval answered %q, want pending", st)
	}

	res := g10Approve(i, l.requestID, "acme-approved")
	if res.Code != 0 {
		t.Fatalf("an approved enrol approve exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.approve", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-approved"}})

	poll := l.poll(t)
	if poll["status"] != "approved" || poll["client_id"] != "acme-approved" {
		t.Fatalf("a poll after the approval answered %v, want approved for acme-approved", poll)
	}
	if !l.conn.Closed() {
		t.Fatalf("relay left the lodging connection open after an approved answer")
	}
	cert, _ := poll["cert_pem"].(string)
	ca, _ := poll["ca_pem"].(string)
	if cert == "" || ca == "" {
		t.Fatalf("the approved poll lacks cert_pem or ca_pem: %v", poll)
	}
	conn := i.RemoteDial(l.id, []byte(cert), []byte(ca))
	names, ok := g10ListTools(t, conn, r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("the approved certificate lists tools %v (ok=%v), want acme_note and acme_ok", names, ok)
	}
}

func TestEnrolmentApproveDenied(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeDeny})
	i := r.i
	l := g10OpenAndLodge(t, r, "acme-laptop")
	l.pollOpen(t)

	res := g10Approve(i, l.requestID, "acme-refused")
	g10RequireRefusedCLI(t, i, res, "enrolment.request.approve", "enrolment.sign")
	poll := l.poll(t)
	if poll["status"] != "pending" || poll["cert_pem"] != nil {
		t.Fatalf("a poll after a refused approval answered %v, want pending and no certificate", poll)
	}
	if _, resp := g10GetEnrolment(t, i, "acme-refused"); resp.Status != 404 {
		t.Fatalf("GET of the refused enrolment answered %d, want 404", resp.Status)
	}
}

func TestEnrolmentCreate(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.create": harness.OutcomeApprove})
	i := r.i

	res := i.CLI("enrol", "create", "--client-id", "acme-created", "--grant", r.profile)
	if res.Code != 0 {
		t.Fatalf("an approved enrol create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.create", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-created"}})

	issued := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued"}) {
		issued = issued || (row["credential"] == "enrolment" && row["subject"] == "acme-created" && row["outcome"] == "ok")
	}
	if !issued {
		t.Fatalf("no credential_issued row for the enrolment acme-created")
	}
	e, resp := g10GetEnrolment(t, i, "acme-created")
	if resp.Status != 200 || e.ClientID != "acme-created" || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("GET /api/enrolments/acme-created answered %d %+v, want the enrolment with the profile", resp.Status, e)
	}
}

func TestEnrolmentCreateDeniedCLI(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.create": harness.OutcomeDeny})
	i := r.i

	res := i.CLI("enrol", "create", "--client-id", "acme-refused", "--grant", r.profile)
	g10RequireRefusedCLI(t, i, res, "enrolment.create", "enrolment.create")
	if _, resp := g10GetEnrolment(t, i, "acme-refused"); resp.Status != 404 {
		t.Fatalf("GET of the refused enrolment answered %d, want 404", resp.Status)
	}
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued"}) {
		if row["credential"] == "enrolment" {
			t.Fatalf("a refused create left a credential_issued row: %v", row)
		}
	}
}

func TestEnrolmentCreateDeniedHTTP(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.create": harness.OutcomeDeny})
	i := r.i

	resp := i.HTTP(i.Credential("granter")).Do("POST", "/api/enrolments", map[string]any{"client_id": "acme-refused", "project_ids": []string{r.profile}})
	if resp.Status == 201 {
		t.Fatalf("a refused POST /api/enrolments answered 201")
	}
	g10RequireDenied(t, i, resp.Trace, "enrolment.create", "enrolment.create", "http")
	if _, got := g10GetEnrolment(t, i, "acme-refused"); got.Status != 404 {
		t.Fatalf("GET of the refused enrolment answered %d, want 404", got.Status)
	}
}

func TestEnrolmentSign(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	id := harness.NewRemoteIdentity(t, "acme-signed")
	out := filepath.Join(i.Dir, "signed-out")

	res := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-signed", "--csr", "-", "--grant", r.profile, "--out", out)
	if res.Code != 0 {
		t.Fatalf("an approved enrol sign exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.sign", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-signed"}})
	cert, ca := mustRead(t, filepath.Join(out, "client.crt")), mustRead(t, filepath.Join(out, "ca.crt"))

	names, ok := g10ListTools(t, i.RemoteDial(id, cert, ca), r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("the signed certificate lists tools %v (ok=%v), want acme_note and acme_ok", names, ok)
	}
}

func TestEnrolmentSignDenied(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeDeny})
	i := r.i
	id := harness.NewRemoteIdentity(t, "acme-refused")
	out := filepath.Join(i.Dir, "signed-out")

	res := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-refused", "--csr", "-", "--grant", r.profile, "--out", out)
	g10RequireRefusedCLI(t, i, res, "enrolment.sign", "enrolment.sign")
	if got := i.Events(harness.EventQuery{Key: "enrolment.sign", Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
		t.Fatalf("a refused sign wrote %d ok events", len(got))
	}
	if _, resp := g10GetEnrolment(t, i, "acme-refused"); resp.Status != 404 {
		t.Fatalf("GET of the refused enrolment answered %d, want 404", resp.Status)
	}
	if _, err := os.Stat(filepath.Join(out, "client.crt")); err == nil {
		t.Fatalf("a refused sign wrote %s", filepath.Join(out, "client.crt"))
	}
}

func TestEnrolmentUpdate(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.update": harness.OutcomeApprove})
	i := r.i
	g10Sign(t, r, "acme-update")

	res := i.CLI("enrol", "update", "--client-id", "acme-update", "--max-calls", "5")
	if res.Code != 0 {
		t.Fatalf("an approved enrol update exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.update", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-update"}})
	if e, _ := g10GetEnrolment(t, i, "acme-update"); e.Budget.MaxCalls != 5 {
		t.Fatalf("budget.max_calls is %d after the update, want 5", e.Budget.MaxCalls)
	}
}

func TestEnrolmentUpdateDenied(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.update": harness.OutcomeDeny})
	i := r.i
	g10Sign(t, r, "acme-update")
	before, _ := g10GetEnrolment(t, i, "acme-update")

	res := i.CLI("enrol", "update", "--client-id", "acme-update", "--max-calls", "5")
	g10RequireRefusedCLI(t, i, res, "enrolment.update", "enrolment.update")
	if after, _ := g10GetEnrolment(t, i, "acme-update"); after.Budget.MaxCalls != before.Budget.MaxCalls || before.Budget.MaxCalls == 5 {
		t.Fatalf("budget.max_calls went from %d to %d on a refused update", before.Budget.MaxCalls, after.Budget.MaxCalls)
	}
}

func TestEnrolmentRevokeEndsLiveConnection(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.revoke": harness.OutcomeApprove})
	i := r.i
	c := g10Sign(t, r, "acme-revoke")
	conn := r.dial(c)
	if _, ok := g10ListTools(t, conn, r.profile); !ok {
		t.Fatalf("the enrolled client got no reply before the revoke")
	}

	res := i.CLI("enrol", "revoke", "--client-id", "acme-revoke")
	if res.Code != 0 {
		t.Fatalf("an approved enrol revoke exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.revoke", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-revoke"}})
	if names, ok := g10ListTools(t, conn, r.profile); ok {
		t.Fatalf("the open connection still lists tools %v after the revoke", names)
	}
	if names, ok := g10ListTools(t, r.dial(c), r.profile); ok {
		t.Fatalf("a new connection with the revoked certificate lists tools %v", names)
	}
	revoked := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_revoked"}) {
		revoked = revoked || (row["credential"] == "enrolment" && row["subject"] == "acme-revoke")
	}
	if !revoked {
		t.Fatalf("no credential_revoked row for acme-revoke")
	}
}

func TestEnrolmentRevokeDeniedCLI(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.revoke": harness.OutcomeDeny})
	i := r.i
	c := g10Sign(t, r, "acme-revoke")
	conn := r.dial(c)

	res := i.CLI("enrol", "revoke", "--client-id", "acme-revoke")
	g10RequireRefusedCLI(t, i, res, "enrolment.revoke", "enrolment.revoke")
	if _, ok := g10ListTools(t, conn, r.profile); !ok {
		t.Fatalf("the open connection was closed by a refused revoke")
	}
	if _, ok := g10ListTools(t, r.dial(c), r.profile); !ok {
		t.Fatalf("a new connection with the certificate was refused after a refused revoke")
	}
}

func TestEnrolmentRevokeDeniedHTTP(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.revoke": harness.OutcomeDeny})
	i := r.i
	c := g10Sign(t, r, "acme-revoke")
	conn := r.dial(c)

	resp := i.HTTP(i.Credential("granter")).Do("DELETE", "/api/enrolments/acme-revoke", nil)
	if resp.Status == 204 {
		t.Fatalf("a refused DELETE /api/enrolments/acme-revoke answered 204")
	}
	g10RequireDenied(t, i, resp.Trace, "enrolment.revoke", "enrolment.revoke", "http")
	if _, ok := g10ListTools(t, conn, r.profile); !ok {
		t.Fatalf("the open connection was closed by a refused revoke")
	}
	if _, got := g10GetEnrolment(t, i, "acme-revoke"); got.Status != 200 {
		t.Fatalf("GET of the enrolment answered %d after a refused revoke, want 200", got.Status)
	}
}

func TestEnrolmentListAndGet(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	g10Sign(t, r, "acme-listed")

	if res := i.CLI("enrol", "list"); res.Code != 0 {
		t.Fatalf("enrol list exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/enrolments", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/enrolments answered %d, want 200", resp.Status)
	}
	var list []g10Enrolment
	resp.JSON(t, &list)
	if len(list) != 1 || list[0].ClientID != "acme-listed" {
		t.Fatalf("GET /api/enrolments lists %+v, want the one enrolment acme-listed", list)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	got, getResp := g10GetEnrolment(t, i, "acme-listed")
	if getResp.Status != 200 || got.ClientID != "acme-listed" || len(got.ProjectIDs) != 1 || got.ProjectIDs[0] != r.profile || got.Fingerprint == "" {
		t.Fatalf("GET /api/enrolments/acme-listed answered %d %+v, want the enrolment with its profile and fingerprint", getResp.Status, got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.get", Trace: getResp.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-listed"}})

	_, missing := g10GetEnrolment(t, i, "acme-unknown")
	if missing.Status != 404 {
		t.Fatalf("GET of an unknown enrolment answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.get", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestRemoteCallsScopedAndAudited(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	conn := r.dial(g10Sign(t, r, "acme-caller"))

	names, ok := g10ListTools(t, conn, r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("ListTools named %v (ok=%v), want exactly acme_note and acme_ok", names, ok)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Fields: map[string]any{"status": "ok", "project_id": r.profile, "count": 2}})

	reply, ok := conn.Send(map[string]any{"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{}, "project_id": r.profile})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("CallTool acme_ok answered ok=%v %v, want a Result", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "ok", "tool": "acme_ok", "project_id": r.profile}})
	phases := map[string]string{}
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool"}) {
		if row["tool"] == "acme_ok" {
			phase, _ := row["phase"].(string)
			outcome, _ := row["outcome"].(string)
			phases[phase] = outcome
		}
	}
	if phases["intent"] == "" || phases["completion"] != "ok" {
		t.Fatalf("call_tool rows for acme_ok by phase %v, want an intent row and an ok completion", phases)
	}

	reply, ok = conn.Send(map[string]any{"type": "CallTool", "name": "acme_hidden", "arguments": map[string]any{}, "project_id": r.profile})
	if !ok || reply["type"] != "Error" {
		t.Fatalf("CallTool of a tool outside the profile answered ok=%v %v, want an Error", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "denied", "tool": "acme_hidden"}})
	denied := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "denied"}) {
		denied = denied || row["tool"] == "acme_hidden"
	}
	if !denied {
		t.Fatalf("no call_tool denied row for acme_hidden")
	}

	off := harness.Start(t, harness.Options{Settings: map[string]json.RawMessage{
		"remote": json.RawMessage(remoteBlock),
		"audit":  json.RawMessage(`{"enabled": false}`),
	}})
	if addr := off.Ready.Listeners["remote"]; addr != "" {
		t.Fatalf("ready.json has listeners.remote %q with auditing off, want none", addr)
	}
}

func TestRemoteGrantNarrowOnly(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.update": harness.OutcomeApprove})
	i := r.i
	admin := g10Sign(t, r, "acme-admin")
	plain := g10Sign(t, r, "acme-plain")
	if res := i.CLI("enrol", "update", "--client-id", "acme-admin", "--cli-admin"); res.Code != 0 {
		t.Fatalf("enrol update --cli-admin exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	conn := r.dial(admin)

	reply, ok := conn.Send(map[string]any{"type": "DescribeGrant", "project_id": r.profile})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("DescribeGrant answered ok=%v %v, want a Result", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.describe", Fields: map[string]any{"status": "ok", "client_id": "acme-admin", "project_id": r.profile}})

	widen := map[string]any{"allowed_tools": map[string][]string{"acme-stdio": {"acme_ok", "acme_note", "acme_hidden"}}}
	reply, ok = conn.Send(map[string]any{"type": "NarrowGrant", "project_id": r.profile, "arguments": widen})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32602 {
		t.Fatalf("a NarrowGrant that adds a tool answered ok=%v %v, want Error -32602", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.narrow", Fields: map[string]any{"status": "error", "reason": "invalid"}})

	narrow := map[string]any{"allowed_tools": map[string][]string{"acme-stdio": {"acme_ok"}}}
	reply, ok = conn.Send(map[string]any{"type": "NarrowGrant", "project_id": r.profile, "arguments": narrow})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("a NarrowGrant that drops a tool answered ok=%v %v, want a Result", ok, reply)
	}
	result, _ := reply["result"].(map[string]any)
	if changed, _ := result["changed"].([]any); len(changed) == 0 {
		t.Fatalf("the narrowing reports no changed field: %v", result)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.narrow", Fields: map[string]any{"status": "ok", "client_id": "acme-admin"}})
	if names, _ := g10ListTools(t, conn, r.profile); strings.Join(names, ",") != "acme_ok" {
		t.Fatalf("ListTools after the narrowing names %v, want acme_ok only", names)
	}

	pconn := r.dial(plain)
	for _, frame := range []map[string]any{
		{"type": "DescribeGrant", "project_id": r.profile},
		{"type": "NarrowGrant", "project_id": r.profile, "arguments": narrow},
	} {
		reply, ok := pconn.Send(frame)
		if !ok || reply["type"] != "Error" || g10Code(reply) != -32601 {
			t.Fatalf("%v from an enrolment without cli_admin answered ok=%v %v, want Error -32601", frame["type"], ok, reply)
		}
	}
}

func TestRemoteBudgetThrottles(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	conn := r.dial(g10Sign(t, r, "acme-budget", "--max-calls", "1"))
	call := map[string]any{"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{}, "project_id": r.profile}

	reply, ok := conn.Send(call)
	if !ok || reply["type"] != "Result" {
		t.Fatalf("the first call answered ok=%v %v, want a Result", ok, reply)
	}
	reply, ok = conn.Send(call)
	if !ok || reply["type"] != "Error" {
		t.Fatalf("the call over the budget answered ok=%v %v, want an Error", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "denied", "reason": "throttled"}})
	if rows := i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "throttled"}); len(rows) == 0 {
		t.Fatalf("no call_tool throttled row after a call over the budget")
	}
}
