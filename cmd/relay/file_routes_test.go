package main

// File plane routes through the real FrontendServer: the frontend socket and
// the loopback listener, with real temp directories behind console projects.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

const fileTestToken = "file-plane-bearer"

type fileEnvOpts struct {
	// rec is the audit recorder; nil with auditOff false builds an enabled one.
	rec      *audit.AuditRecorder
	auditOff bool
	gate     *presence.Gate
	hosts    FileHostPool
	newLocal func(root string) (projectfs.Backend, error)
	recheck  time.Duration
}

type fileEnv struct {
	store   config.SettingsStore
	srv     *FrontendServer
	sock    *http.Client
	tcpBase string
	root    string
	proj    config.Project
	rec     *audit.AuditRecorder
}

// newFileEnv wires a real FrontendServer over a Unix socket and a loopback
// listener, with one console project whose path is a fresh temp directory.
func newFileEnv(t *testing.T, o fileEnvOpts) *fileEnv {
	t.Helper()
	store := newCLISandboxStore(t)
	rec := o.rec
	if rec == nil && !o.auditOff {
		rec = aiRecorderAt(t, filepath.Join(t.TempDir(), "file-audit.jsonl"), nil)
	}
	gate := o.gate
	if gate == nil {
		gate = allowGate(t)
	}
	root := t.TempDir()
	proj := mkStoreProject(t, store, config.ProjectKindLocal, "Acme", root)

	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}}
	enrolOps := &EnrolmentOps{Store: store}
	mcpOps := &McpOps{Store: store, Ctx: context.Background()}
	projOps := &ProjectOps{Store: store, Gate: gate, Issuance: enabledIssuanceRecorder(t)}
	extMgr := mcpbroker.NewManager(nil)
	sockDir := mkShortTempDir(t, "file-fe-")
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, filepath.Join(sockDir, "frontend.sock"), fileTestToken),
		NewEnhancedServiceRegistry(nil), nil, nil, ops, enrolOps, &audit.AuditOps{Audit: rec}, mcpOps, projOps,
		nil, nil, nil, nil, nil, nil, nil, sessionRouteDeps{},
	)
	assertNoErr(t, err, "NewFrontendServer")
	fo := srv.FileOps()
	fo.Hosts, fo.NewLocal, fo.WatchRecheck = o.hosts, o.newLocal, o.recheck
	go func() { _ = srv.Serve() }()
	assertNoErr(t, srv.ListenLoopback("127.0.0.1:0"), "ListenLoopback")
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, srv.socketPath, 2*time.Second).Close()
	return &fileEnv{
		store: store, srv: srv, sock: dialFrontendHTTP(srv.socketPath),
		tcpBase: "http://" + srv.tcpLn.Addr().String(), root: root, proj: proj, rec: rec,
	}
}

func (e *fileEnv) path(rel string) string { return filepath.Join(e.root, filepath.FromSlash(rel)) }

func (e *fileEnv) put(t *testing.T, rel, content string) {
	t.Helper()
	assertNoErr(t, os.MkdirAll(filepath.Dir(e.path(rel)), 0o755), "mkdir")
	assertNoErr(t, os.WriteFile(e.path(rel), []byte(content), 0o644), "write fixture")
}

func (e *fileEnv) file(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(e.path(rel))
	assertNoErr(t, err, "read %s", rel)
	return string(b)
}

func (e *fileEnv) has(rel string) bool {
	_, err := os.Lstat(e.path(rel))
	return err == nil
}

// links lays out real/inner.txt, link -> real and flink -> plain.txt.
func (e *fileEnv) links(t *testing.T) {
	t.Helper()
	e.put(t, "real/inner.txt", "inner")
	e.put(t, "plain.txt", "plain")
	assertNoErr(t, os.Symlink("real", e.path("link")), "symlink")
	assertNoErr(t, os.Symlink("plain.txt", e.path("flink")), "symlink")
}

func (e *fileEnv) setProject(t *testing.T, mutate func(*config.Project)) {
	t.Helper()
	assertNoErr(t, e.store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, e.proj.ID)
		mutate(p)
	}), "update project")
}

type fileResp struct {
	status int
	header http.Header
	raw    []byte
}

func (r fileResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.raw, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, r.raw)
	}
	return m
}

// code is the error code of a refusal, failing if the body is not the
// contract's {"error","code"} shape.
func (r fileResp) code(t *testing.T) string {
	t.Helper()
	m := r.json(t)
	msg, _ := m["error"].(string)
	code, _ := m["code"].(string)
	if msg == "" || code == "" {
		t.Fatalf("error body %s lacks error or code", r.raw)
	}
	return code
}

func (e *fileEnv) request(t *testing.T, method, path string, body any, hdr map[string]string) fileResp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		assertNoErr(t, err, "marshal")
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://unix"+path, rd)
	assertNoErr(t, err, "new request")
	req.Header.Set("Authorization", "Bearer "+fileTestToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.sock.Do(req)
	assertNoErr(t, err, "%s %s", method, path)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	assertNoErr(t, err, "read body")
	return fileResp{status: resp.StatusCode, header: resp.Header, raw: raw}
}

func (e *fileEnv) op(t *testing.T, op string, body any) fileResp {
	t.Helper()
	return e.request(t, "POST", "/api/projects/"+e.proj.ID+"/files/"+op, body, nil)
}

func (e *fileEnv) stream(t *testing.T, rel string, hdr map[string]string) fileResp {
	t.Helper()
	return e.request(t, "GET", "/api/projects/"+e.proj.ID+"/files/stream?path="+url.QueryEscape(rel), nil, hdr)
}

// cleanTrash removes a trashed test file by its unique name from the Trash
// directories a Trash move can land in, and only if it still holds content.
func cleanTrash(t *testing.T, name, content string) {
	t.Helper()
	var homes []string
	if u, err := user.Current(); err == nil {
		homes = append(homes, u.HomeDir)
	}
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, h)
	}
	for _, h := range homes {
		p := filepath.Join(h, ".Trash", name)
		if b, err := os.ReadFile(p); err == nil && string(b) == content {
			_ = os.Remove(p)
		}
	}
}

// fileRoutes is every route of the file plane, with the project id filled in
// by the caller.
var fileRoutes = []struct{ method, path string }{
	{"POST", "/api/projects/P/files/list"},
	{"POST", "/api/projects/P/files/stat"},
	{"POST", "/api/projects/P/files/read"},
	{"GET", "/api/projects/P/files/stream"},
	{"POST", "/api/projects/P/files/write"},
	{"POST", "/api/projects/P/files/mkdir"},
	{"POST", "/api/projects/P/files/rename"},
	{"POST", "/api/projects/P/files/move"},
	{"POST", "/api/projects/P/files/delete"},
	{"POST", "/api/projects/P/files/search"},
	{"POST", "/api/projects/P/files/git"},
	{"POST", "/api/hosts/H/pastetmp"},
	{"GET", "/ws/files"},
}

type classRecorder struct {
	mu   sync.Mutex
	seen map[string]control.CapabilityClass
}

func (c *classRecorder) Authorize(r *http.Request, class control.CapabilityClass) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[r.Method+" "+r.URL.Path] = class
	return errors.New("recorded, not served")
}

func TestFileRoutes_AreExecuteClass(t *testing.T) {
	store := newCLISandboxStore(t)
	rec := &classRecorder{seen: map[string]control.CapabilityClass{}}
	mux := http.NewServeMux()
	RegisterFileRoutes(&control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket, Authz: rec}, &FileOps{Store: store})

	for _, rt := range fileRoutes {
		path := strings.NewReplacer("/P/", "/p1/", "/H/", "/h1/").Replace(rt.path)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(rt.method, path, nil))
		if got, ok := rec.seen[rt.method+" "+path]; !ok {
			t.Errorf("%s %s is not registered (status %d)", rt.method, rt.path, w.Code)
		} else if got != control.ClassExecute {
			t.Errorf("%s %s registered as %q, want execute", rt.method, rt.path, got)
		}
	}
}

func TestFileRoutes_AbsentFromLoopbackTCP(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	for _, rt := range fileRoutes {
		path := strings.NewReplacer("/P/", "/"+env.proj.ID+"/", "/H/", "/h1/").Replace(rt.path)
		onSocket := env.request(t, rt.method, path, map[string]any{}, nil)
		if strings.HasPrefix(string(onSocket.raw), "404 page not found") {
			t.Errorf("%s %s is not served on the socket", rt.method, rt.path)
		}
		resp, body := teDo(t, http.DefaultClient, rt.method, env.tcpBase+path, fileTestToken, map[string]any{"path": "a.txt"})
		teAssertMuxRefused(t, resp, body)
	}
}

func TestFileRoutes_ServeEveryOpForAConsoleProject(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "src/a.js", "line one\n// TODO ship\n")
	env.put(t, "0to9.txt", "0123456789")

	r := env.op(t, "list", map[string]any{"path": "src"})
	entries, _ := r.json(t)["entries"].([]any)
	if r.status != 200 || len(entries) != 1 {
		t.Fatalf("list = %d %s", r.status, r.raw)
	}
	if e := entries[0].(map[string]any); e["name"] != "a.js" || e["type"] != "file" || e["size"] != float64(22) || e["mtime_ms"].(float64) <= 0 {
		t.Errorf("list entry = %v", e)
	}

	r = env.op(t, "stat", map[string]any{"path": "src/a.js"})
	if m := r.json(t); r.status != 200 || m["type"] != "file" || m["size"] != float64(22) {
		t.Errorf("stat = %d %s", r.status, r.raw)
	}
	r = env.op(t, "read", map[string]any{"path": "src/a.js"})
	if m := r.json(t); r.status != 200 || m["content"] != "line one\n// TODO ship\n" || m["size"] != float64(22) {
		t.Errorf("read = %d %s", r.status, r.raw)
	}

	r = env.stream(t, "0to9.txt", nil)
	if r.status != 200 || string(r.raw) != "0123456789" || r.header.Get("Content-Type") != "application/octet-stream" ||
		r.header.Get("Accept-Ranges") != "bytes" || r.header.Get("Content-Length") != "10" {
		t.Errorf("stream = %d %q %v", r.status, r.raw, r.header)
	}
	r = env.stream(t, "0to9.txt", map[string]string{"Range": "bytes=2-4"})
	if r.status != 206 || string(r.raw) != "234" || r.header.Get("Content-Range") != "bytes 2-4/10" {
		t.Errorf("ranged stream = %d %q %v", r.status, r.raw, r.header)
	}

	r = env.op(t, "write", map[string]any{"path": "notes/n.md", "content": "hi"})
	if r.status != http.StatusNotFound || r.code(t) != projectfs.CodeENOENT {
		t.Errorf("write into a missing directory = %d %s", r.status, r.raw)
	}
	r = env.op(t, "mkdir", map[string]any{"parent": "", "name": "notes"})
	if m := r.json(t); r.status != 200 || m["path"] != "notes" {
		t.Errorf("mkdir = %d %s", r.status, r.raw)
	}
	r = env.op(t, "write", map[string]any{"path": "notes/n.md", "content": "hi"})
	if m := r.json(t); r.status != 200 || m["path"] != "notes/n.md" || env.file(t, "notes/n.md") != "hi" {
		t.Errorf("write = %d %s", r.status, r.raw)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0x80})
	r = env.op(t, "write", map[string]any{"path": "bin.dat", "content": b64, "encoding": "base64"})
	if r.status != 200 || env.file(t, "bin.dat") != "\xff\x00\x80" {
		t.Errorf("base64 write = %d %s", r.status, r.raw)
	}
	r = env.op(t, "rename", map[string]any{"path": "notes/n.md", "new_name": "m.md"})
	if m := r.json(t); r.status != 200 || m["path"] != "notes/m.md" || !env.has("notes/m.md") || env.has("notes/n.md") {
		t.Errorf("rename = %d %s", r.status, r.raw)
	}
	r = env.op(t, "move", map[string]any{"path": "notes/m.md", "dest_dir": "src"})
	if m := r.json(t); r.status != 200 || m["path"] != "src/m.md" || !env.has("src/m.md") {
		t.Errorf("move = %d %s", r.status, r.raw)
	}

	r = env.op(t, "search", map[string]any{"query": "TODO", "globs": []string{"*.js"}})
	m := r.json(t)
	matches, _ := m["matches"].([]any)
	if r.status != 200 || len(matches) != 1 || m["truncated"] != false {
		t.Fatalf("search = %d %s", r.status, r.raw)
	}
	if hit := matches[0].(map[string]any); hit["path"] != "src/a.js" || hit["line"] != float64(2) || hit["col"] != float64(4) ||
		hit["len"] != float64(4) || hit["text"] != "// TODO ship" {
		t.Errorf("search match = %v", hit)
	}

	// A non-zero git exit is still a 200 with the exit code.
	r = env.op(t, "git", map[string]any{"cwd": "", "args": []string{"rev-parse", "--show-toplevel"}})
	m = r.json(t)
	if r.status != 200 || m["exit_code"] == float64(0) || !strings.Contains(m["stderr"].(string), "not a git repository") {
		t.Errorf("git outside a repo = %d %s", r.status, r.raw)
	}
	if _, ok := m["stdout_b64"]; !ok {
		t.Errorf("git response lacks stdout_b64: %s", r.raw)
	}

	trashName := "relay-route-" + filepath.Base(env.root) + ".txt"
	env.put(t, trashName, "trash-me")
	r = env.op(t, "delete", map[string]any{"path": trashName})
	cleanTrash(t, trashName, "trash-me")
	if r.status == http.StatusForbidden && r.code(t) == projectfs.CodeEACCES {
		t.Fatalf("Trash is not writable here: %s", r.raw)
	}
	if m := r.json(t); r.status != 200 || m["trashed"] != true || env.has(trashName) {
		t.Errorf("delete = %d %s", r.status, r.raw)
	}
}

func TestFileRoutes_ErrorBodiesAndCodes(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.links(t)
	env.put(t, "ten.txt", "0123456789")
	env.put(t, "dir/x.txt", "x")
	env.put(t, "dir/plain.txt", "taken")

	for _, tc := range []struct {
		name   string
		op     string
		body   any
		status int
		code   string
	}{
		{"missing file", "read", map[string]any{"path": "nope.txt"}, 404, projectfs.CodeENOENT},
		{"read a directory", "read", map[string]any{"path": "dir"}, 400, projectfs.CodeEISDIR},
		{"list a file", "list", map[string]any{"path": "ten.txt"}, 400, projectfs.CodeENOTDIR},
		{"over the read cap", "read", map[string]any{"path": "ten.txt", "max_bytes": 3}, 413, projectfs.CodeTooLarge},
		{"create_only on an existing file", "write", map[string]any{"path": "ten.txt", "content": "x", "create_only": true}, 409, projectfs.CodeEEXIST},
		{"mkdir over an existing directory", "mkdir", map[string]any{"parent": "", "name": "dir"}, 409, projectfs.CodeEEXIST},
		{"rename onto an existing console entry", "rename", map[string]any{"path": "plain.txt", "new_name": "ten.txt"}, 409, projectfs.CodeEEXIST},
		{"move onto an existing console entry", "move", map[string]any{"path": "plain.txt", "dest_dir": "dir"}, 409, projectfs.CodeEEXIST},
		{"bad JSON", "stat", []byte("{not json"), 400, projectfs.CodeInvalid},
		{"name with a slash", "mkdir", map[string]any{"parent": "", "name": "a/b"}, 400, projectfs.CodeInvalid},
		{"empty name", "mkdir", map[string]any{"parent": "", "name": ""}, 400, projectfs.CodeInvalid},
		{"new_name dot-dot", "rename", map[string]any{"path": "plain.txt", "new_name": ".."}, 400, projectfs.CodeInvalid},
		{"NUL byte in a path", "stat", map[string]any{"path": "a\u0000b"}, 400, projectfs.CodeInvalid},
		{"unknown encoding", "write", map[string]any{"path": "e.txt", "content": "x", "encoding": "rot13"}, 400, projectfs.CodeInvalid},
		{"content that is not base64", "write", map[string]any{"path": "e.txt", "content": "***", "encoding": "base64"}, 400, projectfs.CodeInvalid},
		{"empty search query", "search", map[string]any{"query": ""}, 400, projectfs.CodeInvalid},
		{"bad regex", "search", map[string]any{"query": "(", "regex": true}, 400, projectfs.CodeInvalid},
		{"git subcommand outside the allowlist", "git", map[string]any{"args": []string{"commit", "-m", "x"}}, 400, projectfs.CodeInvalid},
		{"git argument with a refused prefix", "git", map[string]any{"args": []string{"diff", "--output=" + "/tmp/x"}}, 400, projectfs.CodeInvalid},
		{"git worktree add", "git", map[string]any{"args": []string{"worktree", "add", "x"}}, 400, projectfs.CodeInvalid},
	} {
		r := env.op(t, tc.op, tc.body)
		if r.status != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, r.status, tc.status, r.raw)
			continue
		}
		if got := r.code(t); got != tc.code {
			t.Errorf("%s: code %s, want %s", tc.name, got, tc.code)
		}
		if tc.code == projectfs.CodeTooLarge && r.json(t)["size"] != float64(10) {
			t.Errorf("%s: TOO_LARGE size = %v, want 10", tc.name, r.json(t)["size"])
		}
	}
}

func TestFileRoutes_WriteOverTheLimitIsTooLarge(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	r := env.op(t, "write", map[string]any{"path": "big.txt", "content": strings.Repeat("a", int(projectfs.MaxWriteBytes)+1)})
	if r.status != 413 || r.code(t) != projectfs.CodeTooLarge || r.json(t)["size"] == nil {
		t.Errorf("write over the cap = %d %.200s", r.status, r.raw)
	}
	if env.has("big.txt") {
		t.Error("an oversized write left a file behind")
	}
}

func TestFileRoutes_RefuseProjectsThatHaveNoFiles(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	remote := mkStoreProject(t, env.store, config.ProjectKindRemote, "Profile", "")
	pathless := mkStoreProject(t, env.store, config.ProjectKindLocal, "Pathless", t.TempDir())
	assertNoErr(t, env.store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, pathless.ID)
		p.Path = ""
	}), "clear path")

	// The project and kind checks come before the path is looked at.
	traversal := map[string]any{"path": "../x"}
	for _, tc := range []struct {
		name, id string
		status   int
		code     string
	}{
		{"unknown project", "no-such-project", 404, projectfs.CodeProjectNotFound},
		{"access-profile project", remote.ID, 403, projectfs.CodeNotAvailable},
		{"project with no path", pathless.ID, 403, projectfs.CodeNotAvailable},
	} {
		r := env.request(t, "POST", "/api/projects/"+tc.id+"/files/stat", traversal, nil)
		if r.status != tc.status || r.code(t) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.raw, tc.status, tc.code)
		}
		if tc.code == projectfs.CodeProjectNotFound && r.json(t)["error"] != "project not found" {
			t.Errorf("unknown project message = %v", r.json(t)["error"])
		}
	}
}

func TestFileRoutes_RefuseTraversalOnEveryOp(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "a.txt", "a")
	outside := filepath.Join(filepath.Dir(env.root), "escaped.txt")
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, tc := range []struct {
		op   string
		body any
	}{
		{"list", map[string]any{"path": "../"}},
		{"stat", map[string]any{"path": "a/../../x"}},
		{"read", map[string]any{"path": "../escaped.txt"}},
		{"write", map[string]any{"path": "../escaped.txt", "content": "pwn"}},
		{"mkdir", map[string]any{"parent": "..", "name": "x"}},
		{"rename", map[string]any{"path": "../escaped.txt", "new_name": "y"}},
		{"move", map[string]any{"path": "a.txt", "dest_dir": ".."}},
		{"delete", map[string]any{"path": ".."}},
		{"git", map[string]any{"cwd": "..", "args": []string{"status"}}},
	} {
		r := env.op(t, tc.op, tc.body)
		if r.status != 403 || r.code(t) != projectfs.CodeTraversal {
			t.Errorf("%s: %d %s, want 403 TRAVERSAL", tc.op, r.status, r.raw)
		}
	}
	if r := env.stream(t, "../escaped.txt", nil); r.status != 403 || r.code(t) != projectfs.CodeTraversal {
		t.Errorf("stream: %d %s, want 403 TRAVERSAL", r.status, r.raw)
	}
	if _, err := os.Lstat(outside); err == nil {
		t.Error("a write escaped the project root")
	}
	if !env.has("a.txt") {
		t.Error("a refused operation changed the tree")
	}
}

func TestFileRoutes_RefuseSymlinksExceptInList(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.links(t)

	r := env.op(t, "list", map[string]any{"path": ""})
	types := map[string]string{}
	for _, e := range r.json(t)["entries"].([]any) {
		m := e.(map[string]any)
		types[m["name"].(string)] = m["type"].(string)
	}
	if r.status != 200 || types["link"] != "symlink" || types["flink"] != "symlink" || types["real"] != "directory" {
		t.Errorf("list = %d %v", r.status, types)
	}

	for _, tc := range []struct {
		op   string
		body any
	}{
		{"stat", map[string]any{"path": "flink"}},
		{"read", map[string]any{"path": "flink"}},
		{"read", map[string]any{"path": "link/inner.txt"}},
		{"write", map[string]any{"path": "flink", "content": "pwn"}},
		{"write", map[string]any{"path": "link/new.txt", "content": "pwn"}},
		{"mkdir", map[string]any{"parent": "link", "name": "x"}},
		{"rename", map[string]any{"path": "flink", "new_name": "y"}},
		{"move", map[string]any{"path": "flink", "dest_dir": "real"}},
		{"move", map[string]any{"path": "plain.txt", "dest_dir": "link"}},
		{"delete", map[string]any{"path": "flink"}},
		{"git", map[string]any{"cwd": "link", "args": []string{"status"}}},
	} {
		r := env.op(t, tc.op, tc.body)
		if r.status != 403 || r.code(t) != projectfs.CodeSymlink {
			t.Errorf("%s %v: %d %s, want 403 SYMLINK", tc.op, tc.body, r.status, r.raw)
		}
	}
	if r := env.stream(t, "flink", nil); r.status != 403 || r.code(t) != projectfs.CodeSymlink {
		t.Errorf("stream: %d %s, want 403 SYMLINK", r.status, r.raw)
	}
	if env.file(t, "plain.txt") != "plain" || env.has("real/new.txt") || env.has("real/x") || env.has("real/plain.txt") || !env.has("flink") {
		t.Error("a refused operation changed the tree")
	}
}

func TestFileRoutes_ReadOnlyRefusesMutationsOnTheNextRequest(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "a.txt", "a")
	env.put(t, "dir/b.txt", "b")
	write := func() fileResp { return env.op(t, "write", map[string]any{"path": "w.txt", "content": "w"}) }

	if r := write(); r.status != 200 {
		t.Fatalf("write before the flag = %d %s", r.status, r.raw)
	}
	env.setProject(t, func(p *config.Project) { p.FilesReadOnly = true })

	for _, tc := range []struct {
		op   string
		body any
	}{
		{"write", map[string]any{"path": "w2.txt", "content": "w"}},
		{"mkdir", map[string]any{"parent": "", "name": "newdir"}},
		{"rename", map[string]any{"path": "a.txt", "new_name": "z.txt"}},
		{"move", map[string]any{"path": "a.txt", "dest_dir": "dir"}},
		{"delete", map[string]any{"path": "a.txt"}},
	} {
		r := env.op(t, tc.op, tc.body)
		if r.status != 403 || r.code(t) != projectfs.CodeReadOnly {
			t.Errorf("%s in a read-only project: %d %s, want 403 READ_ONLY", tc.op, r.status, r.raw)
		}
	}
	if !env.has("a.txt") || env.has("w2.txt") || env.has("newdir") || env.has("z.txt") || env.has("dir/a.txt") {
		t.Error("a mutation went through in a read-only project")
	}
	// Reads still work, and a lexical refusal outranks the read-only one.
	if r := env.op(t, "read", map[string]any{"path": "a.txt"}); r.status != 200 {
		t.Errorf("read in a read-only project = %d %s", r.status, r.raw)
	}
	if r := env.op(t, "write", map[string]any{"path": "../x", "content": "w"}); r.code(t) != projectfs.CodeTraversal {
		t.Errorf("traversal in a read-only project: %s", r.raw)
	}

	env.setProject(t, func(p *config.Project) { p.FilesReadOnly = false })
	if r := write(); r.status != 200 {
		t.Errorf("write after the flag was cleared = %d %s", r.status, r.raw)
	}
}

func TestFileRoutes_HostProjectWithoutAPoolIsUnreachable(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.setProject(t, func(p *config.Project) { p.HostID = "no-such-host" })
	r := env.op(t, "list", map[string]any{"path": ""})
	if r.status != 503 || r.code(t) != projectfs.CodeHostUnreachable {
		t.Errorf("host project = %d %s, want 503 HOST_UNREACHABLE", r.status, r.raw)
	}
}

func TestFileRoutes_PutProjectFilesReadOnly(t *testing.T) {
	// A refusing gate proves the flag raises no presence prompt.
	deny, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	env := newFileEnv(t, fileEnvOpts{gate: deny})
	put := func(body map[string]any) map[string]any {
		t.Helper()
		r := env.request(t, "PUT", "/api/projects/"+env.proj.ID, body, nil)
		if r.status != 200 {
			t.Fatalf("PUT %v = %d %s", body, r.status, r.raw)
		}
		return r.json(t)
	}
	stored := func() bool {
		p, _ := config.FindProjectByID(env.store.Get(), env.proj.ID)
		return p.FilesReadOnly
	}

	if v := put(map[string]any{"files_read_only": true}); v["files_read_only"] != true || !stored() {
		t.Errorf("setting the flag: view %v, stored %v", v["files_read_only"], stored())
	}
	if v := put(map[string]any{"name": "Acme Renamed"}); v["files_read_only"] != true || !stored() {
		t.Errorf("a PUT without the key changed the flag: view %v, stored %v", v["files_read_only"], stored())
	}
	if v := put(map[string]any{"files_read_only": false}); v["files_read_only"] == true || stored() {
		t.Errorf("clearing the flag: view %v, stored %v", v["files_read_only"], stored())
	}
}
