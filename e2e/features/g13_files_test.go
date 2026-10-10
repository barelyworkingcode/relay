package features

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

const g13Deadline = 30 * time.Second

var g13Runner = []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}}

// g13Env is one instance with one granted project folder and an execute
// credential on the frontend socket, where the file routes live.
type g13Env struct {
	i      *harness.Instance
	c      *harness.Client
	id     string
	dir    string
	outDir string
}

func g13Start(t *testing.T) g13Env {
	t.Helper()
	i := harness.Start(t, harness.Options{Credentials: g13Runner, Presence: approveGrant})
	p, _ := createProject(t, i, "acme-files")
	out := filepath.Join(i.Home, "outside")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatalf("creating %s: %v", out, err)
	}
	return g13Env{i: i, c: i.SocketHTTP(i.Credential("runner")), id: p.ID, dir: filepath.Join(i.Home, "work", "acme-files"), outDir: out}
}

func (e g13Env) post(t *testing.T, op string, body any) harness.Response {
	t.Helper()
	return e.c.Do("POST", "/api/projects/"+e.id+"/files/"+op, body)
}

// ok requires status 200 and the file.<op> ok event for the response's trace.
func (e g13Env) ok(t *testing.T, op string, body any) harness.Response {
	t.Helper()
	r := e.post(t, op, body)
	if r.Status != 200 {
		t.Fatalf("files/%s answered %d, want 200", op, r.Status)
	}
	e.wantEvent(t, "file."+op, r.Trace, "ok", "")
	return r
}

func (e g13Env) wantEvent(t *testing.T, key, trace, status, reason string) {
	t.Helper()
	fields := map[string]any{"status": status, "project_id": e.id}
	if reason != "" {
		fields["reason"] = reason
	}
	e.i.WaitEvent(harness.EventQuery{Key: key, Trace: trace, Fields: fields}, g13Deadline)
}

// refused requires status 403 with the file error code in the body.
func (e g13Env) refused(t *testing.T, op string, body any, code string) harness.Response {
	t.Helper()
	r := e.post(t, op, body)
	var b struct {
		Code string `json:"code"`
	}
	r.JSON(t, &b)
	if r.Status != 403 || b.Code != code {
		t.Fatalf("files/%s answered %d code %q, want 403 %s", op, r.Status, b.Code, code)
	}
	return r
}

func (e g13Env) fileOps(outcome string) []map[string]any {
	return e.i.Audit(harness.AuditQuery{Event: "file_op", Outcome: outcome})
}

// opRows returns the file_op rows of one outcome for one tool.
func (e g13Env) opRows(outcome, tool string) int {
	n := 0
	for _, row := range e.fileOps(outcome) {
		if row["tool"] == tool {
			n++
		}
	}
	return n
}

func g13Plant(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the folder of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func g13Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestFileReadOps(t *testing.T) {
	t.Parallel()
	e := g13Start(t)
	g13Plant(t, filepath.Join(e.dir, "notes.txt"), "alpha\nacme-needle here\n")
	g13Plant(t, filepath.Join(e.dir, "docs", "readme.md"), "# Acme\n")
	if out, err := exec.Command("git", "-C", e.dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	auditBefore := len(e.i.Audit(harness.AuditQuery{Event: "file_op"}))

	var list struct {
		Entries []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Size  int64  `json:"size"`
			MTime int64  `json:"mtime_ms"`
		} `json:"entries"`
	}
	e.ok(t, "list", map[string]any{"path": "", "show_hidden": false}).JSON(t, &list)
	types := map[string]string{}
	for _, en := range list.Entries {
		types[en.Name] = en.Type
	}
	if types["notes.txt"] != "file" || types["docs"] != "directory" {
		t.Fatalf("list names %v, want notes.txt as file and docs as directory", types)
	}

	var stat struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	e.ok(t, "stat", map[string]any{"path": "notes.txt"}).JSON(t, &stat)
	if stat.Type != "file" || stat.Size != int64(len("alpha\nacme-needle here\n")) {
		t.Fatalf("stat is %+v, want a file of 23 bytes", stat)
	}

	var read struct {
		Content string `json:"content"`
		Size    int64  `json:"size"`
	}
	e.ok(t, "read", map[string]any{"path": "notes.txt"}).JSON(t, &read)
	if read.Content != "alpha\nacme-needle here\n" || read.Size != 23 {
		t.Fatalf("read is %+v, want the planted text and size 23", read)
	}

	var search struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Col  int    `json:"col"`
			Len  int    `json:"len"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	e.ok(t, "search", map[string]any{"query": "acme-needle", "max_matches": 10}).JSON(t, &search)
	if len(search.Matches) != 1 || search.Matches[0].Path != "notes.txt" || search.Matches[0].Line != 2 || search.Matches[0].Col != 1 || search.Matches[0].Len != len("acme-needle") {
		t.Fatalf("search matched %+v, want one match at notes.txt line 2 col 1", search.Matches)
	}

	var git struct {
		ExitCode  int    `json:"exit_code"`
		StdoutB64 string `json:"stdout_b64"`
	}
	e.ok(t, "git", map[string]any{"cwd": "", "args": []string{"status", "--porcelain"}, "max_bytes": 65536}).JSON(t, &git)
	stdout, err := base64.StdEncoding.DecodeString(git.StdoutB64)
	if err != nil {
		t.Fatalf("git stdout_b64 does not decode: %v", err)
	}
	if git.ExitCode != 0 || !bytes.Contains(stdout, []byte("notes.txt")) {
		t.Fatalf("git status exit %d stdout %q, want exit 0 naming notes.txt", git.ExitCode, stdout)
	}

	if got := len(e.i.Audit(harness.AuditQuery{Event: "file_op"})); got != auditBefore {
		t.Fatalf("read operations wrote %d file_op rows, want none", got-auditBefore)
	}
}

func TestFileChangeOps(t *testing.T) {
	t.Parallel()
	e := g13Start(t)
	path := func(r harness.Response) string {
		var b struct {
			Path string `json:"path"`
		}
		r.JSON(t, &b)
		return b.Path
	}
	row := func(op string) {
		t.Helper()
		if n := e.opRows("ok", op); n != 1 {
			t.Fatalf("%d file_op ok rows for %s, want 1", n, op)
		}
	}

	if got := path(e.ok(t, "write", map[string]any{"path": "w.txt", "content": "hello"})); got != "w.txt" {
		t.Fatalf("write answered path %q, want w.txt", got)
	}
	if b, err := os.ReadFile(filepath.Join(e.dir, "w.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("w.txt on disk is %q (%v), want hello", b, err)
	}
	row("write")

	if got := path(e.ok(t, "mkdir", map[string]any{"parent": "", "name": "sub"})); got != "sub" {
		t.Fatalf("mkdir answered path %q, want sub", got)
	}
	if fi, err := os.Stat(filepath.Join(e.dir, "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("sub is not a folder on disk (%v)", err)
	}
	row("mkdir")

	if got := path(e.ok(t, "rename", map[string]any{"path": "w.txt", "new_name": "r.txt"})); got != "r.txt" {
		t.Fatalf("rename answered path %q, want r.txt", got)
	}
	if g13Exists(filepath.Join(e.dir, "w.txt")) || !g13Exists(filepath.Join(e.dir, "r.txt")) {
		t.Fatalf("after rename w.txt exists or r.txt is missing")
	}
	row("rename")

	if got := path(e.ok(t, "move", map[string]any{"path": "r.txt", "dest_dir": "sub"})); got != "sub/r.txt" {
		t.Fatalf("move answered path %q, want sub/r.txt", got)
	}
	if g13Exists(filepath.Join(e.dir, "r.txt")) || !g13Exists(filepath.Join(e.dir, "sub", "r.txt")) {
		t.Fatalf("after move r.txt is still in the root or missing from sub")
	}
	row("move")

	var del struct {
		Trashed bool `json:"trashed"`
	}
	e.ok(t, "delete", map[string]any{"path": "sub/r.txt"}).JSON(t, &del)
	if !del.Trashed {
		t.Fatalf("delete answered trashed false, want true on this Mac")
	}
	if g13Exists(filepath.Join(e.dir, "sub", "r.txt")) {
		t.Fatalf("sub/r.txt is still in the project after delete")
	}
	row("delete")

	if n := len(e.fileOps("denied")) + len(e.fileOps("error")); n != 0 {
		t.Fatalf("permitted changes left %d denied or error file_op rows", n)
	}
}

func TestFileContainment(t *testing.T) {
	t.Parallel()
	e := g13Start(t)
	secret := filepath.Join(e.outDir, "secret.txt")
	g13Plant(t, secret, "outside-original")
	if err := os.Symlink(secret, filepath.Join(e.dir, "link.txt")); err != nil {
		t.Fatalf("planting the symlink: %v", err)
	}

	r := e.refused(t, "read", map[string]any{"path": "../outside/secret.txt"}, "TRAVERSAL")
	e.wantEvent(t, "file.read", r.Trace, "denied", "not_granted")

	r = e.refused(t, "read", map[string]any{"path": "link.txt"}, "SYMLINK")
	e.wantEvent(t, "file.read", r.Trace, "denied", "symlink")

	r = e.refused(t, "write", map[string]any{"path": "link.txt", "content": "overwritten"}, "SYMLINK")
	e.wantEvent(t, "file.write", r.Trace, "denied", "symlink")
	if n := e.opRows("denied", "write"); n != 1 {
		t.Fatalf("%d denied file_op rows for write, want 1", n)
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "outside-original" {
		t.Fatalf("the file outside the project is %q (%v) after a refused write, want it unchanged", b, err)
	}
}

func TestFilesReadOnly(t *testing.T) {
	t.Parallel()
	e := g13Start(t)
	g13Plant(t, filepath.Join(e.dir, "keep.txt"), "kept")

	e.i.MustCLI("project", "update", "--id", e.id, "--files-read-only=true")
	r := e.refused(t, "write", map[string]any{"path": "new.txt", "content": "x"}, "READ_ONLY")
	e.wantEvent(t, "file.write", r.Trace, "denied", "read_only")
	if n := e.opRows("denied", "write"); n != 1 {
		t.Fatalf("%d denied file_op rows for write, want 1", n)
	}
	if g13Exists(filepath.Join(e.dir, "new.txt")) {
		t.Fatalf("new.txt exists after a refused write")
	}
	e.ok(t, "read", map[string]any{"path": "keep.txt"})

	e.i.MustCLI("project", "update", "--id", e.id, "--files-read-only=false")
	e.ok(t, "write", map[string]any{"path": "new.txt", "content": "x"})
	if !g13Exists(filepath.Join(e.dir, "new.txt")) {
		t.Fatalf("new.txt is missing after a write on a project that is writable again")
	}
}

func TestFileStream(t *testing.T) {
	t.Parallel()
	e := g13Start(t)
	want := make([]byte, 0, 4096)
	for n := 0; n < 4096; n++ {
		want = append(want, byte(n%256))
	}
	if err := os.WriteFile(filepath.Join(e.dir, "blob.bin"), want, 0o600); err != nil {
		t.Fatalf("planting blob.bin: %v", err)
	}
	r := e.c.Do("GET", "/api/projects/"+e.id+"/files/stream?path=blob.bin", nil)
	if r.Status != 200 {
		t.Fatalf("stream answered %d, want 200", r.Status)
	}
	if !bytes.Equal(r.Body, want) {
		t.Fatalf("stream returned %d bytes that differ from the %d planted", len(r.Body), len(want))
	}
	e.wantEvent(t, "file.stream", r.Trace, "ok", "")
}

func TestFilesWatch(t *testing.T) {
	t.Parallel()
	frame := func(raw json.RawMessage) (typ, projectID, path string) {
		t.Helper()
		var f struct {
			Type      string `json:"type"`
			ProjectID string `json:"project_id"`
			Path      string `json:"path"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		return f.Type, f.ProjectID, f.Path
	}

	t.Run("cli", func(t *testing.T) {
		t.Parallel()
		e := g13Start(t)
		p := e.i.StartCLI(harness.CLIOpts{}, "files", "watch", "--project", e.id, "--until", "fs_event", "--json")
		if typ, pid, _ := frame(p.FirstLine(g13Deadline)); typ != "watch_ok" || pid != e.id {
			t.Fatalf("first frame is %s for %q, want watch_ok for %s", typ, pid, e.id)
		}
		e.ok(t, "write", map[string]any{"path": "watched.txt", "content": "x"})
		res := p.Wait()
		if res.Code != 0 {
			t.Fatalf("files watch exited %d, want 0\nstderr: %s", res.Code, res.Stderr)
		}
		lines := bytes.Split(bytes.TrimSpace(res.Stdout), []byte("\n"))
		if typ, _, path := frame(lines[len(lines)-1]); typ != "fs_event" || path != "watched.txt" {
			t.Fatalf("last frame is %s for %q, want fs_event for watched.txt", typ, path)
		}
		e.i.WaitEvent(harness.EventQuery{Key: "files.watch", Fields: map[string]any{"status": "ok", "project_id": e.id}}, g13Deadline)
	})

	t.Run("websocket", func(t *testing.T) {
		t.Parallel()
		e := g13Start(t)
		ws := e.i.WebSocket("/ws/files", e.i.Credential("runner"))
		ws.Send(map[string]any{"type": "watch", "project_id": e.id})
		for {
			typ, pid, _ := frame(ws.Next(g13Deadline))
			if typ == "host_status" {
				continue
			}
			if typ != "watch_ok" || pid != e.id {
				t.Fatalf("first frame is %s for %q, want watch_ok for %s", typ, pid, e.id)
			}
			break
		}
		e.ok(t, "write", map[string]any{"path": "watched.txt", "content": "x"})
		for {
			typ, pid, path := frame(ws.Next(g13Deadline))
			if typ != "fs_event" {
				continue
			}
			if pid != e.id || path != "watched.txt" {
				t.Fatalf("fs_event is for %q path %q, want %s and watched.txt", pid, path, e.id)
			}
			break
		}
		ws.Close()
		e.i.WaitEvent(harness.EventQuery{Key: "file.ws.close", Fields: map[string]any{"status": "ok"}}, g13Deadline)
	})
}
