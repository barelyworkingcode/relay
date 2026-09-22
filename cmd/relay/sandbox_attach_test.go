package main

// Server-side tests for `relay sandbox`: project resolution, the launch, and
// the byte pump between the client's stream and relay-sessions. The host is a
// FakeService on a real Unix socket that speaks /launch, /terminate and the
// terminal WebSocket dialect, so the code under test dials it exactly as it
// dials the real one.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// sbxWait bounds every wait that is a failure detector, never a pacing delay:
// a passing run does not spend it.
const sbxWait = 5 * time.Second

type sandboxHost struct {
	t      *testing.T
	f      *sessionRoutesFixture
	fs     *FakeService
	router *appRouter

	conns    chan *websocket.Conn
	rejectWS atomic.Bool
}

func newSandboxHost(t *testing.T) *sandboxHost {
	t.Helper()
	t.Setenv("SHELL", "")
	h := &sandboxHost{t: t, f: newSessionRoutesFixture(t), conns: make(chan *websocket.Conn, 8)}
	upgrader := websocket.Upgrader{}
	var fs *FakeService
	launch := fakeLaunchHandler(&fs, func(id string) string {
		return `{"terminalId":"` + id + `","templateId":"shell","name":"t","directory":"","host":null}`
	})
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/launch":
				launch(w, r)
			case "/terminate":
				w.WriteHeader(http.StatusNoContent)
			case "/ws":
				if h.rejectWS.Load() {
					http.Error(w, "no", http.StatusServiceUnavailable)
					return
				}
				c, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				h.conns <- c
			default:
				http.NotFound(w, r)
			}
		},
	})
	h.fs = fs
	h.f.registerFakeSessionsHost(t, fs, selfPeerToken(t).Process())
	h.router = &appRouter{store: h.f.store, sessionDeps: h.f.deps, services: service.NewRegistry(), onChange: func() {}}
	t.Cleanup(func() {
		for {
			select {
			case c := <-h.conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return h
}

func (h *sandboxHost) requestsTo(path string) []*fakeServiceRequest {
	var out []*fakeServiceRequest
	for _, r := range h.fs.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (h *sandboxHost) launches() []hostapi.LaunchRequest {
	var out []hostapi.LaunchRequest
	for _, r := range h.requestsTo("/launch") {
		var spec hostapi.LaunchRequest
		if err := json.Unmarshal(r.Body, &spec); err != nil {
			h.t.Fatalf("launch body: %v", err)
		}
		out = append(out, spec)
	}
	return out
}

func (h *sandboxHost) terminations() []hostapi.TerminateRequest {
	var out []hostapi.TerminateRequest
	for _, r := range h.requestsTo("/terminate") {
		var tr hostapi.TerminateRequest
		if err := json.Unmarshal(r.Body, &tr); err != nil {
			h.t.Fatalf("terminate body: %v", err)
		}
		out = append(out, tr)
	}
	return out
}

// hasAccounting reports whether relay's own session accounting still holds
// sessionID, without consuming the entry (unlike accounting.take, which
// other tests use for a one-shot check). Direct field access under the
// table's own lock is the same seam session_host_modelkey_integration_test.go
// uses to check without taking.
func (h *sandboxHost) hasAccounting(sessionID string) bool {
	h.f.deps.accounting.mu.Lock()
	defer h.f.deps.accounting.mu.Unlock()
	_, ok := h.f.deps.accounting.byID[sessionID]
	return ok
}

func (h *sandboxHost) requireNothingLaunched() {
	h.t.Helper()
	if n := len(h.requestsTo("/launch")); n != 0 {
		h.t.Fatalf("%d launch request(s) reached the host, want none", n)
	}
	if n := len(h.requestsTo("/ws")); n != 0 {
		h.t.Fatalf("%d viewer connection(s) reached the host, want none", n)
	}
}

// project adds a local project and returns it with its resolved path.
func (h *sandboxHost) project(id, name, path string, mutate func(*config.Project)) config.Project {
	h.t.Helper()
	return addLaunchTestProject(h.t, h.f.store, func(p *config.Project) {
		p.ID, p.Name, p.Path = id, name, path
		if mutate != nil {
			mutate(p)
		}
	})
}

func realDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

// --- the hub -------------------------------------------------------------

type hubSide struct {
	t  *testing.T
	c  *websocket.Conn
	id string
	in chan map[string]any
	wm sync.Mutex
}

func (h *sandboxHost) acceptHub() *hubSide {
	h.t.Helper()
	select {
	case c := <-h.conns:
		hs := &hubSide{t: h.t, c: c, in: make(chan map[string]any, 64)}
		var join map[string]any
		if err := c.ReadJSON(&join); err != nil {
			h.t.Fatalf("read join: %v", err)
		}
		if join["type"] != "join_terminal" {
			h.t.Fatalf("first viewer frame = %v, want join_terminal", join)
		}
		hs.id, _ = join["terminalId"].(string)
		if hs.id == "" {
			h.t.Fatalf("join_terminal carries no terminalId: %v", join)
		}
		go func() {
			defer close(hs.in)
			for {
				var m map[string]any
				if err := c.ReadJSON(&m); err != nil {
					return
				}
				hs.in <- m
			}
		}()
		return hs
	case <-time.After(sbxWait):
		h.t.Fatal("relay never opened a viewer connection to the host")
		return nil
	}
}

func (hs *hubSide) send(m map[string]any) {
	hs.t.Helper()
	hs.wm.Lock()
	defer hs.wm.Unlock()
	if err := hs.c.WriteJSON(m); err != nil {
		hs.t.Fatalf("hub write: %v", err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func (hs *hubSide) output(id, data string) {
	hs.send(map[string]any{"type": "terminal_output", "terminalId": id, "data": b64(data)})
}

func (hs *hubSide) joined(scrollback string) {
	hs.send(map[string]any{"type": "terminal_joined", "terminalId": hs.id, "state": "running", "cols": 100, "rows": 30, "scrollback": b64(scrollback)})
}

func (hs *hubSide) exit(code int) {
	hs.send(map[string]any{"type": "terminal_exit", "terminalId": hs.id, "exitCode": code})
}

// next returns the next frame relay wrote to the hub.
func (hs *hubSide) next() map[string]any {
	hs.t.Helper()
	select {
	case m, ok := <-hs.in:
		if !ok {
			hs.t.Fatal("hub connection closed while waiting for a frame from relay")
		}
		return m
	case <-time.After(sbxWait):
		hs.t.Fatal("relay sent nothing to the hub")
		return nil
	}
}

type attachOutcome struct {
	att bridge.SandboxAttachment
	err error
}

func (h *sandboxHost) startAttach(req bridge.SandboxAttachRequest) <-chan attachOutcome {
	out := make(chan attachOutcome, 1)
	go func() {
		att, err := h.router.SandboxAttach(context.Background(), req)
		out <- attachOutcome{att, err}
	}()
	return out
}

func waitOutcome(t *testing.T, c <-chan attachOutcome) attachOutcome {
	t.Helper()
	select {
	case o := <-c:
		return o
	case <-time.After(sbxWait):
		t.Fatal("SandboxAttach did not return")
		return attachOutcome{}
	}
}

// attach launches and completes the join handshake with scrollback, sending
// the frames in early before the join is answered.
func (h *sandboxHost) attach(req bridge.SandboxAttachRequest, scrollback string, early ...func(hs *hubSide)) (bridge.SandboxAttachment, *hubSide) {
	h.t.Helper()
	res := h.startAttach(req)
	hs := h.acceptHub()
	for _, f := range early {
		f(hs)
	}
	hs.joined(scrollback)
	o := waitOutcome(h.t, res)
	if o.err != nil {
		h.t.Fatalf("SandboxAttach: %v", o.err)
	}
	return o.att, hs
}

func refusalFrom(t *testing.T, err error) *bridge.SandboxRefusal {
	t.Helper()
	var ref *bridge.SandboxRefusal
	if !errors.As(err, &ref) {
		t.Fatalf("error = %v, want a *bridge.SandboxRefusal", err)
	}
	return ref
}

// --- the client stream ---------------------------------------------------

type streamEnd struct {
	conn   net.Conn
	frames chan bridge.StreamFrame
	raw    chan string
	done   <-chan struct{}
}

// serve runs att.Serve over a real Unix socket pair, closing the server end
// when Serve returns, as the bridge's handleConn does.
func serveAttachment(t *testing.T, att bridge.SandboxAttachment, wrap func(net.Conn) net.Conn) *streamEnd {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	client, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-acc
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if wrap != nil {
		server = wrap(server)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		att.Serve(context.Background(), bridge.NewFrameConn(server, "test", 0))
		_ = server.Close()
	}()
	s := &streamEnd{conn: client, frames: make(chan bridge.StreamFrame, 256), raw: make(chan string, 256), done: done}
	go func() {
		defer close(s.frames)
		defer close(s.raw)
		r := bufio.NewReaderSize(client, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var f bridge.StreamFrame
			if json.Unmarshal(line, &f) == nil {
				s.frames <- f
				s.raw <- strings.TrimSpace(string(line))
			}
		}
	}()
	return s
}

func (s *streamEnd) send(f bridge.StreamFrame) {
	b, _ := json.Marshal(f)
	_, _ = s.conn.Write(append(b, '\n'))
}

// output gathers output frames until at least n bytes have arrived.
func (s *streamEnd) output(t *testing.T, n int) string {
	t.Helper()
	var sb strings.Builder
	for sb.Len() < n {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatalf("stream closed after %d output bytes, want %d: %q", sb.Len(), n, sb.String())
			}
			if f.Type == bridge.StreamOutput {
				sb.Write(f.Data)
			}
		case <-time.After(sbxWait):
			t.Fatalf("timed out after %d output bytes, want %d: %q", sb.Len(), n, sb.String())
		}
	}
	return sb.String()
}

// drain reads to the end of the stream.
func (s *streamEnd) drain(t *testing.T) (out string, exit *bridge.StreamFrame, rawExit string) {
	t.Helper()
	var sb strings.Builder
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return sb.String(), exit, rawExit
			}
			raw := <-s.raw
			switch f.Type {
			case bridge.StreamOutput:
				sb.Write(f.Data)
			case bridge.StreamExit:
				fc := f
				exit, rawExit = &fc, raw
			}
		case <-time.After(sbxWait):
			t.Fatal("stream did not end")
		}
	}
}

func (s *streamEnd) waitServeDone(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(sbxWait):
		t.Fatal("Serve did not return")
	}
}

func sbxReq(template, cwd string) bridge.SandboxAttachRequest {
	return bridge.SandboxAttachRequest{Template: template, Cwd: cwd, Cols: 100, Rows: 30}
}

// --- 1, 7: launches the named template for the project holding the cwd ---

func TestSandboxAttach_LaunchesNamedTemplateForTheProjectHoldingTheDirectory(t *testing.T) {
	sub := func(p string) string { d := filepath.Join(p, "src", "deep"); _ = os.MkdirAll(d, 0o755); return d }
	link := func(target string) string {
		l := filepath.Join(mkShortTempDir(t, "sbxlink-"), "cwd")
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	cases := []struct {
		name     string
		template string
		cwd      func(root string) string
		sandbox  bool
	}{
		{"project root, sandboxed template", "shell", func(r string) string { return r }, true},
		{"subfolder, sandboxed template", "claude-code", sub, true},
		{"symlinked directory inside the project", "shell", func(r string) string { return link(sub(r)) }, true},
		{"project root, template that opts out of the sandbox", "plain", func(r string) string { return r }, false},
		{"subfolder, template that opts out of the sandbox", "plain", sub, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSandboxHost(t)
			proj := h.project("p-main", "Main", t.TempDir(), nil)
			root := realDir(t, proj.Path)
			cwd := c.cwd(root)

			att, _ := h.attach(sbxReq(c.template, cwd), "")
			specs := h.launches()
			if len(specs) != 1 {
				t.Fatalf("%d launches, want 1", len(specs))
			}
			spec := specs[0]
			if spec.Kind != KindPTY || spec.TemplateID != c.template {
				t.Errorf("launched kind=%q template=%q, want pty/%q", spec.Kind, spec.TemplateID, c.template)
			}
			var ref struct{ ID string }
			_ = json.Unmarshal(spec.Project, &ref)
			if ref.ID != "p-main" {
				t.Errorf("launched for project %q, want p-main", ref.ID)
			}
			if !strings.HasPrefix(realDir(t, spec.Directory), root) {
				t.Errorf("launch directory %q is outside the project %q", spec.Directory, root)
			}
			if (spec.Sandbox != nil) != c.sandbox {
				t.Errorf("Sandbox spec present=%v, want %v: the template's own setting must reach the host untouched", spec.Sandbox != nil, c.sandbox)
			}
			if spec.PTY == nil || spec.PTY.Cols != 100 || spec.PTY.Rows != 30 {
				t.Errorf("PTY size = %+v, want the client's 100x30", spec.PTY)
			}
			res := att.Result()
			if res.SessionID != spec.SessionID || res.TemplateID != c.template || res.ProjectName != "Main" {
				t.Errorf("result = %+v, want session %s template %s project Main", res, spec.SessionID, c.template)
			}
			att.Abort()
		})
	}
}

func TestSandboxAttach_ViewerJoinsTheLaunchedSessionWithTheHostsBearer(t *testing.T) {
	h := newSandboxHost(t)
	proj := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, proj.Path)), "")
	defer att.Abort()

	if want := h.launches()[0].SessionID; hs.id != want {
		t.Fatalf("relay joined terminal %q, want the launched session %q", hs.id, want)
	}
	ws := h.requestsTo("/ws")
	if len(ws) != 1 || ws[0].Headers.Get("Authorization") != "Bearer "+h.fs.Token() {
		t.Fatalf("viewer connection did not carry the host's bearer: %+v", ws)
	}
}

// --- 2, 3, 4: refusals launch nothing -------------------------------------

func TestSandboxAttach_RefusalsLaunchNothing(t *testing.T) {
	type tc struct {
		name     string
		setup    func(h *sandboxHost) bridge.SandboxAttachRequest
		reason   string
		mentions []string
	}
	cases := []tc{
		{
			name: "directory outside every registered project",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				h.project("p-main", "Main", t.TempDir(), nil)
				return sbxReq("shell", realDir(t, t.TempDir()))
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "no projects registered at all",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				return sbxReq("shell", realDir(t, t.TempDir()))
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "the parent of a project is not inside it",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				parent := realDir(t, t.TempDir())
				child := filepath.Join(parent, "proj")
				_ = os.MkdirAll(child, 0o755)
				h.project("p-main", "Main", child, nil)
				return sbxReq("shell", parent)
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "a sibling whose name shares the project's prefix is not inside it",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				parent := realDir(t, t.TempDir())
				proj, sibling := filepath.Join(parent, "app"), filepath.Join(parent, "app-other")
				_ = os.MkdirAll(proj, 0o755)
				_ = os.MkdirAll(sibling, 0o755)
				h.project("p-main", "Main", proj, nil)
				return sbxReq("shell", sibling)
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "remote project holding the path is never matched",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				dir := realDir(t, t.TempDir())
				h.project("p-remote", "Remote", dir, func(p *config.Project) { p.Kind = config.ProjectKindRemote })
				return sbxReq("shell", dir)
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "ssh-hosted project holding the path is never matched",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				dir := realDir(t, t.TempDir())
				h.project("p-ssh", "Hosted", dir, func(p *config.Project) { p.HostID = "some-host" })
				return sbxReq("shell", dir)
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "a remote or hosted project cannot be selected with --project either",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				dir := realDir(t, t.TempDir())
				h.project("p-remote", "Remote", dir, func(p *config.Project) { p.Kind = config.ProjectKindRemote })
				h.project("p-ssh", "Hosted", dir, func(p *config.Project) { p.HostID = "some-host" })
				r := sbxReq("shell", dir)
				r.Project = "p-remote"
				return r
			},
			reason: bridge.SandboxReasonNoProject,
		},
		{
			name: "unknown template",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				p := h.project("p-main", "Main", t.TempDir(), nil)
				return sbxReq("no-such-template", realDir(t, p.Path))
			},
			reason: bridge.SandboxReasonUnknownTemplate,
		},
		{
			name: "template exists but the project allows none (empty allowed_templates)",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				p := h.project("p-main", "Main", t.TempDir(), func(p *config.Project) { p.AllowedTemplates = nil })
				return sbxReq("shell", realDir(t, p.Path))
			},
			reason: bridge.SandboxReasonTemplateDenied,
		},
		{
			name: "template exists but is not among the project's listed templates",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				p := h.project("p-main", "Main", t.TempDir(), func(p *config.Project) { p.AllowedTemplates = []string{"pi"} })
				return sbxReq("shell", realDir(t, p.Path))
			},
			reason: bridge.SandboxReasonTemplateDenied,
		},
		{
			name: "two projects hold the directory and none was named",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				outer := realDir(t, t.TempDir())
				inner := filepath.Join(outer, "inner")
				_ = os.MkdirAll(inner, 0o755)
				h.project("p-outer", "Outer", outer, nil)
				h.project("p-inner", "Inner", inner, nil)
				return sbxReq("shell", inner)
			},
			reason:   bridge.SandboxReasonProjectAmbiguous,
			mentions: []string{"Outer", "Inner"},
		},
		{
			name: "two projects share one path",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				dir := realDir(t, t.TempDir())
				h.project("p-one", "One", dir, nil)
				h.project("p-two", "Two", dir, nil)
				return sbxReq("shell", dir)
			},
			reason:   bridge.SandboxReasonProjectAmbiguous,
			mentions: []string{"One", "Two"},
		},
		{
			name: "two projects share a name, so the name does not disambiguate",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				dir := realDir(t, t.TempDir())
				h.project("p-one", "Same", dir, nil)
				h.project("p-two", "Same", dir, nil)
				r := sbxReq("shell", dir)
				r.Project = "Same"
				return r
			},
			reason: bridge.SandboxReasonProjectAmbiguous,
		},
		{
			name: "--project names a project that does not hold the directory",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				outer := realDir(t, t.TempDir())
				inner := filepath.Join(outer, "inner")
				_ = os.MkdirAll(inner, 0o755)
				h.project("p-outer", "Outer", outer, nil)
				h.project("p-inner", "Inner", inner, nil)
				h.project("p-elsewhere", "Elsewhere", t.TempDir(), nil)
				r := sbxReq("shell", inner)
				r.Project = "Elsewhere"
				return r
			},
			reason:   bridge.SandboxReasonProjectMismatch,
			mentions: []string{"Outer", "Inner"},
		},
		{
			name: "--project names a project that does not hold the directory, given by id, with a single holder",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				p := h.project("p-main", "Main", t.TempDir(), nil)
				h.project("p-elsewhere", "Elsewhere", t.TempDir(), nil)
				r := sbxReq("shell", realDir(t, p.Path))
				r.Project = "p-elsewhere"
				return r
			},
			reason: bridge.SandboxReasonProjectMismatch,
		},
		{
			name: "--project names nothing that exists",
			setup: func(h *sandboxHost) bridge.SandboxAttachRequest {
				p := h.project("p-main", "Main", t.TempDir(), nil)
				r := sbxReq("shell", realDir(t, p.Path))
				r.Project = "ghost"
				return r
			},
			reason: bridge.SandboxReasonProjectMismatch,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSandboxHost(t)
			req := c.setup(h)
			// A server that wrongly launches would wait for a viewer join no
			// one answers, so the call is bounded.
			o := waitOutcome(t, h.startAttach(req))
			if o.err == nil {
				o.att.Abort()
				t.Fatal("SandboxAttach succeeded, want a refusal")
			}
			ref := refusalFrom(t, o.err)
			if ref.Reason != c.reason {
				t.Errorf("reason = %q (%s), want %q", ref.Reason, ref.Message, c.reason)
			}
			if ref.Message == "" {
				t.Error("refusal has no message for a human")
			}
			for _, name := range c.mentions {
				if !strings.Contains(ref.Message+strings.Join(ref.Projects, " "), name) {
					t.Errorf("refusal %+v does not name project %q", ref, name)
				}
			}
			h.requireNothingLaunched()
		})
	}
}

func TestSandboxAttach_AllowedTemplatesSemantics(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		tmpl    string
		ok      bool
	}{
		{"empty allows none", nil, "shell", false},
		{"lone star allows every template", []string{"*"}, "pi", true},
		{"lone star allows a template that opts out of the sandbox", []string{"*"}, "plain", true},
		{"listed id is allowed", []string{"pi", "shell"}, "shell", true},
		{"unlisted id is refused", []string{"pi", "shell"}, "claude-code", false},
		{"listed opt-out template is allowed", []string{"plain"}, "plain", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSandboxHost(t)
			p := h.project("p-main", "Main", t.TempDir(), func(p *config.Project) { p.AllowedTemplates = c.allowed })
			res := h.startAttach(sbxReq(c.tmpl, realDir(t, p.Path)))
			if c.ok {
				hs := h.acceptHub()
				hs.joined("")
				o := waitOutcome(t, res)
				if o.err != nil {
					t.Fatalf("refused: %v", o.err)
				}
				o.att.Abort()
				return
			}
			o := waitOutcome(t, res)
			if o.err == nil {
				o.att.Abort()
				t.Fatal("launched a template the project does not allow")
			}
			if ref := refusalFrom(t, o.err); ref.Reason != bridge.SandboxReasonTemplateDenied {
				t.Fatalf("reason = %q, want %q", ref.Reason, bridge.SandboxReasonTemplateDenied)
			}
			h.requireNothingLaunched()
		})
	}
}

func TestSandboxAttach_ProjectSelectionAmongProjectsHoldingTheDirectory(t *testing.T) {
	cases := []struct {
		selector string
		wantID   string
	}{
		{"Outer", "p-outer"},
		{"p-outer", "p-outer"},
		{"Inner", "p-inner"},
		{"p-inner", "p-inner"},
	}
	for _, c := range cases {
		t.Run(c.selector, func(t *testing.T) {
			h := newSandboxHost(t)
			outer := realDir(t, t.TempDir())
			inner := filepath.Join(outer, "inner")
			_ = os.MkdirAll(inner, 0o755)
			h.project("p-outer", "Outer", outer, nil)
			h.project("p-inner", "Inner", inner, nil)
			req := sbxReq("shell", inner)
			req.Project = c.selector

			att, _ := h.attach(req, "")
			defer att.Abort()
			var ref struct{ ID string }
			_ = json.Unmarshal(h.launches()[0].Project, &ref)
			if ref.ID != c.wantID {
				t.Fatalf("launched for %q, want %q", ref.ID, c.wantID)
			}
		})
	}
}

func TestSandboxAttach_RemoteAndHostedProjectsDoNotMakeALocalDirectoryAmbiguous(t *testing.T) {
	h := newSandboxHost(t)
	dir := realDir(t, t.TempDir())
	h.project("p-local", "Local", dir, nil)
	h.project("p-remote", "Remote", dir, func(p *config.Project) { p.Kind = config.ProjectKindRemote })
	h.project("p-ssh", "Hosted", dir, func(p *config.Project) { p.HostID = "some-host" })

	att, _ := h.attach(sbxReq("shell", dir), "")
	defer att.Abort()
	var ref struct{ ID string }
	_ = json.Unmarshal(h.launches()[0].Project, &ref)
	if ref.ID != "p-local" {
		t.Fatalf("launched for %q, want the only local project", ref.ID)
	}
}

func TestSandboxAttach_RefusesWhenTheSessionHostIsNotRunning(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	h.f.deps.enhanced.Forget(config.RelaySessionsServiceID)

	att, err := h.router.SandboxAttach(context.Background(), sbxReq("shell", realDir(t, p.Path)))
	if err == nil {
		att.Abort()
		t.Fatal("SandboxAttach succeeded with no session host")
	}
	if ref := refusalFrom(t, err); ref.Reason != bridge.SandboxReasonUnavailable {
		t.Fatalf("reason = %q, want %q", ref.Reason, bridge.SandboxReasonUnavailable)
	}
	h.requireNothingLaunched()
}

func TestSandboxAttach_RouterWithNoSessionDepsRefusesInsteadOfPanicking(t *testing.T) {
	mkSandboxRelayHome(t)
	r := &appRouter{}
	if _, err := r.SandboxAttach(context.Background(), sbxReq("shell", "/tmp")); err == nil {
		t.Fatal("an unwired router accepted a sandbox request")
	}
}

// --- 6: ungated -----------------------------------------------------------

func TestSandboxAttach_IsUngatedAndReachesNoRemoteTable(t *testing.T) {
	for _, op := range presence.GatedOps {
		if strings.Contains(strings.ToLower(op), "sandbox") {
			t.Errorf("presence.GatedOps contains %q; relay sandbox is deliberately ungated", op)
		}
	}
	if _, ok := remoteHandlers[bridge.ReqSandboxAttach]; ok {
		t.Error("remoteHandlers has an entry for SandboxAttach: a VM could reach it")
	}
	if _, ok := remoteConfigHandlers[bridge.ReqSandboxAttach]; ok {
		t.Error("remoteConfigHandlers has an entry for SandboxAttach")
	}
}

func TestSandboxAttach_WorksWithNoPresenceGateWired(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	// The fixture router has no presence gate at all, so success here means the
	// attach path never asks one.
	att, _ := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	att.Abort()
}

// --- 8: disconnect and exit ----------------------------------------------

func TestSandboxAttach_ClientDisconnectTerminatesTheSessionExactlyOnce(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	_ = s.conn.Close()
	s.waitServeDone(t)

	terms := h.terminations()
	if len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
	if terms[0].Reason == "" {
		t.Error("terminate carries no reason")
	}
}

func TestSandboxAttach_ClientDisconnectWithNothingSentStillTerminates(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "banner")
	s := serveAttachment(t, att, nil)
	// The client goes away before reading or writing a single frame.
	_ = s.conn.Close()
	s.waitServeDone(t)
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
}

func TestSandboxAttach_AbortTerminatesASessionNobodyEverAttachedTo(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	att.Abort()
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
}

func TestSandboxAttach_TerminatesOnceEvenWhenBothAbortAndServeEnd(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, _ := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)
	_ = s.conn.Close()
	s.waitServeDone(t)
	att.Abort()
	if n := len(h.terminations()); n != 1 {
		t.Fatalf("%d terminations, want exactly 1", n)
	}
}

func TestSandboxAttach_JoinRefusedByTheHostEndsTheLaunchedSession(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	res := h.startAttach(sbxReq("shell", realDir(t, p.Path)))
	hs := h.acceptHub()
	hs.send(map[string]any{"type": "error", "message": "no such terminal"})

	o := waitOutcome(t, res)
	if o.err == nil {
		o.att.Abort()
		t.Fatal("SandboxAttach succeeded although the host refused the join")
	}
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
}

func TestSandboxAttach_HostDroppingTheViewerBeforeTheJoinAnswerEndsTheLaunchedSession(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	res := h.startAttach(sbxReq("shell", realDir(t, p.Path)))
	hs := h.acceptHub()
	_ = hs.c.Close()

	o := waitOutcome(t, res)
	if o.err == nil {
		o.att.Abort()
		t.Fatal("SandboxAttach succeeded although the viewer connection died")
	}
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
}

func TestSandboxAttach_ViewerConnectionRefusedEndsTheLaunchedSession(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	h.rejectWS.Store(true)

	att, err := h.router.SandboxAttach(context.Background(), sbxReq("shell", realDir(t, p.Path)))
	if err == nil {
		att.Abort()
		t.Fatal("SandboxAttach succeeded with no viewer connection")
	}
	launches := h.launches()
	if len(launches) != 1 {
		t.Fatalf("%d launches, want 1", len(launches))
	}
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != launches[0].SessionID {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, launches[0].SessionID)
	}
}

func TestSandboxAttach_ToolExitReachesTheClientAsAnExitFrameAndIsNotTerminated(t *testing.T) {
	for _, code := range []int{0, 3, 130} {
		t.Run(fmt.Sprintf("code_%d", code), func(t *testing.T) {
			h := newSandboxHost(t)
			p := h.project("p-main", "Main", t.TempDir(), nil)
			att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
			s := serveAttachment(t, att, nil)

			hs.output(hs.id, "bye")
			hs.exit(code)
			out, exit, raw := s.drain(t)
			s.waitServeDone(t)

			if out != "bye" {
				t.Errorf("output = %q, want %q delivered before the exit", out, "bye")
			}
			if exit == nil || exit.Code != code {
				t.Fatalf("exit frame = %+v, want code %d", exit, code)
			}
			if !strings.Contains(raw, `"code":`) {
				t.Errorf("exit frame %s omits the code; a zero exit must still be explicit", raw)
			}
			if n := len(h.terminations()); n != 0 {
				t.Fatalf("%d terminate call(s) after the tool exited on its own, want 0", n)
			}
		})
	}
}

func TestSandboxAttach_SessionClosedElsewhereEndsTheStreamWithoutAnExitFrame(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	if !h.hasAccounting(hs.id) {
		t.Fatal("test premise broken: the launch left no accounting entry to clean up")
	}

	hs.send(map[string]any{"type": "terminal_closed", "terminalId": hs.id})
	_, exit, _ := s.drain(t)
	s.waitServeDone(t)
	if exit != nil {
		t.Fatalf("got an exit frame %+v for a session closed by someone else; there is no exit status to report", exit)
	}
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id || terms[0].Reason != "sandbox_session_closed" {
		t.Fatalf("terminations = %+v, want exactly one for %s with reason sandbox_session_closed", terms, hs.id)
	}
	if h.hasAccounting(hs.id) {
		t.Error("session accounting entry for the dropped session survived; it must be taken exactly as a client disconnect takes it")
	}
}

func TestSandboxAttach_HostConnectionLossEndsTheStream(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	_ = hs.c.Close()
	_, exit, _ := s.drain(t)
	s.waitServeDone(t)
	if exit != nil {
		t.Fatalf("exit frame %+v invented for a lost host connection", exit)
	}
}

// --- 12: stream protocol ---------------------------------------------------

func TestSandboxAttach_ScrollbackIsDeliveredBeforeLiveOutputAndInOrder(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "SCROLL|", func(hs *hubSide) {
		// Arrives after join_terminal and before terminal_joined.
		hs.output(hs.id, "EARLY|")
	})
	s := serveAttachment(t, att, nil)
	hs.output(hs.id, "LIVE|")

	if got := s.output(t, len("SCROLL|EARLY|LIVE|")); got != "SCROLL|EARLY|LIVE|" {
		t.Fatalf("client saw %q, want scrollback, then buffered output, then live output", got)
	}
}

func TestSandboxAttach_AChunkPresentInBothScrollbackAndLiveOutputStillFollowsScrollback(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "AB", func(hs *hubSide) {
		hs.output(hs.id, "B")
	})
	s := serveAttachment(t, att, nil)
	hs.output(hs.id, "C")

	got := s.output(t, len("ABBC"))
	if !strings.HasPrefix(got, "AB") || !strings.HasSuffix(got, "C") {
		t.Fatalf("client saw %q, want scrollback first and the later output last", got)
	}
}

func TestSandboxAttach_LargeScrollbackIsChunkedBelowTheFrameCeiling(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	big := strings.Repeat("x", 3<<20)
	att, _ := h.attach(sbxReq("shell", realDir(t, p.Path)), big)
	s := serveAttachment(t, att, nil)
	if got := s.output(t, len(big)); got != big {
		t.Fatalf("scrollback arrived damaged: %d bytes, want %d", len(got), len(big))
	}
}

func TestSandboxAttach_FramesForOtherTerminalsAreIgnored(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "", func(hs *hubSide) {
		hs.output("someone-else", "OTHER-EARLY")
		hs.send(map[string]any{"type": "terminal_exit", "terminalId": "someone-else", "exitCode": 9})
	})
	s := serveAttachment(t, att, nil)

	hs.output("someone-else", "OTHER-LIVE")
	hs.send(map[string]any{"type": "terminal_exit", "terminalId": "someone-else", "exitCode": 9})
	hs.send(map[string]any{"type": "terminal_closed", "terminalId": "someone-else"})
	hs.output(hs.id, "MINE")
	hs.exit(0)

	out, exit, _ := s.drain(t)
	if out != "MINE" {
		t.Fatalf("client saw %q, want only this session's output", out)
	}
	if exit == nil || exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0: another terminal's exit or close must not end this stream", exit)
	}
}

func TestSandboxAttach_ClientInputAndResizeReachTheSessionsTerminal(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	s.send(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte("ls -l\r")})
	in := hs.next()
	if in["type"] != "terminal_input" || in["terminalId"] != hs.id || in["data"] != b64("ls -l\r") {
		t.Fatalf("host saw %v, want terminal_input for %s carrying the typed bytes", in, hs.id)
	}

	s.send(bridge.StreamFrame{Type: bridge.StreamResize, Cols: 132, Rows: 43})
	rz := hs.next()
	if rz["type"] != "terminal_resize" || rz["terminalId"] != hs.id || rz["cols"] != float64(132) || rz["rows"] != float64(43) {
		t.Fatalf("host saw %v, want terminal_resize 132x43 for %s", rz, hs.id)
	}
}

func TestSandboxAttach_BinaryInputSurvivesTheRoundTrip(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	raw := []byte{0x03, 0x1b, '[', 'A', 0x00, 0xff, 0xc3, 0x28}
	s.send(bridge.StreamFrame{Type: bridge.StreamInput, Data: raw})
	in := hs.next()
	got, err := base64.StdEncoding.DecodeString(in["data"].(string))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("host saw %q (%v), want %q byte for byte", got, err, raw)
	}
}

// --- 9: no deadlines --------------------------------------------------------

type sbxDeadlineRecorder struct {
	net.Conn
	calls atomic.Int32
}

func (d *sbxDeadlineRecorder) SetDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetDeadline(t)
}
func (d *sbxDeadlineRecorder) SetReadDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetReadDeadline(t)
}
func (d *sbxDeadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetWriteDeadline(t)
}

func TestSandboxAttach_AttachedStreamAppliesNoDeadlineToTheClientConnection(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "hello")
	var rec *sbxDeadlineRecorder
	s := serveAttachment(t, att, func(c net.Conn) net.Conn { rec = &sbxDeadlineRecorder{Conn: c}; return rec })

	s.output(t, 5)
	s.send(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte("x")})
	hs.next()
	hs.output(hs.id, "more")
	s.output(t, 4)
	hs.exit(0)
	s.drain(t)
	s.waitServeDone(t)
	if n := rec.calls.Load(); n != 0 {
		t.Fatalf("%d deadline(s) applied to the attached client connection", n)
	}
}

func TestSandboxAttach_AnIdleStreamIsNotEndedByRelay(t *testing.T) {
	// There is no clock to inject: the attach path holds no timer at all (the
	// source scan below pins that). What this can show is that a quiet stream,
	// with traffic before and after, is still one session with no terminate.
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	att, hs := h.attach(sbxReq("shell", realDir(t, p.Path)), "")
	s := serveAttachment(t, att, nil)

	hs.output(hs.id, "before")
	s.output(t, 6)
	// Nothing happens for as long as the test likes; sleep and wake look
	// exactly like this to relay.
	select {
	case <-s.done:
		t.Fatal("Serve returned while both sides were idle")
	default:
	}
	if n := len(h.terminations()); n != 0 {
		t.Fatalf("%d terminate call(s) on an idle attached stream", n)
	}
	hs.output(hs.id, "after")
	s.output(t, 5)
	hs.exit(0)
	s.drain(t)
}

// --- end to end over the real bridge socket --------------------------------

type sbxMember struct{ member bool }

func (m sbxMember) ResolveMembership(peertoken.Token, time.Time) (bridge.MemberSession, bool) {
	return bridge.MemberSession{SessionID: "sess-inside", ProjectID: "p-main"}, m.member
}

func (m sbxMember) RefreshMembership(s bridge.MemberSession) (bridge.MemberSession, bool) {
	return s, m.member
}

func sbxStartBridge(t *testing.T, h *sandboxHost, member bool) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := bridge.NewBridgeServer(ctx, h.router)
	if err != nil {
		cancel()
		t.Fatalf("NewBridgeServer: %v", err)
	}
	srv.SetMembershipResolverForTest(sbxMember{member: member})
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { cancel(); srv.Close() })
	return bridge.SocketPath()
}

func sbxBridgeRequest(t *testing.T, conn net.Conn, req bridge.SandboxAttachRequest) {
	t.Helper()
	args, _ := json.Marshal(req)
	line, _ := json.Marshal(bridge.BridgeRequest{Type: bridge.ReqSandboxAttach, Arguments: args})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func TestSandboxAttach_OverTheBridgeSocketLaunchesStreamsAndReportsTheExit(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	sock := sbxStartBridge(t, h, false)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	sbxBridgeRequest(t, conn, sbxReq("shell", realDir(t, p.Path)))
	hs := h.acceptHub()
	hs.joined("$ ")

	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack bridge.BridgeResponse
	_ = json.Unmarshal(line, &ack)
	if ack.Type != bridge.RespAttached {
		t.Fatalf("ack = %s", line)
	}

	in, _ := json.Marshal(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte("e")})
	_, _ = conn.Write(append(in, '\n'))
	if got := hs.next(); got["type"] != "terminal_input" || got["data"] != b64("e") {
		t.Fatalf("host saw %v, want the typed byte", got)
	}
	hs.output(hs.id, "echo")
	hs.exit(4)

	var out strings.Builder
	exitCode := -99
	for exitCode == -99 {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("stream ended before an exit frame: %v (output %q)", err, out.String())
		}
		var f bridge.StreamFrame
		_ = json.Unmarshal(line, &f)
		switch f.Type {
		case bridge.StreamOutput:
			out.Write(f.Data)
		case bridge.StreamExit:
			exitCode = f.Code
		}
	}
	if out.String() != "$ echo" || exitCode != 4 {
		t.Fatalf("output %q exit %d, want %q and 4", out.String(), exitCode, "$ echo")
	}
	if n := len(h.terminations()); n != 0 {
		t.Fatalf("%d terminate call(s) after a normal exit", n)
	}
}

func TestSandboxAttach_OverTheBridgeSocketRefusesASessionMemberAndLaunchesNothing(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	sock := sbxStartBridge(t, h, true)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sbxBridgeRequest(t, conn, sbxReq("plain", realDir(t, p.Path)))

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp bridge.BridgeResponse
	_ = json.Unmarshal(line, &resp)
	var ref bridge.SandboxRefusal
	_ = json.Unmarshal(resp.Data, &ref)
	if resp.Type != bridge.RespError || ref.Reason != bridge.SandboxReasonInsideSession {
		t.Fatalf("response = %s, want an inside_session refusal", line)
	}
	h.requireNothingLaunched()
}

func TestSandboxAttach_OverTheBridgeSocketClientDisconnectEndsTheSession(t *testing.T) {
	h := newSandboxHost(t)
	p := h.project("p-main", "Main", t.TempDir(), nil)
	sock := sbxStartBridge(t, h, false)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	sbxBridgeRequest(t, conn, sbxReq("shell", realDir(t, p.Path)))
	hs := h.acceptHub()
	hs.joined("")
	if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	_ = conn.Close()

	deadline := time.After(sbxWait)
	for len(h.terminations()) == 0 {
		select {
		case <-deadline:
			t.Fatal("relay never terminated the session after the client vanished")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if terms := h.terminations(); len(terms) != 1 || terms[0].SessionID != hs.id {
		t.Fatalf("terminations = %+v, want exactly one for %s", terms, hs.id)
	}
}
