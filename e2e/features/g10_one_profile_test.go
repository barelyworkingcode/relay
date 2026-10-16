package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

// g10SecondProfile creates another remote access profile on the rig's MCP.
func g10SecondProfile(t *testing.T, r *g10Rig, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name, "kind": "remote",
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_ok"}},
		"access":          map[string]string{"acme-stdio": "read"},
	})
	if err != nil {
		t.Fatalf("encoding the access profile: %v", err)
	}
	var prof project
	r.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &prof)
	if prof.ID == "" {
		t.Fatalf("project create returned no profile id for %s", name)
	}
	return prof.ID
}

func g10RequireNoEnrolment(t *testing.T, i *harness.Instance, clientID string) {
	t.Helper()
	if _, resp := g10GetEnrolment(t, i, clientID); resp.Status != 404 {
		t.Fatalf("GET of enrolment %s answered %d, want 404 (none issued)", clientID, resp.Status)
	}
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_issued"}) {
		if row["credential"] == "enrolment" && row["subject"] == clientID {
			t.Fatalf("a credential_issued row exists for the refused enrolment %s: %v", clientID, row)
		}
	}
}

func TestEnrolmentOneProfileCreate(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.create": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "globex-profile")

	res := i.CLI("enrol", "create", "--client-id", "acme-two", "--grant", r.profile, "--grant", other)
	if res.Code != 1 {
		t.Fatalf("enrol create with two --grant exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	g10RequireNoEnrolment(t, i, "acme-two")
	if got := i.Events(harness.EventQuery{Key: "enrolment.create", Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
		t.Fatalf("a refused create wrote %d ok events", len(got))
	}
}

func TestEnrolmentOneProfileSign(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "globex-profile")

	id := harness.NewRemoteIdentity(t, "acme-two")
	out := filepath.Join(i.Dir, "signed-two")
	res := i.CLIWith(harness.CLIOpts{Stdin: id.CSRPEM()}, "enrol", "sign", "--client-id", "acme-two", "--csr", "-",
		"--grant", r.profile, "--grant", other, "--out", out)
	if res.Code != 1 {
		t.Fatalf("enrol sign with two --grant exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "client.crt")); err == nil {
		t.Fatalf("a sign with two profiles wrote %s", filepath.Join(out, "client.crt"))
	}
	g10RequireNoEnrolment(t, i, "acme-two")

	// One --grant still issues a certificate for a carried CSR.
	c := g10Sign(t, r, "acme-one")
	names, ok := g10ListTools(t, r.dial(c), r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("the one-profile certificate lists tools %v (ok=%v), want acme_note and acme_ok", names, ok)
	}
	if e, resp := g10GetEnrolment(t, i, "acme-one"); resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("GET of acme-one answered %d %+v, want the one profile", resp.Status, e)
	}
}

func TestEnrolmentOneProfileUpdate(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.update": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "globex-profile")
	g10Sign(t, r, "acme-update")

	res := i.CLI("enrol", "update", "--client-id", "acme-update", "--grant", r.profile, "--grant", other, "--max-calls", "7")
	if res.Code != 1 {
		t.Fatalf("enrol update naming two profiles exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	e, resp := g10GetEnrolment(t, i, "acme-update")
	if resp.Status != 200 || len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("enrolment after the refused update is %d %+v, want the original one profile", resp.Status, e)
	}
	if e.Budget.MaxCalls == 7 {
		t.Fatalf("budget.max_calls changed to 7 on a refused update")
	}
	if got := i.Events(harness.EventQuery{Key: "enrolment.update", Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
		t.Fatalf("a refused update wrote %d ok events", len(got))
	}
}

func TestEnrolmentOneProfileHTTP(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.create": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "globex-profile")

	resp := i.HTTP(i.Credential("granter")).Do("POST", "/api/enrolments", map[string]any{
		"client_id": "acme-two", "project_ids": []string{r.profile, other},
	})
	if resp.Status != 400 {
		t.Fatalf("POST /api/enrolments with two project_ids answered %d, want 400\n%s", resp.Status, resp.Body)
	}
	g10RequireNoEnrolment(t, i, "acme-two")
}

// g10WriteProjectIDs rewrites one enrolment's project_ids in settings.json, as
// a record from before one-profile enrolments reads.
func g10WriteProjectIDs(t *testing.T, i *harness.Instance, clientID string, ids []string) {
	t.Helper()
	path := filepath.Join(i.ConfigDir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding settings.json: %v", err)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(doc["enrolments"], &list); err != nil {
		t.Fatalf("decoding settings.json enrolments: %v", err)
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("encoding project_ids: %v", err)
	}
	found := false
	for _, e := range list {
		if string(e["client_id"]) == `"`+clientID+`"` {
			e["project_ids"] = idsJSON
			found = true
		}
	}
	if !found {
		t.Fatalf("settings.json holds no enrolment %s", clientID)
	}
	if doc["enrolments"], err = json.Marshal(list); err != nil {
		t.Fatalf("encoding enrolments: %v", err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding settings.json: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}
}

// g10ListStatus returns the STATUS cell of an enrolment's row in `relay enrol
// list`. The verb has no JSON form, so the test reads the column.
func g10ListStatus(t *testing.T, i *harness.Instance, clientID string) string {
	t.Helper()
	res := i.MustCLI("enrol", "list")
	lines := strings.Split(string(res.Stdout), "\n")
	cols := strings.Fields(strings.Replace(lines[0], "CLIENT ID", "CLIENT_ID", 1))
	statusAt := -1
	for n, c := range cols {
		if c == "STATUS" {
			statusAt = n
		}
	}
	if statusAt < 0 {
		t.Fatalf("enrol list has no STATUS column: %q", lines[0])
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > statusAt && f[0] == clientID {
			// "refused: names N profiles" spans several fields; join up to the next column.
			if f[statusAt] == "-" {
				return "-"
			}
			return strings.Join(f[statusAt:statusAt+4], " ")
		}
	}
	t.Fatalf("enrol list has no row for %s:\n%s", clientID, res.Stdout)
	return ""
}

func TestEnrolmentNamingTwoProfilesRefused(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.update": harness.OutcomeApprove})
	i := r.i
	other := g10SecondProfile(t, r, "globex-profile")
	c := g10Sign(t, r, "acme-legacy")
	if got := g10ListStatus(t, i, "acme-legacy"); got != "-" {
		t.Fatalf("enrol list STATUS of a one-profile enrolment is %q, want -", got)
	}

	// The stand-in for an upgrade: a record that names two profiles.
	g10WriteProjectIDs(t, i, "acme-legacy", []string{r.profile, other})

	conn := r.dial(c)
	reply, ok := conn.Send(map[string]any{"type": "CallTool", "name": "acme_ok", "arguments": map[string]any{}, "project_id": r.profile})
	if !ok || reply["type"] != "Error" || g10Code(reply) != -32001 {
		t.Fatalf("a call from a two-profile enrolment answered ok=%v %v, want Error -32001", ok, reply)
	}
	// The criterion states the refusal names the fix command.
	if msg, _ := reply["message"].(string); !strings.Contains(msg, "relay enrol update") {
		t.Fatalf("the refusal %q does not name relay enrol update", msg)
	}
	if rows := i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "denied"}); len(rows) == 0 {
		t.Errorf("no call_tool denied row after the refused call")
	}
	if reply, ok := conn.Send(map[string]any{"type": "ListTools", "project_id": other}); ok && reply["type"] != "Error" {
		t.Fatalf("ListTools from a two-profile enrolment answered %v, want an Error", reply)
	}

	if got := g10ListStatus(t, i, "acme-legacy"); got != "refused: names 2 profiles" {
		t.Fatalf("enrol list STATUS is %q, want %q", got, "refused: names 2 profiles")
	}

	res := i.CLI("enrol", "update", "--client-id", "acme-legacy", "--grant", r.profile)
	if res.Code != 0 {
		t.Fatalf("enrol update --grant to one profile exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	if e, _ := g10GetEnrolment(t, i, "acme-legacy"); len(e.ProjectIDs) != 1 || e.ProjectIDs[0] != r.profile {
		t.Fatalf("enrolment names %v after narrowing, want only %s", e.ProjectIDs, r.profile)
	}
	names, ok := g10ListTools(t, r.dial(c), r.profile)
	if !ok || strings.Join(names, ",") != "acme_note,acme_ok" {
		t.Fatalf("after narrowing, the client lists tools %v (ok=%v), want acme_note and acme_ok", names, ok)
	}
	if got := g10ListStatus(t, i, "acme-legacy"); got != "-" {
		t.Fatalf("enrol list STATUS after narrowing is %q, want -", got)
	}
}

type g10RevokedRow struct {
	Credential string   `json:"credential"`
	Subject    string   `json:"subject"`
	Grants     []string `json:"grants"`
	Via        string   `json:"via"`
	Cause      string   `json:"cause"`
	Outcome    string   `json:"outcome"`
}

func g10RevokedRows(t *testing.T, i *harness.Instance) []g10RevokedRow {
	t.Helper()
	var out []g10RevokedRow
	for _, row := range i.Audit(harness.AuditQuery{Event: "credential_revoked"}) {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("encoding an audit row: %v", err)
		}
		var rr g10RevokedRow
		if err := json.Unmarshal(b, &rr); err != nil {
			t.Fatalf("decoding an audit row: %v", err)
		}
		out = append(out, rr)
	}
	return out
}

func TestProfileRemoveRevokesEnrolments(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove}, func(o *harness.Options) {
		o.Credentials = append(append([]harness.CredentialSpec{}, g10Creds...), harness.CredentialSpec{Name: "configurer", Classes: []string{"configure"}})
	})
	i := r.i
	profB := g10SecondProfile(t, r, "globex-profile")
	profC := g10SecondProfile(t, r, "initech-profile")
	rB := &g10Rig{i: i, profile: profB}
	rC := &g10Rig{i: i, profile: profC}

	a1 := g10Sign(t, r, "acme-a1")
	g10Sign(t, r, "acme-a2")
	b1 := g10Sign(t, rB, "acme-b1")
	c1 := g10Sign(t, rC, "acme-c1")
	connA1, connB1, connC1 := r.dial(a1), rB.dial(b1), rC.dial(c1)
	for n, p := range map[*harness.RemoteConn]string{connA1: r.profile, connB1: profB, connC1: profC} {
		if _, ok := g10ListTools(t, n, p); !ok {
			t.Fatalf("an enrolled client got no reply before the remove")
		}
	}

	// Through the CLI.
	res := i.CLI("project", "remove", "--id", r.profile)
	if res.Code != 0 {
		t.Fatalf("project remove exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: res.Trace, Fields: map[string]any{"status": "ok", "project_id": r.profile}})
	if names, ok := g10ListTools(t, connA1, r.profile); ok {
		t.Fatalf("the live connection of an enrolment on the removed profile still lists tools %v", names)
	}
	for _, id := range []string{"acme-a1", "acme-a2"} {
		if _, resp := g10GetEnrolment(t, i, id); resp.Status != 404 {
			t.Fatalf("GET of %s after its profile was removed answered %d, want 404", id, resp.Status)
		}
	}
	if _, ok := g10ListTools(t, connB1, profB); !ok {
		t.Fatalf("an enrolment on another profile lost its connection")
	}

	rows := map[string]g10RevokedRow{}
	for _, row := range g10RevokedRows(t, i) {
		if row.Credential != "enrolment" {
			continue
		}
		if _, dup := rows[row.Subject]; dup {
			t.Fatalf("more than one credential_revoked row for %s", row.Subject)
		}
		rows[row.Subject] = row
	}
	if len(rows) != 2 {
		t.Fatalf("credential_revoked enrolment rows %+v, want exactly acme-a1 and acme-a2", rows)
	}
	for _, id := range []string{"acme-a1", "acme-a2"} {
		row, ok := rows[id]
		if !ok || row.Cause != "profile_deleted" || row.Via != "cli" || row.Outcome != "ok" || len(row.Grants) != 1 || row.Grants[0] != r.profile {
			t.Fatalf("revocation row for %s is %+v (found=%v), want cause profile_deleted, via cli, ok, grants [%s]", id, row, ok, r.profile)
		}
	}

	// Through HTTP: the row names its own door.
	del := i.HTTP(i.Credential("configurer")).Do("DELETE", "/api/projects/"+profC, nil)
	if del.Status != 204 {
		t.Fatalf("DELETE /api/projects/%s answered %d, want 204", profC, del.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: del.Trace, Fields: map[string]any{"status": "ok", "project_id": profC}})
	if names, ok := g10ListTools(t, connC1, profC); ok {
		t.Fatalf("the live connection of an enrolment on the HTTP-removed profile still lists tools %v", names)
	}
	if _, resp := g10GetEnrolment(t, i, "acme-c1"); resp.Status != 404 {
		t.Fatalf("GET of acme-c1 after its profile was removed answered %d, want 404", resp.Status)
	}
	found := false
	for _, row := range g10RevokedRows(t, i) {
		if row.Subject == "acme-c1" {
			found = true
			if row.Cause != "profile_deleted" || row.Via != "http" {
				t.Fatalf("revocation row for acme-c1 is %+v, want cause profile_deleted via http", row)
			}
		}
	}
	if !found {
		t.Fatalf("no credential_revoked row for acme-c1")
	}

	// An unknown id answers 404 and revokes nothing.
	before := len(g10RevokedRows(t, i))
	missing := i.HTTP(i.Credential("configurer")).Do("DELETE", "/api/projects/unknown-profile", nil)
	if missing.Status != 404 {
		t.Fatalf("DELETE of an unknown profile answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
	if after := len(g10RevokedRows(t, i)); after != before {
		t.Fatalf("a remove of an unknown profile wrote %d revocation rows", after-before)
	}
	if _, resp := g10GetEnrolment(t, i, "acme-b1"); resp.Status != 200 {
		t.Fatalf("GET of acme-b1 answered %d, want 200: it names a profile that stayed", resp.Status)
	}
}

func TestEnrolmentRevokeKeepsProfile(t *testing.T) {
	t.Parallel()
	r := g10Start(t, map[string]harness.Outcome{"enrolment.sign": harness.OutcomeApprove, "enrolment.revoke": harness.OutcomeApprove})
	i := r.i
	g10Sign(t, r, "acme-keep")

	res := i.CLI("enrol", "revoke", "--client-id", "acme-keep")
	if res.Code != 0 {
		t.Fatalf("enrol revoke exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	if _, resp := g10GetEnrolment(t, i, "acme-keep"); resp.Status != 404 {
		t.Fatalf("GET of the revoked enrolment answered %d, want 404", resp.Status)
	}
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+r.profile, nil)
	if resp.Status != 200 {
		t.Fatalf("GET of the access profile after a revoke answered %d, want 200", resp.Status)
	}
	for _, row := range g10RevokedRows(t, i) {
		if row.Subject == "acme-keep" && row.Cause != "" {
			t.Fatalf("a direct revoke wrote cause %q, want none", row.Cause)
		}
	}
}
