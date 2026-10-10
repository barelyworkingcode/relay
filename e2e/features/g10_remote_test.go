package features

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

// g10Rig is an instance with the remote listeners on, one fake MCP and one
// access profile that allows acme_ok and acme_note but not acme_hidden.
type g10Rig struct {
	i       *harness.Instance
	profile string
}

func g10Start(t *testing.T, presence map[string]harness.Outcome, mut ...func(*harness.Options)) *g10Rig {
	t.Helper()
	p := map[string]harness.Outcome{"project.grant": harness.OutcomeApprove}
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

// g10Lodge lodges a request on the enrolment listener, with no comparison code
// (the carried-pin form), and returns its id and key hash.
func g10Lodge(t *testing.T, i *harness.Instance, label string) (id *harness.RemoteIdentity, requestID, spki string) {
	t.Helper()
	id = harness.NewRemoteIdentity(t, label)
	replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": label})
	if len(replies) != 1 || replies[0]["type"] != "Result" {
		t.Fatalf("lodging %s answered %v, want one Result", label, replies)
	}
	result, _ := replies[0]["result"].(map[string]any)
	requestID, _ = result["request_id"].(string)
	spki, _ = result["spki_sha256"].(string)
	if requestID == "" || spki == "" {
		t.Fatalf("the Result for %s lacks request_id or spki_sha256: %v", label, result)
	}
	return id, requestID, spki
}

func g10Poll(t *testing.T, i *harness.Instance, requestID string) map[string]any {
	t.Helper()
	replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequestPoll", "request_id": requestID})
	if len(replies) != 1 || replies[0]["type"] != "Result" {
		t.Fatalf("polling %s answered %v, want one Result", requestID, replies)
	}
	result, _ := replies[0]["result"].(map[string]any)
	return result
}

type g10RemoteView struct {
	Enabled            bool   `json:"enabled"`
	Listen             string `json:"listen"`
	Effective          string `json:"effective"`
	EnrolmentRequests  bool   `json:"enrolment_requests"`
	EnrolmentEffective string `json:"enrolment_effective"`
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
	requireEvent(t, i, harness.EventQuery{Key: "remote.config.get", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
}

// g10WidenBody turns the enrolment-request listener on, which widens what a
// remote machine reaches.
const g10WidenBody = `{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true,"enrolment_listen":"127.0.0.1:0"}`

func g10RemoteOffEnrolment(t *testing.T, presence map[string]harness.Outcome) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0"}`)},
		Credentials: g10Creds,
		Presence:    presence,
	})
}

func TestRemoteSet(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffEnrolment(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeApprove})
	if g10RemoteShow(t, i).EnrolmentRequests {
		t.Fatalf("the instance starts with the enrolment listener on")
	}

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(g10WidenBody)}, "remote", "set", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("an approved remote set exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.configure", Trace: res.Trace, Fields: map[string]any{"status": "ok", "enabled": true}})
	if !g10RemoteShow(t, i).EnrolmentRequests {
		t.Fatalf("remote show still reports the enrolment listener off after an approved set")
	}
}

func TestRemoteSetDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffEnrolment(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
	before := g10RemoteShow(t, i)

	res := i.CLIWith(harness.CLIOpts{Stdin: []byte(g10WidenBody)}, "remote", "set", "--file", "-", "--json")
	g10RequireRefusedCLI(t, i, res, "remote.configure", "remote.configure")
	if after := g10RemoteShow(t, i); after != before {
		t.Fatalf("a refused remote set changed the settings from %+v to %+v", before, after)
	}
}

func TestRemoteSetDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g10RemoteOffEnrolment(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
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
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
	})

	_, first, _ := g10Lodge(t, i, "acme-lodge-1")
	if st, _ := g10Poll(t, i, first)["status"].(string); st != "pending" {
		t.Fatalf("a poll of a lodged request answered status %q, want pending", st)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "ok", "request_id": first}})

	// A second lodge from the same source inside ten seconds is throttled.
	quick := harness.NewRemoteIdentity(t, "acme-lodge-quick")
	replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(quick.CSRPEM()), "label": "acme-lodge-quick"})
	if len(replies) != 1 || replies[0]["type"] != "Error" || g10Code(replies[0]) != -32000 {
		t.Fatalf("a second lodge inside ten seconds answered %v, want Error -32000", replies)
	}
	if res, _ := replies[0]["result"].(map[string]any); res == nil || res["retry_after_seconds"] == nil {
		t.Fatalf("the per-source throttle carries no retry_after_seconds: %v", replies[0])
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "denied", "reason": "throttled"}})

	// Fill the table to its eight rows, moving the clock past the per-source interval each time.
	for n := 2; n <= 8; n++ {
		i.ClockAdvance(11 * time.Second)
		g10Lodge(t, i, "acme-lodge-"+string(rune('0'+n)))
	}
	i.ClockAdvance(11 * time.Second)
	ninth := harness.NewRemoteIdentity(t, "acme-lodge-9")
	replies = i.EnrolSend(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(ninth.CSRPEM()), "label": "acme-lodge-9"})
	if len(replies) != 1 || replies[0]["type"] != "Error" || g10Code(replies[0]) != -32000 {
		t.Fatalf("the ninth lodge answered %v, want Error -32000", replies)
	}
	if res := replies[0]["result"]; res != nil {
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
	replies = i.EnrolSend(map[string]any{"type": "EnrolmentApprove", "request_id": first})
	if len(replies) != 1 || replies[0]["type"] != "Error" || g10Code(replies[0]) != -32601 {
		t.Fatalf("an EnrolmentApprove frame answered %v, want Error -32601", replies)
	}
	if st, _ := g10Poll(t, i, first)["status"].(string); st != "pending" {
		t.Fatalf("the request reads %q after an approve frame, want pending", st)
	}
}

func TestEnrolmentRequestsList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
	})
	_, requestID, spki := g10Lodge(t, i, "acme-laptop")

	res := i.MustCLI("enrol", "requests", "--json")
	var pending []struct {
		RequestID string `json:"request_id"`
		Label     string `json:"label"`
		SPKI      string `json:"spki_sha256"`
	}
	res.JSON(t, &pending)
	if len(pending) != 1 || pending[0].RequestID != requestID || pending[0].Label != "acme-laptop" || pending[0].SPKI != spki {
		t.Fatalf("enrol requests lists %+v, want the one request %s labelled acme-laptop with key %s", pending, requestID, spki)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pending[0].SPKI) {
		t.Fatalf("spki_sha256 %q is not 64 hex characters", pending[0].SPKI)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.list", Trace: res.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestEnrolmentRefuse(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
	})
	_, requestID, _ := g10Lodge(t, i, "acme-laptop")

	res := i.MustCLI("enrol", "refuse", "--id", requestID)
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.refuse", Trace: res.Trace, Fields: map[string]any{"status": "ok", "request_id": requestID}})
	if st, _ := g10Poll(t, i, requestID)["status"].(string); st != "refused" {
		t.Fatalf("a poll of a refused request answered %q, want refused", st)
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

func TestEnrolmentApprove(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	id, requestID, _ := g10Lodge(t, i, "acme-laptop")

	res := i.MustCLI("enrol", "approve", "--id", requestID, "--client-id", "acme-approved", "--grant", r.profile)
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.approve", Trace: res.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-approved"}})

	poll := g10Poll(t, i, requestID)
	if poll["status"] != "approved" || poll["client_id"] != "acme-approved" {
		t.Fatalf("a poll after the approval answered %v, want approved for acme-approved", poll)
	}
	cert, _ := poll["cert_pem"].(string)
	ca, _ := poll["ca_pem"].(string)
	if cert == "" || ca == "" {
		t.Fatalf("the approved poll lacks cert_pem or ca_pem: %v", poll)
	}
	conn := i.RemoteDial(id, []byte(cert), []byte(ca))
	names, ok := g10ListTools(t, conn, r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("the approved certificate lists tools %v (ok=%v), want acme_note and acme_ok", names, ok)
	}
}

func TestEnrolmentApproveDenied(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeDeny})
	i := r.i
	_, requestID, _ := g10Lodge(t, i, "acme-laptop")

	res := i.CLI("enrol", "approve", "--id", requestID, "--client-id", "acme-refused", "--grant", r.profile)
	g10RequireRefusedCLI(t, i, res, "enrolment.request.approve", "enrolment.sign")
	poll := g10Poll(t, i, requestID)
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

func TestCAFingerprint(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove}})

	none := i.CLI("enrol", "ca-fingerprint")
	if none.Code != 1 || len(none.Stdout) != 0 {
		t.Fatalf("ca-fingerprint before any CA exited %d with %d stdout bytes, want exit 1 and none", none.Code, len(none.Stdout))
	}

	id := harness.NewRemoteIdentity(t, "acme-ca")
	out := filepath.Join(i.Dir, "signed-out")
	if res := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-ca", "--csr", "-", "--out", out); res.Code != 0 {
		t.Fatalf("enrol sign exited %d\nstderr: %s", res.Code, res.Stderr)
	}

	res := i.MustCLI("enrol", "ca-fingerprint")
	line := strings.TrimSuffix(string(res.Stdout), "\n")
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(line) {
		t.Fatalf("ca-fingerprint printed %q, want sha256: and 64 hex characters on one line", res.Stdout)
	}
	block, _ := pem.Decode(mustRead(t, filepath.Join(out, "ca.crt")))
	if block == nil {
		t.Fatalf("ca.crt holds no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	if want := "sha256:" + hex.EncodeToString(sum[:]); line != want {
		t.Fatalf("ca-fingerprint printed %s, the hash of ca.crt is %s", line, want)
	}
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
