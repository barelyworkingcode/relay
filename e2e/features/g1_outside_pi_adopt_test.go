package features

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

type paFake struct {
	t              *testing.T
	cmd            *exec.Cmd
	in             io.WriteCloser
	lines          chan map[string]any
	exit           chan struct{}
	dir, sid, file string
	start          time.Time
	child          int
	stopped        time.Time
}

type paRig struct {
	*osRig
	root, project, model string
	bin                  string
	operatorPath         string
}

func paStart(t *testing.T, extra map[string]any, mut ...func(*harness.Options)) *paRig {
	t.Helper()
	r := osStart(t, false, append([]func(*harness.Options){func(o *harness.Options) {
		o.FakeModelHost = fakeEchoModels()
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		o.Settings["audit"] = json.RawMessage(`{"enabled":true}`)
		o.Settings["terminal_templates"] = json.RawMessage(`[{"id":"pi","name":"Pi","command":"~/.local/bin/pi","sandbox":true,"read_write":["~/.pi","~/.cache","~/Library/Caches"],"read":["~/.bun/bin","~/.bun/install/global","~/.local/bin","/opt/homebrew"]}]`)
	}}, mut...)...)
	r.i.WaitModelHost(g1sHostDeadline)
	_, ids := modelIDs(t, r.i)
	model := ""
	for _, id := range ids {
		if strings.HasPrefix(id, "pi/") && strings.HasSuffix(id, "/fake-echo") {
			model = id
		}
	}
	if model == "" {
		t.Fatalf("no Pi fake-echo in model catalogue: %v", ids)
	}
	body := map[string]any{"allowed_templates": []string{"pi"}}
	for k, v := range extra {
		body[k] = v
	}
	p := g1tProject(t, r.i, "acme-pi", body)
	root, err := filepath.EvalSymlinks(g1tDir(r.i, "acme-pi"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fakeoutsidepi")
	build := exec.Command("go", "build", "-o", bin, "./fakes/fakeoutsidepi")
	build.Dir = filepath.Join(harness.RepoRoot(), "e2e")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build outside fixture: %v\n%s", err, out)
	}
	operatorPath := ""
	for _, call := range r.i.FakeAgentCalls("pi") {
		for _, env := range call.Environ {
			if value, ok := strings.CutPrefix(env, "PATH="); ok {
				operatorPath = value
			}
		}
	}
	if operatorPath == "" {
		t.Fatal("Pi catalogue probe recorded no operator PATH")
	}
	return &paRig{osRig: r, root: root, project: p.ID, model: model, bin: bin, operatorPath: operatorPath}
}

func (r *paRig) fake(name string, options ...string) *paFake {
	r.t.Helper()
	dir := osDir(r.t, r.root, name)
	cmd := exec.Command(r.bin, append([]string{filepath.Join(r.i.Home, ".local", "bin", "outside-pi", name)}, options...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+r.i.Home, "PATH="+r.operatorPath)
	in, err := cmd.StdinPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		r.t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	f := &paFake{t: r.t, cmd: cmd, in: in, lines: make(chan map[string]any, 32), exit: make(chan struct{}), dir: dir, sid: adUUID()}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(f.lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) == nil {
				f.lines <- v
			}
		}
	}()
	// Wait closes the stdout pipe once the process exits, so it runs only after
	// the reader has drained to EOF; otherwise a last line (the SIGTERM
	// receipt) can be lost under load.
	go func() { <-drained; _ = cmd.Wait(); close(f.exit) }()
	r.t.Cleanup(func() {
		if f.child > 0 {
			_ = syscall.Kill(f.child, syscall.SIGKILL)
			g11dUntil(r.t, "outside working child exit", func() bool { return !g11dAlive(f.child) })
		}
		_ = cmd.Process.Kill()
		<-f.exit
		_ = in.Close()
	})
	f.next("ready")
	_, f.start = adLstart(r.t, cmd.Process.Pid)
	encoded := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(strings.TrimPrefix(dir, "/"))
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(f.start.UTC().Format("2006-01-02T15:04:05.000Z"))
	f.file = filepath.Join(r.i.Home, ".pi", "agent", "sessions", "--"+encoded+"--", stamp+"_"+f.sid+".jsonl")
	return f
}

func (f *paFake) next(event string) map[string]any {
	f.t.Helper()
	select {
	case v, ok := <-f.lines:
		if !ok || v["event"] != event {
			f.t.Fatalf("outside event %v, want %s", v, event)
		}
		if event == "child_ready" {
			f.child = int(v["pid"].(float64))
		}
		if event == "child_exit" {
			f.child = 0
		}
		return v
	case <-time.After(osBound):
		f.t.Fatalf("no outside %s completion", event)
		return nil
	}
}

func (f *paFake) untouched() {
	f.t.Helper()
	// The response closes the refusal window; the fixture barrier flushes its
	// signal handler's stdout before we check that it stayed alive.
	if _, err := fmt.Fprintln(f.in, "barrier"); err != nil {
		f.t.Fatal(err)
	}
	f.next("barrier")
	select {
	case <-f.exit:
		f.t.Fatal("refused adoption stopped outside Pi")
	default:
	}
}

func paJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func (f *paFake) message(id, parent, role, stop, text string) string {
	m := map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}, "timestamp": f.start.UnixMilli(), "provider": "fake", "model": "fake-echo", "api": "openai-completions"}
	if stop != "" {
		m["stopReason"] = stop
	}
	var p any = parent
	if parent == "" {
		p = nil
	}
	return paJSON(map[string]any{"type": "message", "id": id, "parentId": p, "timestamp": f.start.UTC().Format(time.RFC3339Nano), "message": m})
}
func (f *paFake) write(tail ...string) []byte {
	f.t.Helper()
	h := paJSON(map[string]any{"type": "session", "version": 3, "id": f.sid, "cwd": f.dir, "timestamp": f.start.UTC().Format(time.RFC3339Nano)})
	raw := []byte(strings.Join(append([]string{h}, tail...), "\n") + "\n")
	osWrite(f.t, f.file, raw)
	return raw
}
func (f *paFake) idle() []byte {
	return f.write(f.message("11111111", "", "user", "", "Acme first turn"), f.message("22222222", "11111111", "assistant", "stop", "Acme complete"))
}

func (r *paRig) rows() []map[string]any {
	r.t.Helper()
	_, raw := r.probeRaw()
	var s struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		r.t.Fatal(err)
	}
	return s.Sessions
}
func (r *paRig) row(f *paFake) map[string]any {
	r.t.Helper()
	for _, row := range r.rows() {
		if row["agent"] == "pi" && row["conversationId"] == f.sid {
			return row
		}
	}
	sub := osSubscribe(r.t, r.i)
	defer sub.ws.Close()
	var found map[string]any
	sub.until(osBound, func(osSnap) bool {
		var snapshot struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if err := json.Unmarshal(sub.last, &snapshot); err != nil {
			r.t.Fatal(err)
		}
		for _, row := range snapshot.Sessions {
			if row["agent"] == "pi" && row["conversationId"] == f.sid {
				found = row
				return true
			}
		}
		return false
	})
	return found
}
func (r *paRig) adopt(id string, interrupt bool) harness.Response {
	return r.osRig.adopt(map[string]any{"outsideId": id, "model": r.model, "interrupt": interrupt})
}
func (r *paRig) adopted(f *paFake) string {
	r.t.Helper()
	row := r.row(f)
	if row["adoptable"] != true {
		r.t.Fatalf("idle row not adoptable: %v", row)
	}
	resp := r.adopt(row["id"].(string), false)
	var v struct {
		SessionID, Agent, ConversationID string
		Interrupted, RecoverySent        bool
	}
	resp.JSON(r.t, &v)
	if resp.Status != 201 || v.SessionID == "" || v.Agent != "pi" || v.ConversationID != f.sid || v.Interrupted || v.RecoverySent {
		r.t.Fatalf("adopt response %d %s", resp.Status, resp.Body)
	}
	signal := f.next("signal")
	if signal["signal"] != "SIGTERM" {
		r.t.Fatalf("idle takeover signal: %v", signal)
	}
	select {
	case <-f.exit:
	case <-time.After(osBound):
		r.t.Fatal("terminal Pi did not exit")
	}
	var err error
	f.stopped, err = time.Parse(time.RFC3339Nano, signal["at"].(string))
	if err != nil {
		r.t.Fatal(err)
	}
	r.i.MustCLI("session", "wait", "--id", v.SessionID, "--json")
	g1cKillRuns(r.t, r.i, v.SessionID)
	return v.SessionID
}

func TestPiAdoptInventoryIdentity(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("inventory")
	f.idle()
	row := r.row(f)
	if id, _ := row["id"].(string); !regexp.MustCompile(`^outside:[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("invalid outside ID: %v", row)
	}
	if row["outside"] != true || row["readOnly"] != true || row["adoptable"] != true || row["projectId"] != r.project {
		t.Fatalf("Pi row fields: %v", row)
	}
	adNoKeys(t, "Pi row", row, "claudeSessionId", "pid", "procStart", "argv", "environment")
	same := adSpawn(t, osDir(t, r.root, "claude"), f.sid)
	same.idle(r.i.Home, adModelSn)
	s := r.probe()
	if c := s.liveAt(same.dir); c == nil || c.ID == row["id"] {
		t.Fatalf("same UUID collided across agents: %+v, %v", c, row)
	}
	detail := r.detail(row["id"].(string))
	if detail.Status != 200 {
		t.Fatalf("Pi detail: %d %s", detail.Status, detail.Body)
	}
	page := r.transcriptPage(row["id"].(string), "")
	if got := otTexts(page.Entries); len(got) != 2 || got[0] != "Acme first turn" || got[1] != "Acme complete" {
		t.Fatalf("Pi normalized transcript: %q", got)
	}
}

func TestPiAdoptSameFileResume(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("continuity")
	before := f.idle()
	old, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	sid := r.adopted(f)
	audited := false
	for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_adopt"}) {
		args, _ := row["args"].(map[string]any)
		if row["phase"] != "intent" || args["agent"] != "pi" || args["conversation_id"] != f.sid {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, row["ts"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if at.After(f.stopped) {
			t.Fatal("adoption audit intent follows terminal signal")
		}
		audited = true
	}
	if !audited {
		t.Fatal("missing Pi adoption intent with conversation identity")
	}

	after, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("idle adoption changed the transcript or sent a recovery turn")
	}
	starts := 0
	for _, c := range r.i.FakeAgentCalls("pi") {
		if c.Event == "start" && adHasFlag(c.Argv, "--mode", "rpc") {
			starts++
			if !adHasFlag(c.Argv, "--session", f.file) || !adHasFlag(c.Argv, "--session-dir", filepath.Dir(f.file)) {
				t.Fatalf("resume argv: %v", c.Argv)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("managed RPC starts = %d, want one", starts)
	}
	g1sSendText(t, r.i, sid, "Acme follow up", "echo: Acme follow up")
	r.i.MustCLI("session", "wait", "--id", sid, "--json")
	after, err = os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(old, now) || !bytes.HasPrefix(after, before) {
		t.Fatal("resume replaced the original file or rewrote its prefix")
	}
	var prompts int
	for _, line := range bytes.Split(after[len(before):], []byte("\n")) {
		var e struct {
			Message struct {
				Role    string
				Content any
			}
		}
		if json.Unmarshal(line, &e) == nil && e.Message.Role == "user" {
			prompts++
			if e.Message.Content != "Acme follow up" {
				t.Fatalf("unexpected appended user message %v", e.Message.Content)
			}
		}
	}
	if prompts != 1 {
		t.Fatalf("appended %d follow-up prompts, want one", prompts)
	}
	for _, row := range r.rows() {
		if row["agent"] == "pi" && row["conversationId"] == f.sid {
			t.Fatal("managed Pi still listed outside")
		}
	}
}

func TestPiAdoptBusyRefused(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"user", "toolUse", "error", "length"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			r := paStart(t, nil)
			f := r.fake("busy")
			last := f.message("22222222", "11111111", "assistant", state, "Acme unfinished")
			if state == "user" {
				last = f.message("22222222", "11111111", "user", "", "Acme working")
			}
			if state == "toolUse" {
				last = paJSON(map[string]any{"type": "message", "id": "22222222", "parentId": "11111111", "timestamp": f.start.UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "toolCall", "id": "acme-call", "name": "bash", "arguments": map[string]any{"command": "true"}}}}})
			}
			f.write(f.message("11111111", "", "user", "", "Acme first"), last)
			row := r.row(f)
			if row["adoptable"] == true {
				t.Fatalf("unfinished Pi is adoptable: %v", row)
			}
			otRefusal(t, r.adopt(row["id"].(string), true), 409, "busy")
			f.untouched()
		})
	}
}

func TestPiAdoptNoFirstTurn(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	absent := r.fake("absent")
	header := r.fake("header")
	header.write()
	for _, row := range r.rows() {
		if row["directory"] == absent.dir || row["directory"] == header.dir {
			t.Fatalf("Pi without a first turn listed: %v", row)
		}
	}
	absent.untouched()
	header.untouched()
	// Positive control: the same header-only transcript lists once it holds a
	// first turn, so the absence above is the missing turn and not the rig.
	header.idle()
	if row := r.row(header); row["directory"] != header.dir {
		t.Fatalf("Pi with a first turn not listed: %v", row)
	}
}

func TestPiAdoptAmbiguousRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"file", "process"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := paStart(t, nil)
			f := r.fake("ambiguous")
			f.idle()
			id := r.row(f)["id"].(string)
			if kind == "file" {
				original := f.file
				f.start = f.start.Add(time.Second)
				f.file = filepath.Join(filepath.Dir(original), strings.NewReplacer(":", "-", ".", "-").Replace(f.start.UTC().Format("2006-01-02T15:04:05.000Z"))+"_"+f.sid+".jsonl")
				f.idle()
				f.file = original
			} else {
				other := r.fake("other")
				if _, err := fmt.Fprintln(other.in, "cwd "+f.dir); err != nil {
					t.Fatal(err)
				}
				other.next("cwd")
				// A fresh header within both process lifetimes keeps both matches valid.
				if err := os.Remove(f.file); err != nil {
					t.Fatal(err)
				}
				f.start = other.start
				stamp := strings.NewReplacer(":", "-", ".", "-").Replace(f.start.UTC().Format("2006-01-02T15:04:05.000Z"))
				f.file = filepath.Join(filepath.Dir(f.file), stamp+"_"+f.sid+".jsonl")
				f.idle()
				defer other.untouched()
			}
			resp := r.adopt(id, false)
			otRefusal(t, resp, 409, "ambiguous")
			f.untouched()
		})
	}
}

func TestPiAdoptStaleEvidenceRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"cwd", "start", "file-link", "root-link", "partial", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := paStart(t, nil)
			f := r.fake("evidence")
			data := f.idle()
			id := r.row(f)["id"].(string)
			switch kind {
			case "cwd":
				if _, err := fmt.Fprintln(f.in, "cwd "+r.root); err != nil {
					t.Fatal(err)
				}
				f.next("cwd")
			case "start":
				var h map[string]any
				lines := bytes.Split(data, []byte("\n"))
				_ = json.Unmarshal(lines[0], &h)
				// The header time and the file name move together, so the
				// transcript stays well formed and only the process start
				// window can refuse it.
				shifted := f.start.Add(-time.Hour).UTC()
				h["timestamp"] = shifted.Format(time.RFC3339Nano)
				lines[0] = []byte(paJSON(h))
				if err := os.Remove(f.file); err != nil {
					t.Fatal(err)
				}
				f.file = filepath.Join(filepath.Dir(f.file), strings.NewReplacer(":", "-", ".", "-").Replace(shifted.Format("2006-01-02T15:04:05.000Z"))+"_"+f.sid+".jsonl")
				osWrite(t, f.file, bytes.Join(lines, []byte("\n")))
			case "partial":
				osWrite(t, f.file, append(data, []byte(`{"type":"message"`)...))
			case "malformed":
				osWrite(t, f.file, append(data, []byte("invalid json\n")...))
			case "file-link":
				target := filepath.Join(r.i.Home, "acme-transcript.jsonl")
				osWrite(t, target, data)
				if err := os.Remove(f.file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, f.file); err != nil {
					t.Fatal(err)
				}
			case "root-link":
				root := filepath.Join(r.i.Home, ".pi", "agent", "sessions")
				target := root + "-saved"
				if err := os.Rename(root, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			}
			resp := r.adopt(id, true)
			if kind == "root-link" {
				// An unreadable session root prevents a fresh registry query;
				// identified candidates with changed evidence still use 409.
				otRefusal(t, resp, 502, "unavailable")
			} else if resp.Status != 409 {
				t.Fatalf("changed %s evidence answered %d: %s", kind, resp.Status, resp.Body)
			}
			f.untouched()
		})
	}
}

func paAuditOff(t *testing.T, i *harness.Instance) {
	t.Helper()
	i.Stop()
	path := filepath.Join(i.ConfigDir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]json.RawMessage
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	s["audit"] = json.RawMessage(`{"enabled":false}`)
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	osWrite(t, path, data)
	i.Restart()
	i.WaitSessionHost(g1sHostDeadline)
}

func TestPiAdoptAuthorizationAndAuditRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"audit-off", "model-policy", "transcript-grant"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			extra := map[string]any{}
			var mut []func(*harness.Options)
			if kind == "model-policy" {
				extra["allowed_models"] = []string{"pi/fake/other"}
			}
			if kind == "transcript-grant" {
				mut = append(mut, func(o *harness.Options) {
					o.Settings["terminal_templates"] = json.RawMessage(`[{"id":"pi","name":"Pi","command":"~/.local/bin/pi","sandbox":true,"read_write":["~/.pi"],"deny":["~/.pi/agent/sessions"]}]`)
				})
			}
			r := paStart(t, extra, mut...)
			f := r.fake("refusal")
			before := f.idle()
			id := r.row(f)["id"].(string)
			status := 403
			code := ""
			if kind == "audit-off" {
				paAuditOff(t, r.i)
				status = 503
				code = "audit_unavailable"
			}
			if kind == "transcript-grant" {
				status = 409
				code = "transcript_not_granted"
			}
			resp := r.adopt(id, false)
			if code != "" {
				otRefusal(t, resp, status, code)
			} else if resp.Status != status {
				t.Fatalf("model policy refusal: %d %s", resp.Status, resp.Body)
			}
			f.untouched()
			after, err := os.ReadFile(f.file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("refused adoption changed transcript")
			}
			if got := adSessions(t, r.i); len(got) != 0 {
				t.Fatalf("refused adoption created sessions: %v", got)
			}
		})
	}
}

func paHold(t *testing.T, r *paRig, sid string) *harness.WSConn {
	t.Helper()
	return paHoldNth(t, r, sid, 1)
}

// paHoldNth starts a held turn and waits for the nth hold receipt, so a
// second turn on a restarted instance counts the receipts of the first.
func paHoldNth(t *testing.T, r *paRig, sid string, nth int) *harness.WSConn {
	t.Helper()
	ws := r.i.WebSocket("/ws", r.i.Credential("cos"))
	t.Cleanup(ws.Close)
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1aNext(t, ws, g1tFrameDeadline, g1aIs("session_joined", sid))
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!hold"})
	g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool {
		return v["type"] == "session_state" && v["sessionId"] == sid && v["state"] == "running"
	})
	g11dUntil(t, "Pi hold command receipt", func() bool {
		n := 0
		for _, c := range r.i.FakeAgentCalls("pi") {
			if c.Event == "pi_holding" {
				n++
			}
		}
		return n == nth
	})

	return ws
}

func TestPiAdoptManagedAbortAudited(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("interrupt")
	f.idle()
	sid := r.adopted(f)
	ws := paHold(t, r, sid)
	ws.Send(map[string]any{"type": "stop_generation", "sessionId": sid})
	g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool {
		return v["type"] == "session_state" && v["sessionId"] == sid && v["state"] == "idle"
	})
	r.i.MustCLI("session", "wait", "--id", sid, "--json")
	var receipt time.Time
	for _, call := range r.i.FakeAgentCalls("pi") {
		if call.Event == "pi_abort" && call.Text == f.sid {
			receipt = call.TS
		}
	}
	if receipt.IsZero() {
		t.Fatal("managed Pi received no RPC abort")
	}
	var found bool
	for _, row := range r.i.Audit(harness.AuditQuery{Event: "session_interrupt"}) {
		args, _ := row["args"].(map[string]any)
		if row["phase"] != "intent" || args["session_id"] != sid || args["method"] != "abort" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, row["ts"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if at.After(receipt) {
			t.Fatalf("interrupt audit intent %v follows RPC receipt %v", at, receipt)
		}
		actor, _ := row["actor"].(map[string]any)
		if actor["kind"] != "service" {
			t.Fatalf("interrupt actor: %v", actor)
		}
		found = true
	}
	if !found {
		t.Fatal("no durable session_interrupt intent with method abort and session identity")
	}
}

func paLowAudit(o *harness.Options) {
	o.Settings["audit"] = json.RawMessage(`{"enabled":true,"max_file_bytes":1,"generations":5}`)
}

func paBlockAuditWrites(t *testing.T, r *paRig) func() {
	t.Helper()
	dir := filepath.Join(r.i.ConfigDir, "logs", "audit")
	active := filepath.Join(dir, "toolcalls.jsonl")
	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 1 {
		t.Fatal("audit fault requires a regular active log above the configured rotation cap")
	}
	restored := false
	restore := func() {
		t.Helper()
		if restored {
			return
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(active, 0o600); err != nil {
			t.Fatal(err)
		}
		restored = true
	}
	t.Cleanup(restore)
	// Rotation closes the existing descriptor; denying both the file and its
	// directory prevents reopening the old file or creating a replacement.
	if err := os.Chmod(active, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(active, os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_ = f.Close()
		t.Fatal("audit fault setup allowed append-open of the active log")
	}
	if !os.IsPermission(err) {
		t.Fatalf("audit fault setup failed for a reason other than permissions: %v", err)
	}
	t.Log("audit fault ready: active log append-open refused with permission denied")
	return restore
}

func TestPiAdoptManagedAbortAuditFailureRefused(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil, paLowAudit)
	f := r.fake("interrupt-audit-failure")
	f.idle()
	sid := r.adopted(f)
	ws := paHold(t, r, sid)
	restoreAudit := paBlockAuditWrites(t, r)
	ws.Send(map[string]any{"type": "stop_generation", "sessionId": sid})
	frames := g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool { return v["type"] == "error" && v["sessionId"] == sid })
	if got := frames[len(frames)-1]["code"]; got != "audit_unavailable" {
		t.Fatalf("interrupt refusal code %v", got)
	}
	// The coded response is the completion marker for the refused handler.
	n := 0
	for _, call := range r.i.FakeAgentCalls("pi") {
		if call.Event == "pi_abort" {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("audit rejection delivered RPC abort: %d receipts", n)
	}
	list, _ := g1sListSessions(t, r.i)
	row, ok := g1sFindSession(list, sid)
	if !ok || row.Attention == nil || row.Attention.State != "running" {
		t.Fatalf("audit rejection ended running turn: %+v", row)
	}
	// The recorder keeps its first write error, so a permission fix alone does
	// not revive it: restore, then restart the instance, the recorder's
	// lifecycle boundary.
	restoreAudit()
	ws.Close()
	since := time.Now()
	r.i.Restart()
	r.i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1sHostDeadline)
	r.i.WaitSessionHost(g1sHostDeadline)
	// A managed Pi ends with its host, so the refused session is gone after the
	// restart and nothing is dormant to resume. A fresh outside Pi is adopted
	// instead, and its held turn is aborted through the revived recorder.
	r.i.WaitModelHost(g1sHostDeadline)
	f2 := r.fake("interrupt-audit-recovered")
	f2.idle()
	sid = r.adopted(f2)
	ws = paHoldNth(t, r, sid, 2)
	ws.Send(map[string]any{"type": "stop_generation", "sessionId": sid})
	g1aNext(t, ws, g1tFrameDeadline, func(v map[string]any) bool {
		return v["type"] == "session_state" && v["sessionId"] == sid && v["state"] == "idle"
	})
	r.i.MustCLI("session", "wait", "--id", sid, "--json")
	n = 0
	for _, call := range r.i.FakeAgentCalls("pi") {
		if call.Event == "pi_abort" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("abort after audit recovery delivered %d RPC abort receipts, want 1", n)
	}
}

func paTool(f *paFake, id, parent string) string {
	return paJSON(map[string]any{"type": "message", "id": id, "parentId": parent, "timestamp": f.start.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "toolCall", "id": "acme-call", "name": "bash", "arguments": map[string]any{"command": "true"}}}}})
}

func TestPiAdoptSelectedLeafReadiness(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"abandoned-tool", "selected-user", "unresolved-ancestor"} {
		t.Run(branch, func(t *testing.T) {
			t.Parallel()
			r := paStart(t, nil)
			f := r.fake("branches")
			entries := []string{f.message("11111111", "", "user", "", "Acme first"), paTool(f, "22222222", "11111111")}
			parent := "11111111"
			if branch == "unresolved-ancestor" {
				parent = "22222222"
			}
			entries = append(entries, f.message("33333333", parent, "assistant", "stop", "Acme branch complete"))
			if branch == "selected-user" {
				entries = append(entries, f.message("44444444", "11111111", "user", "", "Acme selected work"))
			}
			f.write(entries...)
			row := r.row(f)
			if branch == "abandoned-tool" {
				if row["adoptable"] != true {
					t.Fatalf("abandoned branch vetoed idle selected leaf: %v", row)
				}
				r.adopted(f)
			} else {
				if row["adoptable"] == true {
					t.Fatalf("selected busy ancestry is adoptable: %v", row)
				}
				otRefusal(t, r.adopt(row["id"].(string), true), 409, "busy")
				f.untouched()
			}
		})
	}
}

func TestPiAdoptWorkingChildRefused(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("working-child")
	f.idle()
	id := r.row(f)["id"].(string)
	if _, err := fmt.Fprintln(f.in, "child"); err != nil {
		t.Fatal(err)
	}
	f.next("child_ready")
	otRefusal(t, r.adopt(id, true), 409, "busy")
	f.untouched()
	if !g11dAlive(f.child) {
		t.Fatal("refused adoption stopped the working child")
	}
	if _, err := fmt.Fprintln(f.in, "stop-child"); err != nil {
		t.Fatal(err)
	}
	f.next("child_exit")
	// The child-exit completion closes the veto window; the same unchanged
	// transcript can now be adopted without a blanket child-process exemption.
	r.adopted(f)
}

func TestPiAdoptFailedSuccessorPreservesTranscript(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("failed-successor")
	before := f.idle()
	original, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	id := r.row(f)["id"].(string)
	osWrite(t, f.file+".fail-start", []byte("Acme startup failure\n"))
	otRefusal(t, r.adopt(id, false), 502, "launch_failed")
	if signal := f.next("signal"); signal["signal"] != "SIGTERM" {
		t.Fatalf("outside takeover signal: %v", signal)
	}
	select {
	case <-f.exit:
	case <-time.After(osBound):
		t.Fatal("outside Pi did not exit before successor failure")
	}
	var failed bool
	for _, c := range r.i.FakeAgentCalls("pi") {
		if c.Event == "pi_start_failed" && c.Text == f.file {
			failed = true
		}
	}
	if !failed {
		t.Fatal("refusal did not exercise successor startup failure")
	}
	after, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(original, current) {
		t.Fatal("failed successor altered or replaced the original transcript")
	}
}

func TestPiAdoptDeleteOnlyExactFile(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil)
	f := r.fake("exact-delete")
	data := f.idle()
	sid := r.adopted(f)
	// Same-UUID siblings are planted after adoption so they do not make the
	// pre-takeover correlation ambiguous.
	same := filepath.Join(filepath.Dir(f.file), "2000-01-01T00-00-00-000Z_"+f.sid+".jsonl")
	other := filepath.Join(filepath.Dir(f.file), "2000-01-01T00-00-00-000Z_"+adUUID()+".jsonl")
	osWrite(t, same, data)
	osWrite(t, other, []byte("Acme unrelated transcript\n"))
	siblings := map[string][]byte{}
	for _, path := range []string{same, other} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		siblings[path] = raw
	}
	resp := r.i.SocketHTTP(r.i.Credential("cos")).Do("DELETE", "/api/sessions/"+sid, nil)
	if resp.Status != 204 {
		t.Fatalf("managed delete answered %d: %s", resp.Status, resp.Body)
	}
	if _, err := os.Lstat(f.file); !os.IsNotExist(err) {
		t.Fatalf("exact adopted transcript survives deletion: %v", err)
	}
	for path, before := range siblings {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("delete removed sibling: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("delete altered sibling transcript")
		}
	}
}

func TestPiAdoptAuditWriteFailureRefused(t *testing.T) {
	t.Parallel()
	r := paStart(t, nil, paLowAudit)
	f := r.fake("audit-write-failure")
	before := f.idle()
	original, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	id := r.row(f)["id"].(string)
	paBlockAuditWrites(t, r)
	otRefusal(t, r.adopt(id, false), 503, "audit_unavailable")
	f.untouched()
	after, err := os.ReadFile(f.file)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(original, current) {
		t.Fatal("audit-write rejection altered the outside transcript")
	}
	for _, c := range r.i.FakeAgentCalls("pi") {
		if c.Event == "start" && adHasFlag(c.Argv, "--mode", "rpc") {
			t.Fatal("audit-write rejection launched managed Pi")
		}
	}
}

func TestPiAdoptTitleErasedIdentity(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"absolute-wrapper", "env-node", "compound-wrapper", "wrong-runtime"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r := paStart(t, nil)
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			node, err = filepath.EvalSymlinks(node)
			if err != nil {
				t.Fatal(err)
			}
			copied := filepath.Join(r.i.Home, ".local", "bin", "runtime", "bin", "node")
			data, err := os.ReadFile(node)
			if err != nil {
				t.Fatal(err)
			}
			osWrite(t, copied, data)
			if err := os.Chmod(copied, 0o755); err != nil {
				t.Fatal(err)
			}
			// Preserve Node's observed @loader_path/../lib runtime layout;
			// only the executable is copied, leaving installed libraries intact.
			lib := filepath.Join(filepath.Dir(filepath.Dir(node)), "lib")
			if info, err := os.Stat(lib); err == nil && info.IsDir() {
				if err := os.Symlink(lib, filepath.Join(r.i.Home, ".local", "bin", "runtime", "lib")); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.Stat(node)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := os.Stat(copied)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(original, selected) {
				t.Fatal("runtime fixture must use distinct executable files")
			}
			if out, err := exec.Command(copied, "--version").CombinedOutput(); err != nil {
				t.Fatalf("copied Node startup: %v: %s", err, out)
			}
			operatorBin := filepath.Join(r.i.Home, ".local", "bin")
			if err := os.Symlink(node, filepath.Join(operatorBin, "node")); err != nil {
				t.Fatal(err)
			}
			r.operatorPath = strings.Join([]string{operatorBin, "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, string(os.PathListSeparator))
			options := []string{"--title"}
			if kind != "env-node" {
				options = append(options, "--node="+copied)
			}
			f := r.fake("titled", options...)
			f.idle()
			entry := filepath.Join(r.i.Home, ".local", "bin", "outside-pi", "titled", "bin", "pi")
			target, err := filepath.EvalSymlinks(entry)
			if err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(r.i.Home, ".local", "bin", "operator-pi")
			command := entry
			if kind != "env-node" {
				text := "#!/bin/sh\nPI_SKIP_VERSION_CHECK=\"1\" exec \"" + target + "\" \"$@\"\n"
				if kind == "compound-wrapper" {
					text = "#!/bin/sh\nprintf acme >/dev/null\n" + strings.TrimPrefix(text, "#!/bin/sh\n")
				}
				osWrite(t, wrapper, []byte(text))
				if err := os.Chmod(wrapper, 0o755); err != nil {
					t.Fatal(err)
				}
				command = wrapper
			}
			if kind == "wrong-runtime" {
				raw, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				osWrite(t, target, bytes.Replace(raw, []byte("#!"+copied), []byte("#!"+node), 1))
				if err := os.Chmod(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			r.i.Stop()
			settings := filepath.Join(r.i.ConfigDir, "settings.json")
			raw, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]json.RawMessage
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			var templates []map[string]any
			if err := json.Unmarshal(config["terminal_templates"], &templates); err != nil {
				t.Fatal(err)
			}
			changed := false
			for _, template := range templates {
				if template["id"] == "pi" {
					template["command"] = command
					changed = true
				}
			}
			if !changed {
				t.Fatal("no configured Pi template")
			}
			config["terminal_templates"] = json.RawMessage(paJSON(templates))
			osWrite(t, settings, []byte(paJSON(config)))
			r.i.Restart()
			r.i.WaitSessionHost(g1sHostDeadline)
			r.i.WaitModelHost(g1sHostDeadline)
			if kind == "compound-wrapper" || kind == "wrong-runtime" {
				for _, row := range r.rows() {
					if row["agent"] == "pi" && row["conversationId"] == f.sid {
						t.Fatalf("untrusted runtime published Pi row: %v", row)
					}
				}
				f.untouched()
			} else {
				sid := r.adopted(f)
				g1sSendText(t, r.i, sid, "Acme title continuation", "echo: Acme title continuation")
			}
		})
	}
}
