package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

// A host whose shell caps the command line (Windows OpenSSH with cmd.exe, 8,191
// characters) must still start the file agent and serve the project's files.
func TestBug427HostFilesUnderCommandLineCap(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{
			{Name: "runner", Classes: []string{"execute"}},
			{Name: "configurer", Classes: []string{"configure"}},
		},
		Presence: approveGrant,
		SSHStub:  true,
	})
	// The cap exists before the host does, so the probe runs under it too.
	i.SSHCommandLineCap("acme@winbox", 8191)

	hr := i.SocketHTTP(i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{"name": "Acme Win", "target": "acme@winbox"})
	if hr.Status != http.StatusCreated {
		t.Fatalf("POST /api/hosts answered %d, want 201: %s", hr.Status, hr.Body)
	}
	var h struct {
		ID string `json:"id"`
	}
	hr.JSON(t, &h)

	dir := filepath.Join(i.RemoteRoot(), "winproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("acme notes\n"), 0o600); err != nil {
		t.Fatalf("planting notes.txt: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme Win Project", "path": dir, "host_id": h.ID})
	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create for a host project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p project
	res.JSON(t, &p)

	c := i.SocketHTTP(i.Credential("runner"))
	lr := c.Do("POST", "/api/projects/"+p.ID+"/files/list", map[string]any{"path": ""})
	if lr.Status != http.StatusOK {
		t.Fatalf("POST files/list answered %d (%s), want 200", lr.Status, lr.Body)
	}
	var listed struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	lr.JSON(t, &listed)
	found := false
	for _, e := range listed.Entries {
		found = found || e.Name == "notes.txt"
	}
	if !found {
		t.Fatalf("files/list did not list notes.txt: %s", lr.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "file.list", Trace: lr.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	rr := c.Do("POST", "/api/projects/"+p.ID+"/files/read", map[string]any{"path": "notes.txt"})
	if rr.Status != http.StatusOK {
		t.Fatalf("POST files/read answered %d (%s), want 200", rr.Status, rr.Body)
	}
	var read struct {
		Content string `json:"content"`
	}
	rr.JSON(t, &read)
	if read.Content != "acme notes\n" {
		t.Fatalf("files/read content %q, want the planted text", read.Content)
	}
}
