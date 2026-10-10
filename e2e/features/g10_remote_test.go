package features

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"relaye2e/harness"
)

// g10Catalogue marks every tool read-only and closed-world so a read profile
// lists it.
func g10Catalogue() harness.Catalogue {
	tool := func(name string) json.RawMessage {
		return json.RawMessage(`{"name":"` + name + `","description":"Say ok","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true,"openWorldHint":false}}`)
	}
	return harness.Catalogue{Tools: []json.RawMessage{tool("acme_ok"), tool("acme_note"), tool("acme_hidden")}}
}

var g10Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "executor", Classes: []string{"execute"}},
	{Name: "grantor", Classes: []string{"grant", "read"}},
}

func g10Approve(gates ...string) map[string]harness.Outcome {
	m := map[string]harness.Outcome{"project.grant": harness.OutcomeApprove}
	for _, g := range gates {
		m[g] = harness.OutcomeApprove
	}
	return m
}

// g10Start boots an instance with the remote and enrolment listeners on, one
// stdio fake MCP up and the three credential classes the G10 doors use.
func g10Start(t *testing.T, presence map[string]harness.Outcome) *harness.Instance {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: g10Catalogue()}},
		Credentials: g10Creds,
		Presence:    presence,
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)
	return i
}

// g10Profile creates an access profile that allows acme_ok and acme_note.
func g10Profile(t *testing.T, i *harness.Instance) string {
	t.Helper()
	return g10ProfileNamed(t, i, "acme-profile")
}

// g10ProfileNamed creates the same access profile under another name.
func g10ProfileNamed(t *testing.T, i *harness.Instance, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name, "kind": "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_ok", "acme_note"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	if err != nil {
		t.Fatalf("encoding the access profile: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create returned no profile id")
	}
	return p.ID
}

type g10Client struct {
	identity *harness.RemoteIdentity
	cert, ca []byte
}

func (c g10Client) dial(i *harness.Instance) *harness.RemoteConn {
	return i.RemoteDial(c.identity, c.cert, c.ca)
}

// g10Sign signs a fresh identity through `enrol sign` and reads the issued files.
func g10Sign(t *testing.T, i *harness.Instance, clientID string, args ...string) g10Client {
	t.Helper()
	id := harness.NewRemoteIdentity(t, clientID)
	out := filepath.Join(i.Dir, "signed-"+clientID)
	full := append([]string{"enrol", "sign", "--client-id", clientID, "--csr", "-", "--out", out}, args...)
	if r := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, full...); r.Code != 0 {
		t.Fatalf("enrol sign exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	return g10Client{identity: id, cert: mustRead(t, filepath.Join(out, "client.crt")), ca: mustRead(t, filepath.Join(out, "ca.crt"))}
}

// g10ListTools sends ListTools and returns the tool names; ok is false when
// relay closed the connection or answered anything but Tools.
func g10ListTools(t *testing.T, c *harness.RemoteConn, profileID string) (names []string, ok bool) {
	t.Helper()
	reply, got := c.Send(map[string]any{"type": "ListTools", "project_id": profileID})
	if !got || reply["type"] != "Tools" {
		return nil, false
	}
	return toolNamesOf(t, reply["tools"]), true
}

func g10Code(reply map[string]any) float64 {
	code, _ := reply["code"].(float64)
	return code
}

func g10Result(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	res, _ := reply["result"].(map[string]any)
	if res == nil {
		t.Fatalf("the reply has no result object: %v", reply)
	}
	return res
}

// g10RateWindow is longer than the per-source lodge interval (10 s).
const g10RateWindow = 11 * time.Second

type g10Lodged struct {
	identity  *harness.RemoteIdentity
	requestID string
	spki      string
}

func g10LodgeFrame(id *harness.RemoteIdentity, label string) map[string]any {
	return map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": label}
}

// g10Lodge lodges one request over the enrolment listener.
func g10Lodge(t *testing.T, i *harness.Instance, label string) g10Lodged {
	t.Helper()
	id := harness.NewRemoteIdentity(t, label)
	replies := i.EnrolSend(g10LodgeFrame(id, label))
	if len(replies) != 1 || replies[0]["type"] != "Result" {
		t.Fatalf("EnrolmentRequest answered %v, want one Result", replies)
	}
	res := g10Result(t, replies[0])
	requestID, _ := res["request_id"].(string)
	spki, _ := res["spki_sha256"].(string)
	if requestID == "" {
		t.Fatalf("the lodge Result has no request_id: %v", replies[0])
	}
	return g10Lodged{identity: id, requestID: requestID, spki: spki}
}

func g10Poll(t *testing.T, i *harness.Instance, requestID string) map[string]any {
	t.Helper()
	replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequestPoll", "request_id": requestID})
	if len(replies) != 1 || replies[0]["type"] != "Result" {
		t.Fatalf("EnrolmentRequestPoll answered %v, want one Result", replies)
	}
	return g10Result(t, replies[0])
}

type g10Request struct {
	RequestID string `json:"request_id"`
	Spki      string `json:"spki_sha256"`
	Label     string `json:"label"`
}

func g10Requests(t *testing.T, i *harness.Instance) ([]g10Request, harness.Result) {
	t.Helper()
	r := i.MustCLI("enrol", "requests", "--json")
	var reqs []g10Request
	r.JSON(t, &reqs)
	return reqs, r
}

// g10RequireRefusal checks the refusal set of a gated act the owner denied:
// the event is denied for presence_refused, and a control_decision denied row
// names the gate.
func g10RequireRefusal(t *testing.T, i *harness.Instance, trace, event, gate string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: event, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == gate {
			return
		}
	}
	t.Fatalf("no control_decision denied audit row for %s", gate)
}

func g10RequireCLIRefusal(t *testing.T, i *harness.Instance, r harness.Result, event, gate string) {
	t.Helper()
	if r.Code != 1 {
		t.Fatalf("a refused %s exited %d, want 1", gate, r.Code)
	}
	if len(bytes.TrimSpace(r.Stdout)) != 0 {
		t.Fatalf("a refused %s printed on stdout: %s", gate, r.Stdout)
	}
	g10RequireRefusal(t, i, r.Trace, event, gate)
}

type g10Enrolment struct {
	ClientID   string   `json:"client_id"`
	ProjectIDs []string `json:"project_ids"`
	CLIAdmin   bool     `json:"cli_admin"`
	Budget     struct {
		MaxCalls int `json:"max_calls"`
	} `json:"budget"`
}

func g10GetEnrolment(t *testing.T, i *harness.Instance, clientID string) (g10Enrolment, harness.Response) {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/enrolments/"+clientID, nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	var e g10Enrolment
	if resp.Status == 200 {
		resp.JSON(t, &e)
	}
	return e, resp
}

type g10RemoteView struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Effective         string `json:"effective"`
	EnrolmentRequests bool   `json:"enrolment_requests"`
}

func g10Show(t *testing.T, i *harness.Instance) g10RemoteView {
	t.Helper()
	var v g10RemoteView
	i.MustCLI("remote", "show", "--json").JSON(t, &v)
	return v
}

// g10SetBody is the PUT /api/remote body that switches the enrolment listener on.
func g10SetBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"enabled": true, "listen": "127.0.0.1:0", "enrolment_requests": true, "enrolment_listen": "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("encoding the remote record: %v", err)
	}
	return b
}

// g10StartWithoutEnrolmentRequests boots with the tool-plane listener only, so
// turning the enrolment listener on is a change that widens.
func g10StartWithoutEnrolmentRequests(t *testing.T, presence map[string]harness.Outcome) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Settings:    map[string]json.RawMessage{"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0"}`)},
		Credentials: g10Creds,
		Presence:    presence,
	})
}

func TestRemoteShow(t *testing.T) {
	t.Parallel()
	i := g10Start(t, nil)

	r := i.MustCLI("remote", "show", "--json")
	var v g10RemoteView
	r.JSON(t, &v)
	if !v.Enabled || v.Listen != "127.0.0.1:0" || v.Effective == "" {
		t.Fatalf("remote show reports %+v, want enabled, the configured listen and an effective address", v)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.config.get", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})

	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/remote", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/remote answered %d, want 200", resp.Status)
	}
	var got g10RemoteView
	resp.JSON(t, &got)
	if got != v {
		t.Fatalf("GET /api/remote reports %+v, remote show reports %+v", got, v)
	}
}

func TestRemoteSet(t *testing.T) {
	t.Parallel()
	i := g10StartWithoutEnrolmentRequests(t, g10Approve("remote.configure"))
	if g10Show(t, i).EnrolmentRequests {
		t.Fatalf("the instance starts with enrolment requests on")
	}

	r := i.CLIWith(harness.CLIOpts{Stdin: g10SetBody(t)}, "remote", "set", "--file", "-", "--json")
	if r.Code != 0 {
		t.Fatalf("an approved remote set exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "remote.configure", Trace: r.Trace, Fields: map[string]any{"status": "ok", "enabled": true}})
	if !g10Show(t, i).EnrolmentRequests {
		t.Fatalf("remote show does not reflect the approved change")
	}
}

func TestRemoteSetDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g10StartWithoutEnrolmentRequests(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
	before := g10Show(t, i)

	r := i.CLIWith(harness.CLIOpts{Stdin: g10SetBody(t)}, "remote", "set", "--file", "-", "--json")
	g10RequireCLIRefusal(t, i, r, "remote.configure", "remote.configure")
	if after := g10Show(t, i); after != before {
		t.Fatalf("a refused remote set changed the settings: %+v -> %+v", before, after)
	}
}

func TestRemoteSetDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g10StartWithoutEnrolmentRequests(t, map[string]harness.Outcome{"remote.configure": harness.OutcomeDeny})
	before := g10Show(t, i)

	resp := i.SocketHTTP(i.Credential("executor")).Do("PUT", "/api/remote", json.RawMessage(g10SetBody(t)))
	if resp.Status == 200 {
		t.Fatalf("a refused PUT /api/remote answered 200")
	}
	g10RequireRefusal(t, i, resp.Trace, "remote.configure", "remote.configure")
	if after := g10Show(t, i); after != before {
		t.Fatalf("a refused PUT /api/remote changed the settings: %+v -> %+v", before, after)
	}
}

func TestEnrolmentLodgeAndThrottle(t *testing.T) {
	t.Parallel()
	i := g10Start(t, nil)

	// A source may lodge one request per rate window, so the clock moves past
	// the window between lodges and the table fills to its cap of 8.
	const accepted = 8
	var firstID string
	for n := 0; n < accepted; n++ {
		lodged := g10Lodge(t, i, "acme-n")
		if n == 0 {
			firstID = lodged.requestID
		}
		i.ClockAdvance(g10RateWindow)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "ok", "request_id": firstID}})
	if got := g10Poll(t, i, firstID)["status"]; got != "pending" {
		t.Fatalf("poll of a lodged request answered %v, want pending", got)
	}

	over := harness.NewRemoteIdentity(t, "acme-over")
	replies := i.EnrolSend(g10LodgeFrame(over, "acme-over"), map[string]any{"type": "EnrolmentApprove", "request_id": firstID})
	if len(replies) != 2 {
		t.Fatalf("the listener answered %d of 2 frames: %v", len(replies), replies)
	}
	if replies[0]["type"] != "Error" || g10Code(replies[0]) != -32000 {
		t.Fatalf("lodge %d answered %v, want Error -32000", accepted+1, replies[0])
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "denied", "reason": "throttled"}})
	if replies[1]["type"] != "Error" || g10Code(replies[1]) != -32601 {
		t.Fatalf("an EnrolmentApprove frame answered %v, want Error -32601", replies[1])
	}
}

func TestEnrolmentRequestsList(t *testing.T) {
	t.Parallel()
	i := g10Start(t, nil)
	lodged := g10Lodge(t, i, "acme-laptop")

	reqs, r := g10Requests(t, i)
	if len(reqs) != 1 || reqs[0].RequestID != lodged.requestID || reqs[0].Label != "acme-laptop" || reqs[0].Spki != lodged.spki || lodged.spki == "" {
		t.Fatalf("enrol requests lists %+v, want the lodged request %s with label acme-laptop and fingerprint %q", reqs, lodged.requestID, lodged.spki)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestEnrolmentRefuse(t *testing.T) {
	t.Parallel()
	i := g10Start(t, nil)
	lodged := g10Lodge(t, i, "acme-laptop")

	r := i.MustCLI("enrol", "refuse", "--id", lodged.requestID)
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.refuse", Trace: r.Trace, Fields: map[string]any{"status": "ok", "request_id": lodged.requestID}})
	if got := g10Poll(t, i, lodged.requestID)["status"]; got != "refused" {
		t.Fatalf("poll of a refused request answered %v, want refused", got)
	}
	if reqs, _ := g10Requests(t, i); len(reqs) != 0 {
		t.Fatalf("enrol requests still lists %d requests after the refusal", len(reqs))
	}
}

func TestEnrolmentApprove(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	lodged := g10Lodge(t, i, "acme-laptop")

	r := i.CLI("enrol", "approve", "--id", lodged.requestID, "--client-id", "acme-client", "--grant", profile)
	if r.Code != 0 {
		t.Fatalf("an approved enrol approve exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.approve", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client", "request_id": lodged.requestID}})

	poll := g10Poll(t, i, lodged.requestID)
	certPEM, _ := poll["cert_pem"].(string)
	caPEM, _ := poll["ca_pem"].(string)
	if poll["status"] != "approved" || certPEM == "" || caPEM == "" {
		t.Fatalf("poll of an approved request answered %v, want approved with cert_pem and ca_pem", poll)
	}
	conn := i.RemoteDial(lodged.identity, []byte(certPEM), []byte(caPEM))
	names, ok := g10ListTools(t, conn, profile)
	if !ok || !hasAll(names, "acme_ok", "acme_note") {
		t.Fatalf("the issued certificate lists tools %v (ok=%v), want acme_ok and acme_note", names, ok)
	}
}

func TestEnrolmentApproveDenied(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve())
	profile := g10Profile(t, i)
	lodged := g10Lodge(t, i, "acme-laptop")
	i.SetPresence(map[string]harness.Outcome{"enrolment.sign": harness.OutcomeDeny})

	r := i.CLI("enrol", "approve", "--id", lodged.requestID, "--client-id", "acme-client", "--grant", profile)
	g10RequireCLIRefusal(t, i, r, "enrolment.request.approve", "enrolment.sign")
	poll := g10Poll(t, i, lodged.requestID)
	if poll["status"] != "pending" || poll["cert_pem"] != nil {
		t.Fatalf("poll after a refused approval answered %v, want pending and no certificate", poll)
	}
}

func TestEnrolmentCreate(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.create"))
	profile := g10Profile(t, i)

	r := i.CLI("enrol", "create", "--client-id", "acme-client", "--grant", profile)
	if r.Code != 0 {
		t.Fatalf("an approved enrol create exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.create", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})
	issued := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued"}) {
		issued = issued || (row["credential"] == "enrolment" && row["subject"] == "acme-client" && row["outcome"] == "ok")
	}
	if !issued {
		t.Fatalf("no credential_issued ok audit row for the enrolment")
	}
	e, resp := g10GetEnrolment(t, i, "acme-client")
	if resp.Status != 200 || e.ClientID != "acme-client" || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != profile {
		t.Fatalf("GET /api/enrolments/acme-client answered %d %+v, want the enrolment with profile %s", resp.Status, e, profile)
	}
}

func TestEnrolmentCreateDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve())
	profile := g10Profile(t, i)
	i.SetPresence(map[string]harness.Outcome{"enrolment.create": harness.OutcomeDeny})

	r := i.CLI("enrol", "create", "--client-id", "acme-client", "--grant", profile)
	g10RequireCLIRefusal(t, i, r, "enrolment.create", "enrolment.create")
	if _, resp := g10GetEnrolment(t, i, "acme-client"); resp.Status != 404 {
		t.Fatalf("a refused create left an enrolment: GET answered %d, want 404", resp.Status)
	}
}

func TestEnrolmentCreateDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve())
	profile := g10Profile(t, i)
	i.SetPresence(map[string]harness.Outcome{"enrolment.create": harness.OutcomeDeny})

	resp := i.HTTP(i.Credential("grantor")).Do("POST", "/api/enrolments", map[string]any{"client_id": "acme-client", "project_ids": []string{profile}})
	if resp.Status == 201 {
		t.Fatalf("a refused POST /api/enrolments answered 201")
	}
	g10RequireRefusal(t, i, resp.Trace, "enrolment.create", "enrolment.create")
	if _, got := g10GetEnrolment(t, i, "acme-client"); got.Status != 404 {
		t.Fatalf("a refused create left an enrolment: GET answered %d, want 404", got.Status)
	}
}

func TestEnrolmentSign(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)

	id := harness.NewRemoteIdentity(t, "acme-laptop")
	out := filepath.Join(i.Dir, "signed")
	r := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-client", "--csr", "-", "--grant", profile, "--out", out)
	if r.Code != 0 {
		t.Fatalf("an approved enrol sign exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.sign", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})
	conn := i.RemoteDial(id, mustRead(t, filepath.Join(out, "client.crt")), mustRead(t, filepath.Join(out, "ca.crt")))
	if names, ok := g10ListTools(t, conn, profile); !ok || !hasAll(names, "acme_ok") {
		t.Fatalf("the signed certificate lists tools %v (ok=%v), want acme_ok", names, ok)
	}
}

func TestEnrolmentSignDenied(t *testing.T) {
	t.Parallel()
	i := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeDeny})
	id := harness.NewRemoteIdentity(t, "acme-laptop")
	out := filepath.Join(i.Dir, "signed")

	r := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-client", "--csr", "-", "--out", out)
	g10RequireCLIRefusal(t, i, r, "enrolment.sign", "enrolment.sign")
	if got := filepath.Join(out, "client.crt"); g10FileExists(got) {
		t.Fatalf("a refused sign wrote %s", got)
	}
}

func TestEnrolmentUpdate(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign", "enrolment.update"))
	profile := g10Profile(t, i)
	g10Sign(t, i, "acme-client", "--grant", profile)

	r := i.CLI("enrol", "update", "--client-id", "acme-client", "--max-calls", "5")
	if r.Code != 0 {
		t.Fatalf("an approved enrol update exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.update", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})
	if e, resp := g10GetEnrolment(t, i, "acme-client"); resp.Status != 200 || e.Budget.MaxCalls != 5 {
		t.Fatalf("GET /api/enrolments/acme-client answered %d with max_calls %d, want 5", resp.Status, e.Budget.MaxCalls)
	}

	other := g10ProfileNamed(t, i, "acme-profile-two")
	r = i.CLI("enrol", "update", "--client-id", "acme-client", "--grant", other)
	if r.Code != 0 {
		t.Fatalf("an approved enrol update --grant exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.update", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})
	e, resp := g10GetEnrolment(t, i, "acme-client")
	if resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != other {
		t.Fatalf("GET /api/enrolments/acme-client answered %d %+v, want the grant list replaced by %s", resp.Status, e, other)
	}
	if e.Budget.MaxCalls != 5 {
		t.Fatalf("a grant update changed max_calls to %d, want 5 kept", e.Budget.MaxCalls)
	}
}

func TestEnrolmentUpdateDenied(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	g10Sign(t, i, "acme-client", "--grant", profile)
	before, _ := g10GetEnrolment(t, i, "acme-client")
	i.SetPresence(map[string]harness.Outcome{"enrolment.update": harness.OutcomeDeny})

	r := i.CLI("enrol", "update", "--client-id", "acme-client", "--max-calls", "5")
	g10RequireCLIRefusal(t, i, r, "enrolment.update", "enrolment.update")
	if after, _ := g10GetEnrolment(t, i, "acme-client"); after.Budget.MaxCalls != before.Budget.MaxCalls || before.Budget.MaxCalls == 5 {
		t.Fatalf("a refused update changed max_calls from %d to %d", before.Budget.MaxCalls, after.Budget.MaxCalls)
	}
}

func TestEnrolmentRevokeEndsLiveConnection(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign", "enrolment.revoke"))
	profile := g10Profile(t, i)
	client := g10Sign(t, i, "acme-client", "--grant", profile)
	conn := client.dial(i)
	if _, ok := g10ListTools(t, conn, profile); !ok {
		t.Fatalf("a fresh enrolment cannot list tools")
	}

	r := i.CLI("enrol", "revoke", "--client-id", "acme-client")
	if r.Code != 0 {
		t.Fatalf("an approved enrol revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})
	if _, ok := g10ListTools(t, conn, profile); ok {
		t.Fatalf("the open connection of a revoked client still lists tools")
	}
	if _, ok := g10ListTools(t, client.dial(i), profile); ok {
		t.Fatalf("a new connection of a revoked client lists tools")
	}
}

func TestEnrolmentRevokeDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	conn := g10Sign(t, i, "acme-client", "--grant", profile).dial(i)
	i.SetPresence(map[string]harness.Outcome{"enrolment.revoke": harness.OutcomeDeny})

	r := i.CLI("enrol", "revoke", "--client-id", "acme-client")
	g10RequireCLIRefusal(t, i, r, "enrolment.revoke", "enrolment.revoke")
	if names, ok := g10ListTools(t, conn, profile); !ok || !hasAll(names, "acme_ok") {
		t.Fatalf("after a refused revoke the connection lists %v (ok=%v), want acme_ok", names, ok)
	}
}

func TestEnrolmentRevokeDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	conn := g10Sign(t, i, "acme-client", "--grant", profile).dial(i)
	i.SetPresence(map[string]harness.Outcome{"enrolment.revoke": harness.OutcomeDeny})

	resp := i.HTTP(i.Credential("grantor")).Do("DELETE", "/api/enrolments/acme-client", nil)
	if resp.Status == 204 {
		t.Fatalf("a refused DELETE /api/enrolments/acme-client answered 204")
	}
	g10RequireRefusal(t, i, resp.Trace, "enrolment.revoke", "enrolment.revoke")
	if names, ok := g10ListTools(t, conn, profile); !ok || !hasAll(names, "acme_ok") {
		t.Fatalf("after a refused revoke the connection lists %v (ok=%v), want acme_ok", names, ok)
	}
}

func TestEnrolmentListAndGet(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	g10Sign(t, i, "acme-client", "--grant", profile)

	if r := i.CLI("enrol", "list"); r.Code != 0 {
		t.Fatalf("enrol list exited %d", r.Code)
	}
	reader := i.HTTP(i.Credential("reader"))
	list := reader.Do("GET", "/api/enrolments", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	var all []g10Enrolment
	list.JSON(t, &all)
	if list.Status != 200 || len(all) != 1 || all[0].ClientID != "acme-client" {
		t.Fatalf("GET /api/enrolments answered %d %+v, want the one enrolment", list.Status, all)
	}
	if len(all[0].ProjectIDs) != 1 || all[0].ProjectIDs[0] != profile {
		t.Fatalf("the list shows profiles %v for acme-client, want [%s]", all[0].ProjectIDs, profile)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	e, got := g10GetEnrolment(t, i, "acme-client")
	if got.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != profile {
		t.Fatalf("GET /api/enrolments/acme-client answered %d %+v, want profile %s", got.Status, e, profile)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.get", Trace: got.Trace, Fields: map[string]any{"status": "ok", "client_id": "acme-client"}})

	_, missing := g10GetEnrolment(t, i, "acme-unknown")
	if missing.Status != 404 {
		t.Fatalf("GET of an unknown enrolment answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.get", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

var g10Fingerprint = regexp.MustCompile(`^sha256:[0-9a-f]{64}\n?$`)

func TestCAFingerprint(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g10Approve("enrolment.sign")})

	if r := i.CLI("enrol", "ca-fingerprint"); r.Code != 1 {
		t.Fatalf("ca-fingerprint before any CA exited %d, want 1", r.Code)
	}
	client := g10Sign(t, i, "acme-client")

	r := i.CLI("enrol", "ca-fingerprint")
	if r.Code != 0 || !g10Fingerprint.Match(r.Stdout) {
		t.Fatalf("ca-fingerprint exited %d with stdout %q, want 0 and sha256: plus 64 hex characters", r.Code, r.Stdout)
	}
	block, _ := pem.Decode(client.ca)
	if block == nil {
		t.Fatalf("ca.crt holds no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	if want := "sha256:" + hex.EncodeToString(sum[:]); string(bytes.TrimSpace(r.Stdout)) != want {
		t.Fatalf("ca-fingerprint printed %q, want the fingerprint of ca.crt %q", bytes.TrimSpace(r.Stdout), want)
	}
}

func TestRemoteCallsScopedAndAudited(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	conn := g10Sign(t, i, "acme-client", "--grant", profile).dial(i)

	names, ok := g10ListTools(t, conn, profile)
	if !ok || len(names) != 2 || !hasAll(names, "acme_ok", "acme_note") {
		t.Fatalf("ListTools named %v (ok=%v), want exactly acme_ok and acme_note", names, ok)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Fields: map[string]any{"status": "ok", "project_id": profile, "count": 2}})

	reply, ok := conn.Send(map[string]any{"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{}, "project_id": profile})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("CallTool acme_ok answered ok=%v %v, want a Result", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "ok", "tool": "acme_ok"}})
	phases := map[string]string{}
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool"}) {
		if row["tool"] == "acme_ok" {
			phase, _ := row["phase"].(string)
			outcome, _ := row["outcome"].(string)
			phases[phase] = outcome
		}
	}
	if phases["intent"] != "pending" || phases["completion"] != "ok" {
		t.Fatalf("call_tool rows for acme_ok by phase: %v, want intent pending and completion ok", phases)
	}

	reply, ok = conn.Send(map[string]any{"type": "CallTool", "name": "acme_hidden", "arguments": map[string]any{}, "project_id": profile})
	if !ok || reply["type"] != "Error" {
		t.Fatalf("CallTool of a tool outside the profile answered ok=%v %v, want an Error", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "denied", "tool": "acme_hidden"}})
	denied := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "denied"}) {
		denied = denied || row["tool"] == "acme_hidden"
	}
	if !denied {
		t.Fatalf("no call_tool denied audit row for acme_hidden")
	}

	off := harness.Start(t, harness.Options{Settings: map[string]json.RawMessage{
		"remote": json.RawMessage(remoteBlock),
		"audit":  json.RawMessage(`{"enabled":false}`),
	}})
	if addr := off.Ready.Listeners["remote"]; addr != "" {
		t.Fatalf("with auditing off ready.json has listeners.remote %q, want none", addr)
	}
}

func TestRemoteGrantNarrowOnly(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign", "enrolment.update"))
	profile := g10Profile(t, i)
	admin := g10Sign(t, i, "acme-admin", "--grant", profile).dial(i)
	plain := g10Sign(t, i, "acme-plain", "--grant", profile).dial(i)
	if r := i.CLI("enrol", "update", "--client-id", "acme-admin", "--cli-admin"); r.Code != 0 {
		t.Fatalf("enrol update --cli-admin exited %d\nstderr: %s", r.Code, r.Stderr)
	}

	reply, ok := admin.Send(map[string]any{"type": "DescribeGrant", "project_id": profile})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("DescribeGrant answered ok=%v %v, want a Result", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.describe", Fields: map[string]any{"status": "ok", "client_id": "acme-admin"}})

	widen := map[string]any{"allowed_tools": map[string][]string{"acme-stdio": {"acme_ok", "acme_note", "acme_hidden"}}}
	reply, ok = admin.Send(map[string]any{"type": "NarrowGrant", "project_id": profile, "arguments": widen})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32602 {
		t.Fatalf("a NarrowGrant that adds a tool answered ok=%v %v, want Error -32602", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.narrow", Fields: map[string]any{"status": "error", "reason": "invalid"}})

	narrow := map[string]any{"allowed_tools": map[string][]string{"acme-stdio": {"acme_ok"}}}
	reply, ok = admin.Send(map[string]any{"type": "NarrowGrant", "project_id": profile, "arguments": narrow})
	if !ok || reply["type"] != "Result" {
		t.Fatalf("a NarrowGrant that drops a tool answered ok=%v %v, want a Result", ok, reply)
	}
	if changed, _ := g10Result(t, reply)["changed"].([]any); len(changed) == 0 {
		t.Fatalf("the narrowing reports no changed field: %v", reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.narrow", Fields: map[string]any{"status": "ok", "client_id": "acme-admin"}})
	if names, ok := g10ListTools(t, admin, profile); !ok || len(names) != 1 || names[0] != "acme_ok" {
		t.Fatalf("after narrowing ListTools named %v (ok=%v), want only acme_ok", names, ok)
	}

	for _, frame := range []string{"DescribeGrant", "NarrowGrant"} {
		reply, ok = plain.Send(map[string]any{"type": frame, "project_id": profile})
		if !ok || reply["type"] != "Error" || g10Code(reply) != -32601 {
			t.Fatalf("%s without cli_admin answered ok=%v %v, want Error -32601", frame, ok, reply)
		}
	}
}

func TestRemoteBudgetThrottles(t *testing.T) {
	t.Parallel()
	i := g10Start(t, g10Approve("enrolment.sign"))
	profile := g10Profile(t, i)
	conn := g10Sign(t, i, "acme-client", "--grant", profile, "--max-calls", "1").dial(i)
	call := map[string]any{"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{}, "project_id": profile}

	if reply, ok := conn.Send(call); !ok || reply["type"] != "Result" {
		t.Fatalf("the first CallTool answered ok=%v %v, want a Result", ok, reply)
	}
	reply, ok := conn.Send(call)
	if !ok || reply["type"] != "Error" {
		t.Fatalf("the CallTool over budget answered ok=%v %v, want an Error", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Fields: map[string]any{"status": "denied", "reason": "throttled"}})
	throttled := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "throttled"}) {
		throttled = throttled || row["tool"] == "acme_ok"
	}
	if !throttled {
		t.Fatalf("no call_tool throttled audit row for the call over budget")
	}
}

func g10FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
