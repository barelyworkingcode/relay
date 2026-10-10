package features

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"testing"

	"relaye2e/harness"
)

var g11Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "executer", Classes: []string{"execute"}},
}

// g11Host is the host object of docs/routes.md, as far as these tests read it.
type g11Host struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Target       string `json:"target"`
	Port         int    `json:"port"`
	IdentityFile string `json:"identity_file"`
	Status       string `json:"status"`
	Probe        struct {
		OK bool `json:"ok"`
	} `json:"probe"`
	Templates []g11Template `json:"terminal_templates"`
}

type g11Template struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"`
}

type g11Rig struct {
	i    *harness.Instance
	ssh  *harness.SSHHost
	conf *harness.Client
}

func g11Start(t *testing.T, mut ...func(*harness.Options)) *g11Rig {
	t.Helper()
	host := harness.StartSSHHost(t)
	o := harness.Options{
		Credentials: g11Creds,
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
	}
	for _, m := range mut {
		m(&o)
	}
	i := harness.Start(t, o)
	i.TrustSSHHost(host)
	return &g11Rig{i: i, ssh: host, conf: i.HTTP(i.Credential("configurer"))}
}

// add creates a host for the rig's sshd and returns it.
func (r *g11Rig) add(t *testing.T, name string) g11Host {
	t.Helper()
	resp := r.conf.Do("POST", "/api/hosts", map[string]any{
		"name": name, "target": r.ssh.Target, "port": r.ssh.Port, "identity_file": r.ssh.IdentityFile,
	})
	if resp.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201\n%s", resp.Status, resp.Body)
	}
	var h g11Host
	resp.JSON(t, &h)
	if h.ID == "" {
		t.Fatalf("POST /api/hosts returned no host id")
	}
	return h
}

func (r *g11Rig) list(t *testing.T) []g11Host {
	t.Helper()
	resp := r.i.HTTP(r.i.Credential("reader")).Do("GET", "/api/hosts", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/hosts answered %d, want 200", resp.Status)
	}
	var hs []g11Host
	resp.JSON(t, &hs)
	return hs
}

func (r *g11Rig) get(t *testing.T, id string) (g11Host, harness.Response) {
	t.Helper()
	resp := r.i.HTTP(r.i.Credential("reader")).Do("GET", "/api/hosts/"+id, nil)
	var h g11Host
	if resp.Status == 200 {
		resp.JSON(t, &h)
	}
	return h, resp
}

func g11Has(hs []g11Host, id string) bool {
	for _, h := range hs {
		if h.ID == id {
			return true
		}
	}
	return false
}

func (r *g11Rig) templates(t *testing.T, hostID string) ([]g11Template, harness.Response) {
	t.Helper()
	resp := r.i.HTTP(r.i.Credential("reader")).Do("GET", "/api/hosts/"+hostID+"/templates", nil)
	var ts []g11Template
	if resp.Status == 200 {
		resp.JSON(t, &ts)
	}
	return ts, resp
}

func g11HasTemplate(ts []g11Template, id string) bool {
	for _, x := range ts {
		if x.ID == id {
			return true
		}
	}
	return false
}

func TestHostListAndGet(t *testing.T) {
	t.Parallel()
	// The seed is the power door: settings.json hosts[].
	seed := `[{"id":"h_acme01","name":"acme-seed","target":"acme@127.0.0.1","port":2222,"created_at":"2026-01-01T00:00:00Z"}]`
	i := harness.Start(t, harness.Options{
		Credentials: g11Creds,
		Settings:    map[string]json.RawMessage{"hosts": json.RawMessage(seed)},
	})
	reader := i.HTTP(i.Credential("reader"))

	resp := reader.Do("GET", "/api/hosts", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/hosts answered %d, want 200", resp.Status)
	}
	var hs []g11Host
	resp.JSON(t, &hs)
	if len(hs) != 1 || hs[0].ID != "h_acme01" {
		t.Fatalf("GET /api/hosts lists %+v, want the seeded host h_acme01", hs)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	resp = reader.Do("GET", "/api/hosts/h_acme01", nil)
	var h g11Host
	if resp.Status != 200 {
		t.Fatalf("GET /api/hosts/h_acme01 answered %d, want 200", resp.Status)
	}
	resp.JSON(t, &h)
	if h.Name != "acme-seed" || h.Target != "acme@127.0.0.1" || h.Port != 2222 || h.Status != "unknown" {
		t.Fatalf("the seeded host reads %+v, want its name, target, port and status unknown", h)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.get", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme01"}})

	resp = reader.Do("GET", "/api/hosts/h_missing", nil)
	if resp.Status != 404 {
		t.Fatalf("GET of an unknown host answered %d, want 404", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.get", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestHostAdd(t *testing.T) {
	t.Parallel()
	r := g11Start(t)

	resp := r.conf.Do("POST", "/api/hosts", map[string]any{
		"name": "acme-box", "target": r.ssh.Target, "port": r.ssh.Port, "identity_file": r.ssh.IdentityFile,
	})
	if resp.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201\n%s", resp.Status, resp.Body)
	}
	var created g11Host
	resp.JSON(t, &created)
	requireEvent(t, r.i, harness.EventQuery{Key: "host.create", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": created.ID}})
	got, _ := r.get(t, created.ID)
	if got.Name != "acme-box" || got.Target != r.ssh.Target || got.Port != r.ssh.Port || got.IdentityFile != r.ssh.IdentityFile {
		t.Fatalf("the saved host reads %+v, want the name, target, port and identity file posted", got)
	}

	before := len(r.list(t))
	bad := r.conf.Do("POST", "/api/hosts", map[string]any{"name": "acme-bad", "target": "acme box;true"})
	if bad.Status != 400 {
		t.Fatalf("POST /api/hosts with a malformed target answered %d, want 400", bad.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host.create", Trace: bad.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
	if after := len(r.list(t)); after != before {
		t.Fatalf("a refused add changed the host count from %d to %d", before, after)
	}
}

func TestHostEdit(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")

	resp := r.conf.Do("PUT", "/api/hosts/"+h.ID, map[string]any{"name": "acme-renamed"})
	if resp.Status != 200 {
		t.Fatalf("PUT /api/hosts/%s answered %d, want 200\n%s", h.ID, resp.Status, resp.Body)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID}})
	if got, _ := r.get(t, h.ID); got.Name != "acme-renamed" || got.Target != h.Target {
		t.Fatalf("the edited host reads %+v, want the new name and the old target", got)
	}

	missing := r.conf.Do("PUT", "/api/hosts/h_missing", map[string]any{"name": "acme-x"})
	if missing.Status != 404 {
		t.Fatalf("PUT of an unknown host answered %d, want 404", missing.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host.update", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestHostRemove(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	free := r.add(t, "acme-free")
	used := r.add(t, "acme-used")
	r.i.MustCLI("host", "probe", "--id", free.ID, "--json")

	resp := r.conf.Do("DELETE", "/api/hosts/"+free.ID, nil)
	if resp.Status != 204 {
		t.Fatalf("DELETE /api/hosts/%s answered %d, want 204", free.ID, resp.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host.remove", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": free.ID}})
	if g11Has(r.list(t), free.ID) {
		t.Fatalf("the removed host is still listed")
	}

	proj := r.conf.Do("POST", "/api/projects", map[string]any{"name": "acme-hosted", "path": "/home/acme/work", "host_id": used.ID})
	if proj.Status != 201 {
		t.Fatalf("POST /api/projects on a host answered %d, want 201\n%s", proj.Status, proj.Body)
	}
	refused := r.conf.Do("DELETE", "/api/hosts/"+used.ID, nil)
	if refused.Status != 409 {
		t.Fatalf("DELETE of a host a project uses answered %d, want 409", refused.Status)
	}
	if !g11Has(r.list(t), used.ID) {
		t.Fatalf("the host a project uses vanished after a refused remove")
	}
}

// g11ClosedPort returns a loopback port nothing listens on.
func g11ClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return port
}

type g11Probed struct {
	Probe struct {
		OK bool `json:"ok"`
	} `json:"probe"`
	Status string `json:"status"`
}

func g11ProbeRows(i *harness.Instance, hostID string) (n int, last map[string]any) {
	for _, row := range i.Audit(harness.AuditQuery{Event: "host.probe"}) {
		if args, _ := row["args"].(map[string]any); args["host_id"] == hostID {
			n++
			last = row
		}
	}
	return n, last
}

func TestHostProbe(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	i := r.i
	h := r.add(t, "acme-box")
	rowsBefore, _ := g11ProbeRows(i, h.ID)

	res := i.MustCLI("host", "probe", "--id", h.ID, "--json")
	var probed g11Probed
	res.JSON(t, &probed)
	if !probed.Probe.OK || (probed.Status != "connected" && probed.Status != "idle") {
		t.Fatalf("probe of a reachable host reads %+v, want probe.ok and status connected or idle", probed)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.probe", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID}})
	if n, last := g11ProbeRows(i, h.ID); n <= rowsBefore || last["outcome"] != "ok" {
		t.Fatalf("host.probe audit rows for the host went from %d to %d, last %v, want a new ok row", rowsBefore, n, last)
	}

	resp := r.conf.Do("POST", "/api/hosts", map[string]any{
		"name": "acme-closed", "target": "acme@127.0.0.1", "port": g11ClosedPort(t), "identity_file": r.ssh.IdentityFile,
	})
	if resp.Status != 201 {
		t.Fatalf("POST /api/hosts for an unreachable host answered %d, want 201", resp.Status)
	}
	var closed g11Host
	resp.JSON(t, &closed)
	res = i.MustCLI("host", "probe", "--id", closed.ID, "--json")
	res.JSON(t, &probed)
	if probed.Probe.OK || probed.Status != "unreachable" {
		t.Fatalf("probe of a closed port reads %+v, want probe.ok false and status unreachable", probed)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.probe", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": closed.ID}})
	if _, last := g11ProbeRows(i, closed.ID); last["outcome"] != "error" {
		t.Fatalf("the audit row of the failed probe is %v, want outcome error", last)
	}
}

func TestHostDisconnect(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	i := r.i
	h := r.add(t, "acme-box")
	var probed g11Probed
	i.MustCLI("host", "probe", "--id", h.ID, "--json").JSON(t, &probed)
	if probed.Status != "connected" {
		t.Fatalf("status after a probe is %q, want connected", probed.Status)
	}

	res := i.MustCLI("host", "disconnect", "--id", h.ID, "--json")
	res.JSON(t, &probed)
	requireEvent(t, i, harness.EventQuery{Key: "host.disconnect", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID}})
	if probed.Status == "connected" {
		t.Fatalf("status after a disconnect is still connected")
	}
	if got, _ := r.get(t, h.ID); got.Status == "connected" {
		t.Fatalf("GET of the host still reads connected after a disconnect")
	}

	i.MustCLI("host", "probe", "--id", h.ID, "--json").JSON(t, &probed)
	if !probed.Probe.OK {
		t.Fatalf("the probe after a disconnect did not reach the host")
	}
}

func TestHostPasteTmp(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")
	data := []byte("acme paste bytes \x00\x01\x02")

	resp := r.i.SocketHTTP(r.i.Credential("executer")).Do("POST", "/api/hosts/"+h.ID+"/pastetmp", map[string]any{
		"name": "eve-paste-1700000000-ab12.png", "data_b64": base64.StdEncoding.EncodeToString(data),
	})
	if resp.Status != 200 {
		t.Fatalf("POST pastetmp answered %d, want 200\n%s", resp.Status, resp.Body)
	}
	var out struct {
		Path string `json:"path"`
	}
	resp.JSON(t, &out)
	if out.Path == "" {
		t.Fatalf("pastetmp answered no path")
	}
	t.Cleanup(func() { _ = os.Remove(out.Path) })
	got, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatalf("reading the pasted file %s: %v", out.Path, err)
	}
	if string(got) != string(data) {
		t.Fatalf("the pasted file holds %d bytes that differ from the %d sent", len(got), len(data))
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host.pastetmp", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID}})
}

func TestHostPasteTmpRefusesOtherNames(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")
	exec := r.i.SocketHTTP(r.i.Credential("executer"))
	payload := base64.StdEncoding.EncodeToString([]byte("acme"))

	for _, name := range []string{"notes.txt", "../eve-paste-1700000000-ab12.png", "eve-paste-1-ab12.exe", "eve-paste-x-ab12.png"} {
		resp := exec.Do("POST", "/api/hosts/"+h.ID+"/pastetmp", map[string]any{"name": name, "data_b64": payload})
		var body struct {
			Code string `json:"code"`
		}
		resp.JSON(t, &body)
		if resp.Status != 400 || body.Code != "INVALID" {
			t.Fatalf("pastetmp with name %q answered %d %q, want 400 INVALID", name, resp.Status, body.Code)
		}
	}
}

func TestHostTemplateList(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")

	ts, resp := r.templates(t, h.ID)
	if resp.Status != 200 {
		t.Fatalf("GET templates answered %d, want 200", resp.Status)
	}
	if !g11HasTemplate(ts, "shell") {
		t.Fatalf("the templates after the first good probe are %+v, want the seeded shell", ts)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID, "count": len(ts)}})
}

func TestHostTemplateAdd(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")
	path := "/api/hosts/" + h.ID + "/templates"

	resp := r.conf.Do("POST", path, map[string]any{"id": "acme-top", "name": "Acme top", "command": "/usr/bin/top"})
	if resp.Status != 201 {
		t.Fatalf("POST templates answered %d, want 201\n%s", resp.Status, resp.Body)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.create", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID, "template_id": "acme-top"}})
	if ts, _ := r.templates(t, h.ID); !g11HasTemplate(ts, "acme-top") {
		t.Fatalf("the added template is not listed: %+v", ts)
	}

	bad := r.conf.Do("POST", path, map[string]any{"id": "acme-nameless", "command": "/usr/bin/top"})
	if bad.Status != 400 {
		t.Fatalf("POST of a template with no name answered %d, want 400", bad.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.create", Trace: bad.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
	if ts, _ := r.templates(t, h.ID); g11HasTemplate(ts, "acme-nameless") {
		t.Fatalf("a refused template was saved")
	}
}

func TestHostTemplateEdit(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")
	base := "/api/hosts/" + h.ID + "/templates"
	if resp := r.conf.Do("POST", base, map[string]any{"id": "acme-top", "name": "Acme top", "command": "/usr/bin/top"}); resp.Status != 201 {
		t.Fatalf("POST templates answered %d, want 201", resp.Status)
	}

	resp := r.conf.Do("PUT", base+"/acme-top", map[string]any{"name": "Acme renamed", "command": "/usr/bin/top"})
	if resp.Status != 200 {
		t.Fatalf("PUT template answered %d, want 200\n%s", resp.Status, resp.Body)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID, "template_id": "acme-top"}})
	renamed := false
	ts, _ := r.templates(t, h.ID)
	for _, x := range ts {
		renamed = renamed || (x.ID == "acme-top" && x.Name == "Acme renamed")
	}
	if !renamed {
		t.Fatalf("the edited template is not listed with its new name: %+v", ts)
	}

	missing := r.conf.Do("PUT", base+"/acme-missing", map[string]any{"name": "Acme x", "command": "/usr/bin/top"})
	if missing.Status != 404 {
		t.Fatalf("PUT of an unknown template answered %d, want 404", missing.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.update", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestHostTemplateRemove(t *testing.T) {
	t.Parallel()
	r := g11Start(t)
	h := r.add(t, "acme-box")
	base := "/api/hosts/" + h.ID + "/templates"
	if resp := r.conf.Do("POST", base, map[string]any{"id": "acme-top", "name": "Acme top", "command": "/usr/bin/top"}); resp.Status != 201 {
		t.Fatalf("POST templates answered %d, want 201", resp.Status)
	}

	resp := r.conf.Do("DELETE", base+"/acme-top", nil)
	if resp.Status != 204 {
		t.Fatalf("DELETE template answered %d, want 204", resp.Status)
	}
	requireEvent(t, r.i, harness.EventQuery{Key: "host_template.remove", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "host_id": h.ID, "template_id": "acme-top"}})
	ts, _ := r.templates(t, h.ID)
	if g11HasTemplate(ts, "acme-top") {
		t.Fatalf("the removed template is still listed: %+v", ts)
	}
	if !g11HasTemplate(ts, "shell") {
		t.Fatalf("the seeded templates changed on a removal: %+v", ts)
	}
}
