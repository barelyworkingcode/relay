package features

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

// outsideRig is a relay instance beside a private tmux server holding one
// session that relay did not start.
type outsideRig struct {
	t                               *testing.T
	i                               *harness.Instance
	tmux, socket, wrapper, inputLog string
	callLog                         string
	reader, configurer              *harness.Client
	hostID                          string
}

func outsideQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func outsideWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
		t.Fatal(err)
	}
}

func outsideStart(t *testing.T) *outsideRig {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("real tmux is required: %v", err)
	}
	dir, err := os.MkdirTemp("/tmp", "outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &outsideRig{t: t, tmux: tmux, socket: filepath.Join(dir, "server.sock"), wrapper: filepath.Join(dir, "tmux"),
		inputLog: filepath.Join(dir, "input"), callLog: filepath.Join(dir, "calls")}
	r.installWrapper("")
	// A private socket is deliberate: no command here can reach another tmux server.
	r.run("-f", "/dev/null", "new-session", "-d", "-s", "acme-outside", "-x", "100", "-y", "40", "-c", dir,
		"exec /bin/sh -c "+outsideQuote("printf 'ACME_READY\\n'; while IFS= read -r line; do printf '%s\\n' \"$line\" >> "+outsideQuote(r.inputLog)+"; printf 'ACME_ACK:%s\\n' \"$line\"; done"))
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", r.socket, "kill-server").Run() })
	r.i = harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove, SSHStub: true})
	r.i.WaitSessionHost(g1tHostDeadline)
	r.reader = r.i.SocketHTTP(r.i.Credential("reader"))
	r.configurer = r.i.SocketHTTP(r.i.Credential("configurer"))
	// Console discovery finds tmux on PATH; the host uses its configured binary.
	outsideWrite(t, filepath.Join(r.i.Home, ".local", "bin", "tmux"), "#!/bin/sh\nexec "+outsideQuote(r.wrapper)+" \"$@\"\n")
	resp := r.configurer.Do("POST", "/api/hosts", map[string]any{"name": "Acme box", "target": "acme@127.0.0.1", "tmux_path": r.wrapper})
	if resp.Status != 201 {
		t.Fatalf("host create: %d %s", resp.Status, resp.Body)
	}
	var h struct {
		ID string `json:"id"`
	}
	resp.JSON(t, &h)
	if h.ID == "" {
		t.Fatal("host has no id")
	}
	r.hostID = h.ID
	return r
}

// installWrapper writes the tmux the host runs. "old" reports tmux 3.1.
func (r *outsideRig) installWrapper(mode string) {
	r.t.Helper()
	fault := ""
	if mode == "old" {
		fault = "if [ \"$1\" = -V ]; then echo 'tmux 3.1'; exit 0; fi\n"
	}
	outsideWrite(r.t, r.wrapper, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+outsideQuote(r.callLog)+"\n"+fault+
		"exec "+outsideQuote(r.tmux)+" -S "+outsideQuote(r.socket)+" \"$@\"\n")
}

func (r *outsideRig) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.tmux, append([]string{"-S", r.socket}, args...)...)
	cmd.Env = append(os.Environ(), "TMUX=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// untilClients polls the real client inventory (tmux gives a test no event)
// until accept holds for the lines whose flags column is not the owner's.
func (r *outsideRig) untilClients(what string, accept func(lines []string) bool) []string {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var lines []string
		for _, l := range strings.Split(r.run("list-clients", "-F", "#{client_flags}|#{client_width}|#{client_height}|#{session_name}|#{client_control_mode}"), "\n") {
			if l != "" && !strings.HasSuffix(l, "|1") { // the control-mode owner is not a viewer
				lines = append(lines, l)
			}
		}
		if accept(lines) {
			return lines
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("tmux clients: %s: got %q", what, lines)
		}
	}
}

func (r *outsideRig) viewers(n int) []string {
	r.t.Helper()
	return r.untilClients(fmt.Sprintf("want %d viewers", n), func(l []string) bool { return len(l) == n })
}

func (r *outsideRig) window() string {
	r.t.Helper()
	return r.run("display-message", "-p", "-t", "=acme-outside:0.0", "#{window_width}x#{window_height}")
}

func (r *outsideRig) pane() string {
	r.t.Helper()
	return r.run("capture-pane", "-p", "-t", "=acme-outside:0.0")
}

func (r *outsideRig) patchHost(enabled bool) {
	r.t.Helper()
	resp := r.configurer.Do("PUT", "/api/hosts/"+r.hostID, map[string]any{"show_all_tmux_sessions": enabled})
	if resp.Status != 200 {
		r.t.Fatalf("host patch: %d %s", resp.Status, resp.Body)
	}
}

type outsideDescriptor struct {
	ID       string `json:"terminalId"`
	Template string `json:"templateId"`
	ReadOnly bool   `json:"readOnly"`
	Outside  bool   `json:"outside"`
}

func (r *outsideRig) post(host bool, name string) harness.Response {
	body := map[string]any{"sessionName": name}
	if host {
		body["hostId"] = r.hostID
	}
	return r.reader.Do("POST", "/api/tmux/views", body)
}

func outsideRefusal(t *testing.T, resp harness.Response, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &body)
	if resp.Status != status || body.Error != code {
		t.Fatalf("refusal: got %d %s, want %d %s", resp.Status, resp.Body, status, code)
	}
}

func (r *outsideRig) open(host bool) outsideDescriptor {
	r.t.Helper()
	resp := r.post(host, "acme-outside")
	if resp.Status != 201 {
		r.t.Fatalf("view create: %d %s", resp.Status, resp.Body)
	}
	var d outsideDescriptor
	resp.JSON(r.t, &d)
	if d.ID == "" || d.Template != "" || !d.ReadOnly || !d.Outside {
		r.t.Fatalf("viewer metadata: %s", resp.Body)
	}
	return d
}

func outsideFrame(t *testing.T, ws *harness.WSConn, kind, id string) map[string]any {
	t.Helper()
	for n := 0; n < 200; n++ {
		var f map[string]any
		if err := json.Unmarshal(ws.Next(15*time.Second), &f); err != nil {
			t.Fatal(err)
		}
		if f["type"] == kind && (id == "" || f["terminalId"] == id) {
			return f
		}
	}
	t.Fatalf("no %s frame for %s", kind, id)
	return nil
}

// join waits on terminal_joined, then the first terminal_output.
func (r *outsideRig) join(d outsideDescriptor) *harness.WSConn {
	r.t.Helper()
	ws := r.i.WebSocket("/ws", r.i.Credential("mounter"))
	r.t.Cleanup(ws.Close)
	ws.Send(map[string]any{"type": "join_terminal", "terminalId": d.ID})
	f := outsideFrame(r.t, ws, "terminal_joined", d.ID)
	if f["readOnly"] != true || f["outside"] != true {
		r.t.Fatalf("terminal_joined flags: %v", f)
	}
	outsideFrame(r.t, ws, "terminal_output", d.ID)
	return ws
}

func (r *outsideRig) closeView(ws *harness.WSConn, d outsideDescriptor) {
	r.t.Helper()
	ws.Send(map[string]any{"type": "terminal_close", "terminalId": d.ID})
	outsideFrame(r.t, ws, "terminal_closed", d.ID)
	r.viewers(0)
}

func (r *outsideRig) sessionLives() {
	r.t.Helper()
	r.run("has-session", "-t", "=acme-outside")
}

// owner is a control-mode client standing in for the person at the keyboard.
type outsideOwner struct {
	in   io.WriteCloser
	done chan error
	cmd  *exec.Cmd
}

func (r *outsideRig) attachOwner() *outsideOwner {
	r.t.Helper()
	cmd := exec.Command(r.tmux, "-S", r.socket, "-C", "attach", "-t", "=acme-outside")
	cmd.Env = append(os.Environ(), "TMUX=")
	in, err := cmd.StdinPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	o := &outsideOwner{in: in, done: make(chan error, 1), cmd: cmd}
	go func() { o.done <- cmd.Wait() }()
	r.t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(r.run("list-clients", "-F", "#{client_control_mode}"), "1") {
		if time.Now().After(deadline) {
			r.t.Fatal("owner did not attach")
		}
	}
	return o
}

func TestOutsideTmuxView(t *testing.T) {
	t.Parallel()

	t.Run("AC1 host opt-in defaults off, console needs none", func(t *testing.T) {
		t.Parallel()
		r := outsideStart(t)
		outsideRefusal(t, r.post(true, "acme-outside"), 403, "outside_view_disabled")
		r.patchHost(false)
		outsideRefusal(t, r.post(true, "acme-outside"), 403, "outside_view_disabled")
		r.viewers(0)
		r.patchHost(true)
		d := r.open(true)
		r.closeView(r.join(d), d)
		d = r.open(false) // console: no host, no flag
		r.closeView(r.join(d), d)
		scoped := r.reader.Do("POST", "/api/tmux/views", map[string]any{"sessionName": "acme-outside"}, harness.ReqOpts{Header: g1tScope})
		if scoped.Status != 403 {
			t.Fatalf("Chief of Staff scope: %d %s", scoped.Status, scoped.Body)
		}
	})

	t.Run("AC2 exact name, read-only ignore-size, window unchanged", func(t *testing.T) {
		t.Parallel()
		r := outsideStart(t)
		r.patchHost(true)
		before := r.window()
		outsideRefusal(t, r.post(true, "acme-out"), 404, "tmux_session_not_found")
		r.viewers(0)
		d := r.open(true)
		ws := r.join(d)
		got := r.viewers(1)[0]
		if !strings.Contains(got, "read-only") || !strings.Contains(got, "ignore-size") || !strings.Contains(got, "|acme-outside|") {
			t.Fatalf("viewer client: %q", got)
		}
		if w := r.window(); w != before {
			t.Fatalf("window after attach: %s, want %s", w, before)
		}
		r.closeView(ws, d)
		if w := r.window(); w != before {
			t.Fatalf("window after close: %s, want %s", w, before)
		}
	})

	t.Run("AC3 input and resize reach nothing; owner size survives; kill refused", func(t *testing.T) {
		t.Parallel()
		r := outsideStart(t)
		d := r.open(false)
		ws := r.join(d)
		r.viewers(1)
		paneBefore, sizeBefore := r.pane(), r.window()
		ws.Send(map[string]any{"type": "terminal_input", "terminalId": d.ID, "data": base64.StdEncoding.EncodeToString([]byte("ACME_FORBIDDEN\r"))})
		outsideFrame(t, ws, "error", "") // the ordered reply shows the frame was handled
		ws.Send(map[string]any{"type": "terminal_resize", "terminalId": d.ID, "cols": 9, "rows": 5})
		outsideFrame(t, ws, "error", "")
		if p, w := r.pane(), r.window(); p != paneBefore || w != sizeBefore {
			t.Fatalf("outside session changed: pane %q -> %q, window %s -> %s", paneBefore, p, sizeBefore, w)
		}
		if data, err := os.ReadFile(r.inputLog); err == nil && len(data) > 0 {
			t.Fatalf("pane received input: %q", data)
		}
		r.closeView(ws, d)

		// An owner resizes the window; the viewer follows; the owner leaves and the size stays.
		owner := r.attachOwner()
		d = r.open(false)
		ws = r.join(d)
		if _, err := io.WriteString(owner.in, "refresh-client -C 120,50\n"); err != nil {
			t.Fatal(err)
		}
		r.untilClients("viewer matches the owner's new size", func(l []string) bool {
			return len(l) == 1 && strings.Contains(l[0], "|120|")
		})
		if w := r.window(); w != "120x50" {
			t.Fatalf("owner resize: window %s, want 120x50", w)
		}
		_ = owner.in.Close()
		select {
		case <-owner.done:
		case <-time.After(15 * time.Second):
			t.Fatal("owner did not detach")
		}
		if w := r.window(); w != "120x50" {
			t.Fatalf("window after owner detach: %s, want 120x50", w)
		}
		r.closeView(ws, d)

		p := g1tCreate(t, r.i, map[string]any{"name": "Acme hosted", "host_id": r.hostID, "path": filepath.Dir(r.socket)})
		kill := r.configurer.Do("DELETE", "/api/projects/"+p.ID+"/persistent-sessions/acme-outside", nil)
		if kill.Status/100 == 2 {
			t.Fatalf("kill of an outside name: %d %s", kill.Status, kill.Body)
		}
		r.sessionLives()
	})

	t.Run("AC4 tmux before 3.2 gives 409 and starts no viewer", func(t *testing.T) {
		t.Parallel()
		r := outsideStart(t)
		r.patchHost(true)
		r.installWrapper("old")
		outsideRefusal(t, r.post(true, "acme-outside"), 409, "tmux_view_unsupported")
		r.viewers(0)
		data, _ := os.ReadFile(r.callLog)
		for _, line := range strings.Split(string(data), "\n") {
			if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "attach") {
				t.Fatalf("unsupported tmux still launched: %s", line)
			}
		}
	})

	t.Run("AC5 close and last disconnect only detach", func(t *testing.T) {
		t.Parallel()
		r := outsideStart(t)
		d := r.open(false)
		r.closeView(r.join(d), d)
		r.sessionLives()
		d = r.open(false)
		ws := r.join(d)
		r.viewers(1)
		ws.Close()
		r.viewers(0)
		r.sessionLives()
	})
}
