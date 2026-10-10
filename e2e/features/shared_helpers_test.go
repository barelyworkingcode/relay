package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

const mcpUpDeadline = 60 * time.Second

var configurer = []harness.CredentialSpec{{Name: "configurer", Classes: []string{"configure"}}}

// remoteBlock turns on the remote listener and the enrolment-request listener
// on loopback ports the kernel picks.
const remoteBlock = `{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true,"enrolment_listen":"127.0.0.1:0"}`

func acmeStdioMCP() []harness.FakeMCPSpec {
	return []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}}
}

// readOnlyCatalogue marks acme_ok read-only and closed-world and leaves
// acme_echo unmarked: a read grant admits only tools annotated read-only, and
// a profile refuses open-world tools by default.
func readOnlyCatalogue() harness.Catalogue {
	return harness.Catalogue{Tools: []json.RawMessage{
		json.RawMessage(`{"name":"acme_echo","description":"Echo the arguments","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true}}`),
		json.RawMessage(`{"name":"acme_ok","description":"Say ok","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true,"openWorldHint":false}}`),
	}}
}

func TestSSHHostProbeReachable(t *testing.T) {
	t.Parallel()
	host := harness.StartSSHHost(t)
	i := harness.Start(t, harness.Options{Credentials: configurer})
	i.TrustSSHHost(host)

	create := i.HTTP(i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{
		"name": "acme-box", "target": host.Target, "port": host.Port, "identity_file": host.IdentityFile,
	})
	if create.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201", create.Status)
	}
	var created struct {
		ID string `json:"id"`
	}
	create.JSON(t, &created)

	res := i.MustCLI("host", "probe", "--id", created.ID, "--json")
	var probed struct {
		Probe struct {
			OK       bool   `json:"ok"`
			TmuxPath string `json:"tmux_path"`
		} `json:"probe"`
	}
	res.JSON(t, &probed)
	if !probed.Probe.OK {
		t.Fatalf("probe.ok is false for a loopback sshd: %s", res.Stdout)
	}
	if probed.Probe.TmuxPath == "" {
		t.Fatalf("probe.tmux_path is empty; tmux is installed on this machine: %s", res.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.probe", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": created.ID}})
}

func TestBridgeAdminFrameNeedsSecret(t *testing.T) {
	t.Parallel()
	const secret = "acme-planted-admin-secret"
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"admin_secret": json.RawMessage(`"` + secret + `"`)},
		FakeMCPs: acmeStdioMCP(),
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	ok := i.BridgeSend(map[string]any{"type": "ReconcileExternalMcps", "token": secret})
	if ok.Type != "OK" {
		t.Fatalf("an admin frame with the planted secret answered %s (code %d), want OK", ok.Type, ok.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.reconcile", Trace: ok.Trace, Fields: map[string]any{"status": "ok"}})

	bad := i.BridgeSend(map[string]any{"type": "ReconcileExternalMcps", "token": "not-the-secret"})
	if bad.Type != "Error" || bad.Code != -32001 {
		t.Fatalf("an admin frame with a wrong token answered %s code %d, want Error -32001", bad.Type, bad.Code)
	}
	if got := i.Events(harness.EventQuery{Key: "mcp.reconcile", Trace: bad.Trace}); len(got) != 0 {
		t.Fatalf("a refused admin frame still wrote %d mcp.reconcile lines", len(got))
	}
}

func TestEnrolClientLodgesRequest(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
	})
	id := harness.NewRemoteIdentity(t, "acme-laptop")

	replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": "acme-laptop"})
	if len(replies) != 1 || replies[0]["type"] != "Result" {
		t.Fatalf("EnrolmentRequest answered %v, want one Result", replies)
	}
	result, _ := replies[0]["result"].(map[string]any)
	requestID, _ := result["request_id"].(string)
	if requestID == "" {
		t.Fatalf("the Result has no request_id: %v", replies[0])
	}

	res := i.MustCLI("enrol", "requests", "--json")
	var pending []struct {
		RequestID string `json:"request_id"`
	}
	res.JSON(t, &pending)
	found := false
	for _, p := range pending {
		found = found || p.RequestID == requestID
	}
	if !found {
		t.Fatalf("enrol requests lists %d requests and not %s", len(pending), requestID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "enrolment.request.lodge", Fields: map[string]any{"status": "ok", "request_id": requestID}})
}

func TestRemoteClientListsProfileTools(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		FakeMCPs: []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: readOnlyCatalogue()}},
		Presence: map[string]harness.Outcome{
			"project.grant":    harness.OutcomeApprove,
			"enrolment.sign":   harness.OutcomeApprove,
			"enrolment.revoke": harness.OutcomeApprove,
		},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	profileBody, err := json.Marshal(map[string]any{
		"name": "acme-profile", "kind": "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_*"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	if err != nil {
		t.Fatalf("encoding the access profile: %v", err)
	}
	var profile project
	i.CLIWith(harness.CLIOpts{Stdin: profileBody}, "project", "create", "--file", "-", "--json").JSON(t, &profile)
	if profile.ID == "" {
		t.Fatalf("project create returned no profile id")
	}

	identity := harness.NewRemoteIdentity(t, "acme-laptop")
	out := filepath.Join(i.Dir, "signed")
	if r := i.CLIWith(harness.CLIOpts{Stdin: identity.CSRPEM()}, "enrol", "sign", "--client-id", "acme-client", "--csr", "-", "--grant", profile.ID, "--out", out); r.Code != 0 {
		t.Fatalf("enrol sign exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	certPEM, caPEM := mustRead(t, filepath.Join(out, "client.crt")), mustRead(t, filepath.Join(out, "ca.crt"))

	conn := i.RemoteDial(identity, certPEM, caPEM)
	reply, ok := conn.Send(map[string]any{"type": "ListTools", "project_id": profile.ID})
	if !ok || reply["type"] != "Tools" {
		t.Fatalf("ListTools answered ok=%v %v, want a Tools reply", ok, reply)
	}
	names := toolNamesOf(t, reply["tools"])
	if !hasAll(names, "acme_ok") || hasAll(names, "acme_echo") {
		t.Fatalf("ListTools named %v, want acme_ok and not acme_echo", names)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Fields: map[string]any{"status": "ok", "project_id": profile.ID, "count": 1}})

	i.MustCLI("enrol", "revoke", "--client-id", "acme-client")
	if reply, ok := conn.Send(map[string]any{"type": "ListTools", "project_id": profile.ID}); ok {
		t.Fatalf("a revoked client still got a reply: %v", reply)
	}
}

func TestWebSocketFilesWatch(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}},
		Presence:    approveGrant,
	})
	p, _ := createProject(t, i, "acme-files")

	ws := i.WebSocket("/ws/files", i.Credential("runner"))
	ws.Send(map[string]any{"type": "watch", "project_id": p.ID})
	var frame struct {
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(ws.Next(30*time.Second), &frame); err != nil {
		t.Fatalf("decoding the first frame: %v", err)
	}
	if frame.Type != "watch_ok" || frame.ProjectID != p.ID {
		t.Fatalf("first frame is %+v, want watch_ok for %s", frame, p.ID)
	}
	ws.Close()
	i.WaitEvent(harness.EventQuery{Key: "file.ws.close", Fields: map[string]any{"status": "ok"}}, 30*time.Second)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func toolNamesOf(t *testing.T, v any) []string {
	t.Helper()
	list, _ := v.([]any)
	var names []string
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}
