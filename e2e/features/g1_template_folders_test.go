package features

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"relaye2e/harness"
)

// g1fTemplate is the terminal template shape the routes answer.
type g1fTemplate struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Sandbox   bool     `json:"sandbox"`
	Read      []string `json:"read"`
	ReadWrite []string `json:"read_write"`
}

// g1fGetTemplate reads one template by id and returns the HTTP status.
func g1fGetTemplate(t *testing.T, i *harness.Instance, id string) (g1fTemplate, int) {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("reader")).Do("GET", "/api/terminal/templates/"+id, nil)
	var tmpl g1fTemplate
	if resp.Status == 200 {
		resp.JSON(t, &tmpl)
	}
	return tmpl, resp.Status
}

// g1fSettingsFile reads the settings.json relay persisted.
func g1fSettingsFile(t *testing.T, i *harness.Instance) (version int, templateIDs []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(i.ConfigDir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var s struct {
		Version   int `json:"version"`
		Templates []struct {
			ID string `json:"id"`
		} `json:"terminal_templates"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("decoding settings.json: %v", err)
	}
	for _, tm := range s.Templates {
		templateIDs = append(templateIDs, tm.ID)
	}
	return s.Version, templateIDs
}

func TestTemplateFolderProjectPath(t *testing.T) {
	t.Parallel()
	tmpl := json.RawMessage(`[
 {"id":"acme-reach","name":"Acme reach","command":"/bin/cat","args":["../acme-shared/note.txt"],"sandbox":true,"read":["${PROJECT_PATH}/../acme-shared"]},
 {"id":"acme-noreach","name":"Acme no reach","command":"/bin/cat","args":["../acme-shared/note.txt"],"sandbox":true}
]`)
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-proj", map[string]any{"allowed_templates": []string{"acme-reach", "acme-noreach"}})
	shared := g1tDir(i, "acme-shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatalf("creating %s: %v", shared, err)
	}
	if err := os.WriteFile(filepath.Join(shared, "note.txt"), []byte("acme shared\n"), 0o600); err != nil {
		t.Fatalf("writing the shared note: %v", err)
	}
	dir := g1tDir(i, "acme-proj")

	trace := harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-reach").Wait(); res.Code != 0 {
		t.Fatalf("a read of the sibling folder a ${PROJECT_PATH} entry grants exited %d, want 0", res.Code)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "ok", "template": "acme-reach", "project_id": p.ID}})

	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "sandbox", "acme-noreach").Wait(); res.Code == 0 {
		t.Fatalf("the same read from a template with no folder exited 0, want a refusal")
	}
}

func TestTemplateFolderUnknownTokenRefused(t *testing.T) {
	t.Parallel()
	supplied := json.RawMessage(`[{"id":"acme-badtoken","name":"Acme bad token","command":"/bin/true","sandbox":true,"read":["${PROJECT_ID}"]}]`)
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": supplied},
	})
	p := g1tProject(t, i, "acme-tokens", map[string]any{"allowed_templates": []string{"*"}})
	cfg := i.SocketHTTP(i.Credential("configurer"))
	reader := i.SocketHTTP(i.Credential("reader"))

	if _, status := g1fGetTemplate(t, i, "acme-badtoken"); status != 404 {
		t.Fatalf("GET of a template supplied with a ${PROJECT_ID} folder answered %d, want 404", status)
	}
	var listed []g1fTemplate
	reader.Do("GET", "/api/terminal/templates?project="+p.ID, nil).JSON(t, &listed)
	for _, row := range listed {
		if row.ID == "acme-badtoken" {
			t.Fatalf("the list holds the template supplied with a ${PROJECT_ID} folder")
		}
	}

	good := cfg.Do("POST", "/api/terminal/templates", map[string]any{
		"id": "acme-edit", "name": "Acme edit", "command": "/bin/true", "sandbox": true,
		"read_write": []string{"${PROJECT_PATH}/build"},
	})
	if good.Status != 201 {
		t.Fatalf("POST of a ${PROJECT_PATH}/build folder answered %d, want 201", good.Status)
	}

	bad := []struct{ field, entry string }{
		{"read_write", "${HOME}/x"},
		{"read", "${PROJECT_PATH}x"},
		{"deny", "~/${PROJECT_PATH}"},
		{"read_write", "${PROJECT_ID}"},
	}
	for n, c := range bad {
		body := map[string]any{
			"id": fmt.Sprintf("acme-bad-%d", n), "name": "Acme bad", "command": "/bin/true", "sandbox": true,
			c.field: []string{c.entry},
		}
		trace := harness.NewTrace(t)
		if resp := cfg.Do("POST", "/api/terminal/templates", body, harness.ReqOpts{Trace: trace}); resp.Status != 400 {
			t.Fatalf("POST with %s %q answered %d, want 400", c.field, c.entry, resp.Status)
		}
		g1tEvent(t, i, harness.EventQuery{Key: "template.create", Trace: trace,
			Fields: map[string]any{"status": "error", "reason": "invalid"}})
		if _, status := g1fGetTemplate(t, i, fmt.Sprintf("acme-bad-%d", n)); status != 404 {
			t.Fatalf("the refused template with %s %q is readable (%d), want 404", c.field, c.entry, status)
		}

		delete(body, "id")
		trace = harness.NewTrace(t)
		if resp := cfg.Do("PUT", "/api/terminal/templates/acme-edit", body, harness.ReqOpts{Trace: trace}); resp.Status != 400 {
			t.Fatalf("PUT with %s %q answered %d, want 400", c.field, c.entry, resp.Status)
		}
		g1tEvent(t, i, harness.EventQuery{Key: "template.update", Trace: trace,
			Fields: map[string]any{"status": "error", "reason": "invalid"}})
	}

	kept, status := g1fGetTemplate(t, i, "acme-edit")
	if status != 200 || !reflect.DeepEqual(kept.ReadWrite, []string{"${PROJECT_PATH}/build"}) {
		t.Fatalf("after the refused PUTs the template answers %d with read_write %v, want 200 and the original folder", status, kept.ReadWrite)
	}
}

const g1fReadOnlyClaude = `[{"id":"claude-code","name":"Claude Code","command":"claude","sandbox":true,
 "read_write":["~/.claude","${PROJECT_PATH}"],"read":["~/.local/bin"]}]`

func TestReadOnlyProjectPathFolderNotWritable(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": json.RawMessage(g1fReadOnlyClaude)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	work := g1tProject(t, i, "acme-ro-work", map[string]any{"allowed_templates": []string{"claude-code"}})
	dir := g1tDir(i, "acme-ro-work")
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("acme\n"), 0o600); err != nil {
		t.Fatalf("writing the project's file: %v", err)
	}
	written := filepath.Join(dir, "written.txt")

	launch := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", map[string]any{
		"projectId": work.ID, "model": "sonnet", "settings": map[string]any{"readOnlyProjects": true},
	})
	if launch.Status != 201 {
		t.Fatalf("a read-only claude launch answered %d, want 201", launch.Status)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	launch.JSON(t, &s)

	var reply struct {
		Text string `json:"text"`
	}
	i.MustCLI("session", "message", "--id", s.SessionID, "--text", "!sh /bin/cat "+existing, "--json").JSON(t, &reply)
	if reply.Text != "exit: 0" {
		t.Fatalf("a read of the project's file answered %q, want exit: 0 (the folder is applied as a read grant)", reply.Text)
	}

	i.MustCLI("session", "message", "--id", s.SessionID, "--text", "!sh touch "+written, "--json").JSON(t, &reply)
	if reply.Text == "exit: 0" {
		t.Fatalf("a write to the project folder answered exit: 0, want a refusal")
	}
	if _, err := os.Stat(written); err == nil {
		t.Fatalf("the refused write left a file in the project folder")
	}
}

func TestAgentTemplatesMigratedOnce(t *testing.T) {
	t.Parallel()
	acmePi := json.RawMessage(`[{"id":"pi","name":"Acme Pi","command":"pi","sandbox":true,"read_write":["~/.acme-pi"],"read":["~/.local/bin"]}]`)
	i := harness.Start(t, harness.Options{
		Credentials:        g1tCreds,
		NoDefaultTemplates: true,
		Settings: map[string]json.RawMessage{
			"version":            json.RawMessage(`2`),
			"terminal_templates": acmePi,
		},
	})

	claude, status := g1fGetTemplate(t, i, "claude-code")
	if status != 200 {
		t.Fatalf("GET claude-code after the first start answered %d, want 200", status)
	}
	if claude.Command != "claude" || !claude.Sandbox ||
		!reflect.DeepEqual(claude.ReadWrite, []string{"~/.claude", "~/.claude.json", "~/.cache", "~/Library/Caches"}) ||
		!reflect.DeepEqual(claude.Read, []string{"~/Library/Keychains", "~/.local/bin", "~/.local/share/claude", "/opt/homebrew"}) {
		t.Fatalf("the migrated claude-code template is %+v, want the documented definition", claude)
	}
	pi, status := g1fGetTemplate(t, i, "pi")
	if status != 200 || pi.Name != "Acme Pi" || !reflect.DeepEqual(pi.ReadWrite, []string{"~/.acme-pi"}) || !reflect.DeepEqual(pi.Read, []string{"~/.local/bin"}) {
		t.Fatalf("the supplied pi template reads %d %+v, want it kept as supplied", status, pi)
	}
	if version, _ := g1fSettingsFile(t, i); version != 3 {
		t.Fatalf("settings.json reads version %d after the migration, want 3", version)
	}

	del := i.SocketHTTP(i.Credential("configurer")).Do("DELETE", "/api/terminal/templates/claude-code", nil)
	if del.Status != 204 {
		t.Fatalf("DELETE claude-code answered %d, want 204", del.Status)
	}
	i.Restart()
	if _, status := g1fGetTemplate(t, i, "claude-code"); status != 404 {
		t.Fatalf("GET claude-code after a restart answered %d, want 404: a removed template stays removed", status)
	}
	version, before := g1fSettingsFile(t, i)
	if version != 3 {
		t.Fatalf("settings.json reads version %d after the restart, want 3", version)
	}
	i.Restart()
	if _, after := g1fSettingsFile(t, i); !reflect.DeepEqual(before, after) {
		t.Fatalf("a second restart changed the templates from %v to %v, want no change", before, after)
	}
	if _, status := g1fGetTemplate(t, i, "claude-code"); status != 404 {
		t.Fatalf("GET claude-code after the second restart answered %d, want 404", status)
	}

	v3 := harness.Start(t, harness.Options{
		Credentials:        g1tCreds,
		NoDefaultTemplates: true,
		Settings:           map[string]json.RawMessage{"version": json.RawMessage(`3`)},
	})
	for _, id := range []string{"claude-code", "pi"} {
		if _, status := g1fGetTemplate(t, v3, id); status != 404 {
			t.Fatalf("a version 3 start added %s (GET answered %d), want nothing added", id, status)
		}
	}
}
