package features

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

// TestNarrowGrantWideningIsInvalidParams drives relay's remote listener with an
// enrolled mTLS client: a NarrowGrant that adds a tool the profile does not
// hold is the client's own mistake, so the reply is invalid params.
func TestNarrowGrantWideningIsInvalidParams(t *testing.T) {
	t.Parallel()
	tool := func(name string) json.RawMessage {
		return json.RawMessage(`{"name":"` + name + `","description":"Say ok","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true,"openWorldHint":false}}`)
	}
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
		FakeMCPs: []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio",
			Catalogue: harness.Catalogue{Tools: []json.RawMessage{tool("acme_ok"), tool("acme_note"), tool("acme_hidden")}}}},
		Presence: map[string]harness.Outcome{
			"project.grant":    harness.OutcomeApprove,
			"enrolment.sign":   harness.OutcomeApprove,
			"enrolment.update": harness.OutcomeApprove,
		},
	})
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
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create returned no profile id")
	}

	id := harness.NewRemoteIdentity(t, "acme-admin")
	out := filepath.Join(i.Dir, "signed-acme-admin")
	if r := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-admin", "--csr", "-", "--out", out, "--grant", p.ID); r.Code != 0 {
		t.Fatalf("enrol sign exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	if r := i.CLI("enrol", "update", "--client-id", "acme-admin", "--cli-admin"); r.Code != 0 {
		t.Fatalf("enrol update --cli-admin exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	conn := i.RemoteDial(id, mustRead(t, filepath.Join(out, "client.crt")), mustRead(t, filepath.Join(out, "ca.crt")))

	widen := map[string]any{"allowed_tools": map[string][]string{"acme-stdio": {"acme_ok", "acme_note", "acme_hidden"}}}
	reply, ok := conn.Send(map[string]any{"type": "NarrowGrant", "project_id": p.ID, "arguments": widen})
	code, _ := reply["code"].(float64)
	if !ok || reply["type"] != "Error" || code != -32602 {
		t.Fatalf("a NarrowGrant that adds a tool answered ok=%v %v, want Error -32602", ok, reply)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.narrow", Fields: map[string]any{"status": "error", "reason": "invalid"}})
}
