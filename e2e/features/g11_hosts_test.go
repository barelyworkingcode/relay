package features

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"testing"

	"relaye2e/harness"
)

var g11Operator = []harness.CredentialSpec{{Name: "operator", Classes: []string{"read", "configure", "execute"}}}

// g11SeededHost is a host record planted in settings.json, the power path.
// It names a port nothing listens on: the tests that use it never connect.
const g11SeededHost = `[{"id":"h_acme0001","name":"acme-seeded","target":"acme@127.0.0.1","port":1,"created_at":"2026-01-01T00:00:00Z","terminal_templates":[{"id":"shell","name":"Shell"}]}]`

type g11Host struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Probe  *struct {
		OK bool `json:"ok"`
	} `json:"probe"`
	Templates []g11Template `json:"terminal_templates"`
}

type g11Template struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func g11SeededInstance(t *testing.T) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Credentials: g11Operator,
		Settings:    map[string]json.RawMessage{"hosts": json.RawMessage(g11SeededHost)},
	})
}

// g11Reachable starts an sshd, trusts it in a fresh instance and adds it as a
// host, which probes it.
func g11Reachable(t *testing.T, o harness.Options) (*harness.Instance, *harness.SSHHost, g11Host) {
	t.Helper()
	sshd := harness.StartSSHHost(t)
	o.Credentials = g11Operator
	i := harness.Start(t, o)
	i.TrustSSHHost(sshd)
	return i, sshd, g11Add(t, i, "acme-box", sshd)
}

func g11Add(t *testing.T, i *harness.Instance, name string, h *harness.SSHHost) g11Host {
	t.Helper()
	r := i.HTTP(i.Credential("operator")).Do("POST", "/api/hosts", map[string]any{
		"name": name, "target": h.Target, "port": h.Port, "identity_file": h.IdentityFile,
	})
	if r.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201", r.Status)
	}
	var host g11Host
	r.JSON(t, &host)
	if host.ID == "" {
		t.Fatalf("POST /api/hosts returned no host id")
	}
	return host
}

func g11ClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return port
}

func g11HostIDs(t *testing.T, i *harness.Instance) []string {
	t.Helper()
	r := i.HTTP(i.Credential("operator")).Do("GET", "/api/hosts", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/hosts answered %d, want 200", r.Status)
	}
	var hosts []g11Host
	r.JSON(t, &hosts)
	var ids []string
	for _, h := range hosts {
		ids = append(ids, h.ID)
	}
	return ids
}

func g11Templates(t *testing.T, i *harness.Instance, hostID string) (harness.Response, []g11Template) {
	t.Helper()
	r := i.HTTP(i.Credential("operator")).Do("GET", "/api/hosts/"+hostID+"/templates", nil)
	var list []g11Template
	if r.Status == 200 {
		r.JSON(t, &list)
	}
	return r, list
}

func g11HasTemplate(list []g11Template, id string) bool {
	for _, e := range list {
		if e.ID == id {
			return true
		}
	}
	return false
}

func g11RequireError(t *testing.T, i *harness.Instance, r harness.Response, key string, want int, reason string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s answered %d, want %d", key, r.Status, want)
	}
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: r.Trace, Fields: map[string]any{"status": "error", "reason": reason}})
}

func TestHostListAndGet(t *testing.T) {
	t.Parallel()
	i := g11SeededInstance(t)
	api := i.HTTP(i.Credential("operator"))

	list := api.Do("GET", "/api/hosts", nil)
	if list.Status != 200 {
		t.Fatalf("GET /api/hosts answered %d, want 200", list.Status)
	}
	if ids := g11HostIDs(t, i); len(ids) != 1 || ids[0] != "h_acme0001" {
		t.Fatalf("the list holds %v, want the one host planted in settings.json", ids)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	get := api.Do("GET", "/api/hosts/h_acme0001", nil)
	var got g11Host
	get.JSON(t, &got)
	if get.Status != 200 || got.ID != "h_acme0001" {
		t.Fatalf("GET host answered %d with id %q, want 200 and h_acme0001", get.Status, got.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.get", Trace: get.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme0001"}})

	missing := api.Do("GET", "/api/hosts/h_unknown", nil)
	g11RequireError(t, i, missing, "host.get", 404, "not_found")
}

func TestHostAdd(t *testing.T) {
	t.Parallel()
	sshd := harness.StartSSHHost(t)
	i := harness.Start(t, harness.Options{Credentials: g11Operator})
	i.TrustSSHHost(sshd)
	api := i.HTTP(i.Credential("operator"))

	r := api.Do("POST", "/api/hosts", map[string]any{
		"name": "acme-box", "target": sshd.Target, "port": sshd.Port, "identity_file": sshd.IdentityFile,
	})
	var host g11Host
	r.JSON(t, &host)
	if r.Status != 201 || host.ID == "" {
		t.Fatalf("POST /api/hosts answered %d with id %q, want 201 and an id", r.Status, host.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.create", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": host.ID}})
	if ids := g11HostIDs(t, i); len(ids) != 1 || ids[0] != host.ID {
		t.Fatalf("the list holds %v, want %s", ids, host.ID)
	}

	bad := api.Do("POST", "/api/hosts", map[string]any{"name": "acme-bad", "target": "acme box;rm"})
	g11RequireError(t, i, bad, "host.create", 400, "invalid")
	if ids := g11HostIDs(t, i); len(ids) != 1 {
		t.Fatalf("a refused add left %d hosts, want 1", len(ids))
	}
}

func TestHostEdit(t *testing.T) {
	t.Parallel()
	i := g11SeededInstance(t)
	api := i.HTTP(i.Credential("operator"))

	r := api.Do("PUT", "/api/hosts/h_acme0001", map[string]any{"name": "acme-renamed"})
	var host g11Host
	r.JSON(t, &host)
	if r.Status != 200 || host.Name != "acme-renamed" {
		t.Fatalf("PUT answered %d with name %q, want 200 and acme-renamed", r.Status, host.Name)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.update", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme0001"}})
	var got g11Host
	api.Do("GET", "/api/hosts/h_acme0001", nil).JSON(t, &got)
	if got.Name != "acme-renamed" {
		t.Fatalf("the host reads %q after the rename, want acme-renamed", got.Name)
	}

	missing := api.Do("PUT", "/api/hosts/h_unknown", map[string]any{"name": "acme-other"})
	g11RequireError(t, i, missing, "host.update", 404, "not_found")
}

func TestHostRemove(t *testing.T) {
	t.Parallel()
	i, sshd, used := g11Reachable(t, harness.Options{Presence: approveGrant})
	api := i.HTTP(i.Credential("operator"))
	other := g11Add(t, i, "acme-spare", sshd)

	i.MustCLI("host", "probe", "--id", other.ID, "--json")

	p := api.Do("POST", "/api/projects", map[string]any{"name": "acme-hosted", "path": "/srv/acme", "host_id": used.ID})
	if p.Status != 201 {
		t.Fatalf("creating a project on the host answered %d, want 201", p.Status)
	}
	blocked := api.Do("DELETE", "/api/hosts/"+used.ID, nil)
	if blocked.Status != 409 {
		t.Fatalf("DELETE of a host a project uses answered %d, want 409", blocked.Status)
	}
	var body struct {
		Projects []string `json:"projects"`
	}
	blocked.JSON(t, &body)
	if len(body.Projects) != 1 {
		t.Fatalf("the 409 names %v, want the one project", body.Projects)
	}

	r := api.Do("DELETE", "/api/hosts/"+other.ID, nil)
	if r.Status != 204 {
		t.Fatalf("DELETE answered %d, want 204", r.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.remove", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": other.ID}})
	for _, id := range g11HostIDs(t, i) {
		if id == other.ID {
			t.Fatalf("host %s is still listed after the removal", other.ID)
		}
	}
}

func TestHostProbe(t *testing.T) {
	t.Parallel()
	i, _, host := g11Reachable(t, harness.Options{})

	res := i.MustCLI("host", "probe", "--id", host.ID, "--json")
	var probed g11Host
	res.JSON(t, &probed)
	if probed.Probe == nil || !probed.Probe.OK {
		t.Fatalf("probe.ok is not true for a loopback sshd: %s", res.Stdout)
	}
	if probed.Status != "connected" && probed.Status != "idle" {
		t.Fatalf("status is %q after a good probe, want connected or idle", probed.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.probe", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": host.ID}})
	if rows := i.Audit(harness.AuditQuery{Event: "host.probe"}); len(rows) == 0 {
		t.Fatalf("no host.probe audit row after a probe")
	}

	dead := i.HTTP(i.Credential("operator")).Do("POST", "/api/hosts", map[string]any{
		"name": "acme-down", "target": "acme@127.0.0.1", "port": g11ClosedPort(t),
	})
	var down g11Host
	dead.JSON(t, &down)
	if dead.Status != 201 || down.ID == "" {
		t.Fatalf("adding an unreachable host answered %d, want 201", dead.Status)
	}
	res = i.MustCLI("host", "probe", "--id", down.ID, "--json")
	var missed g11Host
	res.JSON(t, &missed)
	if missed.Probe == nil || missed.Probe.OK || missed.Status != "unreachable" {
		t.Fatalf("an unreachable host read probe %+v status %q, want probe.ok false and unreachable", missed.Probe, missed.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.probe", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": down.ID}})
}

func TestHostDisconnect(t *testing.T) {
	t.Parallel()
	i, _, host := g11Reachable(t, harness.Options{})

	i.MustCLI("host", "probe", "--id", host.ID, "--json")
	res := i.MustCLI("host", "disconnect", "--id", host.ID, "--json")
	var after g11Host
	res.JSON(t, &after)
	if after.Status == "connected" {
		t.Fatalf("status is still connected after a disconnect")
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.disconnect", Trace: res.Trace, Fields: map[string]any{"status": "ok", "host_id": host.ID}})

	var again g11Host
	i.MustCLI("host", "probe", "--id", host.ID, "--json").JSON(t, &again)
	if again.Probe == nil || !again.Probe.OK {
		t.Fatalf("the probe after a disconnect did not reconnect")
	}
}

func TestHostPasteTmp(t *testing.T) {
	t.Parallel()
	i, _, host := g11Reachable(t, harness.Options{})
	payload := []byte("acme pasted bytes")

	r := i.SocketHTTP(i.Credential("operator")).Do("POST", "/api/hosts/"+host.ID+"/pastetmp", map[string]any{
		"name": "acme.png", "data_b64": base64.StdEncoding.EncodeToString(payload),
	})
	var out struct {
		Path string `json:"path"`
	}
	r.JSON(t, &out)
	if r.Status != 200 || out.Path == "" {
		t.Fatalf("pastetmp answered %d with path %q, want 200 and a path; body %s", r.Status, out.Path, r.Body)
	}
	t.Cleanup(func() { _ = os.Remove(out.Path) })
	got, err := os.ReadFile(out.Path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the file at %s holds %q (err %v), want the pasted bytes", out.Path, got, err)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host.pastetmp", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": host.ID}})
}

func TestHostTemplateList(t *testing.T) {
	t.Parallel()
	i, _, host := g11Reachable(t, harness.Options{})

	r, list := g11Templates(t, i, host.ID)
	if r.Status != 200 || !g11HasTemplate(list, "shell") {
		t.Fatalf("templates answered %d with %v, want 200 and the seeded shell", r.Status, list)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host_template.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": host.ID, "count": len(list)}})
}

func TestHostTemplateAdd(t *testing.T) {
	t.Parallel()
	i := g11SeededInstance(t)
	api := i.HTTP(i.Credential("operator"))

	r := api.Do("POST", "/api/hosts/h_acme0001/templates", map[string]any{"id": "acme-tool", "name": "Acme tool", "command": "/bin/echo"})
	if r.Status != 201 {
		t.Fatalf("POST template answered %d, want 201", r.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host_template.create", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme0001", "template_id": "acme-tool"}})
	if _, list := g11Templates(t, i, "h_acme0001"); !g11HasTemplate(list, "acme-tool") {
		t.Fatalf("the list %v lacks the added template", list)
	}

	bad := api.Do("POST", "/api/hosts/h_acme0001/templates", map[string]any{"id": "acme-nameless"})
	g11RequireError(t, i, bad, "host_template.create", 400, "invalid")
}

func TestHostTemplateEdit(t *testing.T) {
	t.Parallel()
	i := g11SeededInstance(t)
	api := i.HTTP(i.Credential("operator"))

	r := api.Do("PUT", "/api/hosts/h_acme0001/templates/shell", map[string]any{"name": "Acme shell"})
	var got g11Template
	r.JSON(t, &got)
	if r.Status != 200 || got.Name != "Acme shell" {
		t.Fatalf("PUT template answered %d with name %q, want 200 and Acme shell", r.Status, got.Name)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host_template.update", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme0001", "template_id": "shell"}})

	missing := api.Do("PUT", "/api/hosts/h_acme0001/templates/nope", map[string]any{"name": "Acme none"})
	g11RequireError(t, i, missing, "host_template.update", 404, "not_found")
}

func TestHostTemplateRemove(t *testing.T) {
	t.Parallel()
	i := g11SeededInstance(t)
	api := i.HTTP(i.Credential("operator"))

	r := api.Do("DELETE", "/api/hosts/h_acme0001/templates/shell", nil)
	if r.Status != 204 {
		t.Fatalf("DELETE template answered %d, want 204", r.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "host_template.remove", Trace: r.Trace, Fields: map[string]any{"status": "ok", "host_id": "h_acme0001", "template_id": "shell"}})
	if _, list := g11Templates(t, i, "h_acme0001"); g11HasTemplate(list, "shell") {
		t.Fatalf("the list %v still holds the removed template", list)
	}
}
