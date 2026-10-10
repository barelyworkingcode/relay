package features

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

const g13Deadline = 60 * time.Second

type g13Env struct {
	i   *harness.Instance
	p   project
	dir string
	c   *harness.Client
}

// g13Setup boots an instance with an execute credential and one project whose
// folder is empty. File routes are socket-only, so the client is the socket's.
func g13Setup(t *testing.T) g13Env {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}},
		Presence:    approveGrant,
	})
	p, _ := createProject(t, i, "acme-files")
	return g13Env{i: i, p: p, dir: filepath.Join(i.Home, "work", "acme-files"), c: i.SocketHTTP(i.Credential("runner"))}
}

func (e g13Env) route(op string) string { return "/api/projects/" + e.p.ID + "/files/" + op }

func (e g13Env) plant(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the folder of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("planting %s: %v", path, err)
	}
}

// post sends one file route and decodes the JSON answer.
func (e g13Env) post(t *testing.T, op string, body any) (harness.Response, map[string]any) {
	t.Helper()
	resp := e.c.Do("POST", e.route(op), body)
	var out map[string]any
	_ = json.Unmarshal(resp.Body, &out)
	return resp, out
}

// ok sends a file route that must answer 200 and requires its ok event.
func (e g13Env) ok(t *testing.T, op string, body any) map[string]any {
	t.Helper()
	resp, out := e.post(t, op, body)
	if resp.Status != http.StatusOK {
		t.Fatalf("POST files/%s answered %d (%s), want 200", op, resp.Status, resp.Body)
	}
	requireEvent(t, e.i, harness.EventQuery{Key: "file." + op, Trace: resp.Trace, Fields: map[string]any{"status": "ok", "project_id": e.p.ID}})
	return out
}

// refused sends a file route that must answer 403 with the given code and
// requires a denied event for it.
func (e g13Env) refused(t *testing.T, op string, body any, code, reason string) {
	t.Helper()
	resp, out := e.post(t, op, body)
	if resp.Status != http.StatusForbidden || out["code"] != code {
		t.Fatalf("POST files/%s answered %d (%s), want 403 %s", op, resp.Status, resp.Body, code)
	}
	requireEvent(t, e.i, harness.EventQuery{Key: "file." + op, Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": reason, "project_id": e.p.ID}})
}

// fileOps returns the file_op audit rows of a tool and outcome for the project.
func (e g13Env) fileOps(tool, outcome string) []map[string]any {
	var out []map[string]any
	for _, r := range e.i.Audit(harness.AuditQuery{Event: "file_op", Outcome: outcome}) {
		actor, _ := r["actor"].(map[string]any)
		if r["tool"] == tool && actor["project_id"] == e.p.ID {
			out = append(out, r)
		}
	}
	return out
}

func (e g13Env) read(t *testing.T, path string) string {
	t.Helper()
	_, out := e.post(t, "read", map[string]any{"path": path})
	s, _ := out["content"].(string)
	return s
}

func TestFileReadOps(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	e.plant(t, "hello.txt", "hello acme\n")
	e.plant(t, "sub/nested.txt", "nested\n")
	git := exec.Command("git", "init", "-q", e.dir)
	git.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init of the fixture folder: %v\n%s", err, out)
	}

	list := e.ok(t, "list", map[string]any{"path": ""})
	types := map[string]string{}
	entries, _ := list["entries"].([]any)
	for _, raw := range entries {
		m, _ := raw.(map[string]any)
		name, _ := m["name"].(string)
		typ, _ := m["type"].(string)
		types[name] = typ
	}
	if types["hello.txt"] != "file" || types["sub"] != "directory" {
		t.Fatalf("list of the root names %v, want hello.txt as file and sub as directory", types)
	}

	stat := e.ok(t, "stat", map[string]any{"path": "hello.txt"})
	if stat["type"] != "file" || stat["size"] != float64(len("hello acme\n")) {
		t.Fatalf("stat of hello.txt is %v, want a file of %d bytes", stat, len("hello acme\n"))
	}

	read := e.ok(t, "read", map[string]any{"path": "sub/nested.txt"})
	if read["content"] != "nested\n" {
		t.Fatalf("read of sub/nested.txt returned %v", read)
	}

	search := e.ok(t, "search", map[string]any{"query": "acme"})
	matches, _ := search["matches"].([]any)
	found := false
	for _, raw := range matches {
		m, _ := raw.(map[string]any)
		found = found || (m["path"] == "hello.txt" && m["line"] == float64(1))
	}
	if !found {
		t.Fatalf("search for acme returned %v, want a match in hello.txt line 1", search)
	}

	gitOut := e.ok(t, "git", map[string]any{"cwd": "", "args": []string{"status", "--porcelain"}})
	raw, err := base64.StdEncoding.DecodeString(gitOut["stdout_b64"].(string))
	if err != nil || gitOut["exit_code"] != float64(0) || !bytes.Contains(raw, []byte("hello.txt")) {
		t.Fatalf("git status returned %v (decode error %v), want exit 0 naming hello.txt", gitOut, err)
	}

	if rows := e.i.Audit(harness.AuditQuery{Event: "file_op"}); len(rows) != 0 {
		t.Fatalf("read operations wrote %d file_op rows, want none", len(rows))
	}
}

func TestFileChangeOps(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)

	e.ok(t, "write", map[string]any{"path": "a.txt", "content": "one\n"})
	if got := e.read(t, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt reads %q after write, want one", got)
	}
	e.ok(t, "mkdir", map[string]any{"parent": "", "name": "docs"})
	e.ok(t, "mkdir", map[string]any{"parent": "", "name": "archive"})
	moved := e.ok(t, "rename", map[string]any{"path": "a.txt", "new_name": "b.txt"})
	if moved["path"] != "b.txt" {
		t.Fatalf("rename answered path %v, want b.txt", moved["path"])
	}
	if got := e.read(t, "b.txt"); got != "one\n" {
		t.Fatalf("b.txt reads %q after rename, want one", got)
	}
	e.ok(t, "move", map[string]any{"path": "b.txt", "dest_dir": "archive"})
	if got := e.read(t, "archive/b.txt"); got != "one\n" {
		t.Fatalf("archive/b.txt reads %q after move, want one", got)
	}
	if resp, _ := e.post(t, "stat", map[string]any{"path": "b.txt"}); resp.Status != http.StatusNotFound {
		t.Fatalf("stat of the moved-away b.txt answered %d, want 404", resp.Status)
	}
	e.ok(t, "delete", map[string]any{"path": "archive/b.txt"})
	if resp, _ := e.post(t, "stat", map[string]any{"path": "archive/b.txt"}); resp.Status != http.StatusNotFound {
		t.Fatalf("stat of the deleted file answered %d, want 404", resp.Status)
	}

	for _, tool := range []string{"write", "mkdir", "rename", "move", "delete"} {
		if rows := e.fileOps(tool, "ok"); len(rows) == 0 {
			t.Fatalf("no file_op ok row for %s", tool)
		}
	}
}

func TestFileContainment(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	outside := filepath.Join(e.i.Home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("creating %s: %v", outside, err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("planting the secret: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(e.dir, "dirlink")); err != nil {
		t.Fatalf("planting the folder link: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(e.dir, "filelink")); err != nil {
		t.Fatalf("planting the file link: %v", err)
	}

	e.refused(t, "read", map[string]any{"path": "../outside/secret.txt"}, "TRAVERSAL", "not_granted")
	e.refused(t, "read", map[string]any{"path": "dirlink/secret.txt"}, "SYMLINK", "symlink")
	e.refused(t, "read", map[string]any{"path": "filelink"}, "SYMLINK", "symlink")

	e.refused(t, "write", map[string]any{"path": "../outside/escaped.txt", "content": "x"}, "TRAVERSAL", "not_granted")
	e.refused(t, "write", map[string]any{"path": "dirlink/planted.txt", "content": "x"}, "SYMLINK", "symlink")
	e.refused(t, "write", map[string]any{"path": "filelink", "content": "overwritten"}, "SYMLINK", "symlink")

	for _, name := range []string{"escaped.txt", "planted.txt"} {
		if _, err := os.Stat(filepath.Join(outside, name)); err == nil {
			t.Fatalf("a refused write created %s outside the project", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "secret\n" {
		t.Fatalf("the file behind the link reads %q after a refused write", b)
	}
	if rows := e.fileOps("write", "denied"); len(rows) != 3 {
		t.Fatalf("%d denied file_op write rows, want 3", len(rows))
	}
	if rows := e.i.Audit(harness.AuditQuery{Event: "file_op", Outcome: "ok"}); len(rows) != 0 {
		t.Fatalf("refused changes left %d ok file_op rows", len(rows))
	}
}

func TestFilesReadOnly(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	e.plant(t, "keep.txt", "keep\n")
	e.plant(t, "dir/inner.txt", "inner\n")

	r := e.i.CLI("project", "update", "--id", e.p.ID, "--files-read-only=true")
	if r.Code != 0 {
		t.Fatalf("project update --files-read-only=true exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	bodies := map[string]any{
		"write":  map[string]any{"path": "keep.txt", "content": "changed"},
		"mkdir":  map[string]any{"parent": "", "name": "made"},
		"rename": map[string]any{"path": "keep.txt", "new_name": "renamed.txt"},
		"move":   map[string]any{"path": "keep.txt", "dest_dir": "dir"},
		"delete": map[string]any{"path": "keep.txt"},
	}
	for op, body := range bodies {
		e.refused(t, op, body, "READ_ONLY", "read_only")
		if rows := e.fileOps(op, "denied"); len(rows) != 1 {
			t.Fatalf("%d denied file_op rows for %s, want 1", len(rows), op)
		}
	}
	if got := e.read(t, "keep.txt"); got != "keep\n" {
		t.Fatalf("keep.txt reads %q after refused changes, want keep", got)
	}
	e.ok(t, "list", map[string]any{"path": ""})

	r = e.i.CLI("project", "update", "--id", e.p.ID, "--files-read-only=false")
	if r.Code != 0 {
		t.Fatalf("project update --files-read-only=false exited %d", r.Code)
	}
	e.ok(t, "write", map[string]any{"path": "keep.txt", "content": "changed\n"})
	if got := e.read(t, "keep.txt"); got != "changed\n" {
		t.Fatalf("keep.txt reads %q after the project was made writable again", got)
	}
}

func TestFileStream(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	data := make([]byte, 300*1024)
	for n := range data {
		data[n] = byte(n % 251)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "blob.bin"), data, 0o600); err != nil {
		t.Fatalf("planting blob.bin: %v", err)
	}

	resp := e.c.Do("GET", e.route("stream")+"?path=blob.bin", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET files/stream answered %d, want 200", resp.Status)
	}
	if !bytes.Equal(resp.Body, data) {
		t.Fatalf("the stream returned %d bytes that differ from the file's %d", len(resp.Body), len(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("stream Content-Type is %q, want application/octet-stream", ct)
	}
	e.i.WaitEvent(harness.EventQuery{Key: "file.stream", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "project_id": e.p.ID}}, g13Deadline)

	missing := e.c.Do("GET", e.route("stream")+"?path=absent.bin", nil)
	var errBody struct {
		Code string `json:"code"`
	}
	missing.JSON(t, &errBody)
	if missing.Status != http.StatusNotFound || errBody.Code != "ENOENT" {
		t.Fatalf("stream of a missing file answered %d code %q, want 404 ENOENT", missing.Status, errBody.Code)
	}
	failed := e.i.WaitEvent(harness.EventQuery{Key: "file.stream", Trace: missing.Trace, Fields: map[string]any{"project_id": e.p.ID}}, g13Deadline)
	if failed.Str("status") == "ok" {
		t.Fatalf("a stream that failed recorded status ok")
	}
}

// g13Frame is one frame of /ws/files or of relay files watch --json.
type g13Frame struct {
	Type      string `json:"type"`
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
}

func TestWebSocketFilesWatchFsEvent(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	ws := e.i.WebSocket("/ws/files", e.i.Credential("runner"))
	ws.Send(map[string]any{"type": "watch", "project_id": e.p.ID})
	var first g13Frame
	if err := json.Unmarshal(ws.Next(30*time.Second), &first); err != nil || first.Type != "watch_ok" || first.ProjectID != e.p.ID {
		t.Fatalf("first frame is %+v (decode error %v), want watch_ok for %s", first, err, e.p.ID)
	}

	e.ok(t, "write", map[string]any{"path": "changed.txt", "content": "x"})
	for n := 0; n < 50; n++ {
		var f g13Frame
		if err := json.Unmarshal(ws.Next(30*time.Second), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "fs_event" && f.ProjectID == e.p.ID && f.Path == "changed.txt" {
			if f.Kind != "change" && f.Kind != "rename" {
				t.Fatalf("fs_event kind %q, want change or rename", f.Kind)
			}
			ws.Close()
			return
		}
	}
	t.Fatalf("no fs_event for changed.txt in 50 frames")
}

func TestFilesWatch(t *testing.T) {
	t.Parallel()
	e := g13Setup(t)
	watch := e.i.StartCLI(harness.CLIOpts{Deadline: 2 * g13Deadline}, "files", "watch", "--project", e.p.ID, "--json", "--until", "fs_event", "--timeout", "60s")

	var ok g13Frame
	if err := json.Unmarshal(watch.FirstLine(g13Deadline), &ok); err != nil || ok.Type != "watch_ok" || ok.ProjectID != e.p.ID {
		t.Fatalf("first watch frame is %+v (decode error %v), want watch_ok for %s", ok, err, e.p.ID)
	}
	e.ok(t, "write", map[string]any{"path": "watched.txt", "content": "x"})

	res := watch.Wait()
	if res.Code != 0 {
		t.Fatalf("files watch --until fs_event exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var seen *g13Frame
	for _, line := range bytes.Split(res.Stdout, []byte("\n")) {
		var f g13Frame
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.Type == "fs_event" {
			seen = &f
		}
	}
	if seen == nil || seen.ProjectID != e.p.ID || seen.Path != "watched.txt" {
		t.Fatalf("files watch printed no fs_event frame for the project: %q", res.Stdout)
	}
	e.i.WaitEvent(harness.EventQuery{Key: "files.watch", Fields: map[string]any{"status": "ok", "project_id": e.p.ID}}, g13Deadline)
}
