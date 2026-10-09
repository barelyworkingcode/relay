package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Call is one line of a fake's call log.
type Call struct {
	TS           time.Time       `json:"ts"`
	Transport    string          `json:"transport"`
	Method       string          `json:"method"`
	ID           json.RawMessage `json:"id"`
	Params       json.RawMessage `json:"params"`
	ParamsSHA256 string          `json:"params_sha256"`
	Auth         string          `json:"auth"` // "none" | "bearer:<sha256 hex of the token>"
}

// AgentCall is one line of a fake agent CLI's call log.
type AgentCall struct {
	TS             time.Time `json:"ts"`
	Persona, Event string
	Argv           []string `json:"argv"`
	Text           string   `json:"text"`
	LineSHA256     string   `json:"line_sha256"`
}

type fakeMCP struct {
	spec    FakeMCPSpec
	proc    *Proc // http fakes only; relay starts stdio fakes itself
	url     string
	logPath string
}

func (i *Instance) fakePath(name string) string { return filepath.Join(i.Dir, "fakes", name) }

// startFakeMCP writes the fake's catalogue, starts it when it speaks HTTP and
// returns the external_mcps record that attaches it.
func (i *Instance) startFakeMCP(spec FakeMCPSpec) json.RawMessage {
	t := i.t
	t.Helper()
	if spec.ID == "" {
		t.Fatalf("FakeMCPSpec needs an ID")
	}
	cat, err := json.Marshal(spec.Catalogue)
	if err != nil {
		t.Fatalf("encoding the catalogue of fake MCP %s: %v", spec.ID, err)
	}
	catPath := i.fakePath(spec.ID + ".catalogue.json")
	if err := os.WriteFile(catPath, cat, 0o600); err != nil {
		t.Fatalf("writing %s: %v", catPath, err)
	}
	f := &fakeMCP{spec: spec, logPath: i.fakePath(spec.ID + ".calls.jsonl")}
	i.fakeMCPs = append(i.fakeMCPs, f)
	args := []string{"--catalogue", catPath, "--call-log", f.logPath}
	rec := map[string]any{"id": spec.ID, "display_name": "Fake MCP " + spec.ID, "env": map[string]string{}}
	switch spec.Transport {
	case "stdio":
		rec["transport"] = "stdio"
		rec["command"] = bundle.FakeMCP
		rec["args"] = append(args, "--transport", "stdio")
	case "http":
		args = append(args, "--transport", "http", "--listen", "127.0.0.1:0")
		if spec.OAuth {
			args = append(args, "--oauth")
		}
		f.proc = startProc(t, procSpec{bin: bundle.FakeMCP, args: args, env: i.env, dir: i.Dir})
		f.url = strings.TrimSpace(string(f.proc.FirstLine(30 * time.Second)))
		rec["transport"] = "http"
		rec["args"] = []string{}
		rec["url"] = f.url
	default:
		t.Fatalf("fake MCP %s: Transport must be \"stdio\" or \"http\", got %q", spec.ID, spec.Transport)
	}
	raw, _ := json.Marshal(rec)
	return raw
}

func (i *Instance) modelHostRecord(h *FakeModelHostSpec) json.RawMessage {
	t := i.t
	t.Helper()
	if h.ID == "" {
		t.Fatalf("FakeModelHostSpec needs an ID")
	}
	models := h.Models
	if models == nil {
		models = []json.RawMessage{}
	}
	data, _ := json.Marshal(models)
	modelsPath := i.fakePath(h.ID + ".models.json")
	if err := os.WriteFile(modelsPath, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", modelsPath, err)
	}
	i.hostLogPath = i.fakePath(h.ID + ".calls.jsonl")
	rec := map[string]any{
		"id": h.ID, "display_name": "Fake model host " + h.ID,
		"command": bundle.FakeModelHost,
		"args":    []string{"--models", modelsPath, "--call-log", i.hostLogPath},
		// The fake binds $TMPDIR/fmh-<pid>.sock, so each instance hands it its
		// own short TMPDIR; a socket path over 103 bytes cannot bind.
		"env":          map[string]string{"TMPDIR": strings.TrimRight(i.Tmp, "/") + "/"},
		"autostart":    true,
		"capabilities": []string{"model_host"},
	}
	raw, _ := json.Marshal(rec)
	return raw
}

func (i *Instance) stopFakes() {
	for _, f := range i.fakeMCPs {
		if f.proc == nil {
			continue
		}
		select {
		case <-f.proc.done:
		default:
			f.proc.cmd.Process.Signal(syscall.SIGTERM)
			tm := time.NewTimer(10 * time.Second)
			select {
			case <-f.proc.done:
			case <-tm.C:
				f.proc.cmd.Process.Kill()
				<-f.proc.done
			}
			tm.Stop()
		}
	}
}

func (i *Instance) findFake(id string) *fakeMCP {
	i.t.Helper()
	for _, f := range i.fakeMCPs {
		if f.spec.ID == id {
			return f
		}
	}
	i.t.Fatalf("no fake MCP %q was attached", id)
	return nil
}

// FakeMCPURL is the endpoint URL of an HTTP fake MCP.
func (i *Instance) FakeMCPURL(id string) string {
	i.t.Helper()
	f := i.findFake(id)
	if f.url == "" {
		i.t.Fatalf("fake MCP %q is not an HTTP fake", id)
	}
	return f.url
}

func readJSONL[T any](t interface{ Fatalf(string, ...any) }, path string) []T {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("decoding a line of %s: %v\n%s", path, err, sc.Text())
		}
		out = append(out, v)
	}
	return out
}

// FakeMCPCalls reads the fake's call log. A line is written before the fake
// replies, so a read after the reply needs no wait.
func (i *Instance) FakeMCPCalls(id string) []Call {
	i.t.Helper()
	return readJSONL[Call](i.t, i.findFake(id).logPath)
}

// FakeModelHostCalls reads the fake model host's call log.
func (i *Instance) FakeModelHostCalls() []Call {
	i.t.Helper()
	if i.hostLogPath == "" {
		i.t.Fatalf("no fake model host was attached")
	}
	return readJSONL[Call](i.t, i.hostLogPath)
}

// FakeAgentCalls reads the call log a fake agent persona (claude, pi or codex)
// wrote in the session's temp dir under the instance's TMPDIR.
func (i *Instance) FakeAgentCalls(persona string) []AgentCall {
	i.t.Helper()
	name := "fakeagent-" + persona + ".jsonl"
	var out []AgentCall
	filepath.WalkDir(i.Tmp, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			out = append(out, readJSONL[AgentCall](i.t, path)...)
		}
		return nil
	})
	return out
}

// WaitSessionHost blocks until the session host has registered its manifest.
func (i *Instance) WaitSessionHost(deadline time.Duration) {
	i.t.Helper()
	i.WaitEvent(EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, deadline)
}

// WaitModelHost blocks until the fake model host has registered with relay.
func (i *Instance) WaitModelHost(deadline time.Duration) {
	i.t.Helper()
	if i.modelHost == nil {
		i.t.Fatalf("no fake model host was attached")
	}
	i.WaitEvent(EventQuery{Key: "model.host.register", Fields: map[string]any{"service_id": i.modelHost.ID, "status": "ok"}}, deadline)
}

// CompleteOAuth plays the browser for an authorization URL: it fetches the URL
// without following redirects, then fetches the Location, which is relay's
// loopback callback.
func (i *Instance) CompleteOAuth(authorizationURL string) {
	t := i.t
	t.Helper()
	hc := &http.Client{
		Timeout:       requestDeadline,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Get(authorizationURL)
	if err != nil {
		t.Fatalf("GET the authorization URL: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		t.Fatalf("the authorization URL answered %d with Location %q, expected a redirect", resp.StatusCode, loc)
	}
	base, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parsing the authorization URL: %v", err)
	}
	target, err := base.Parse(loc)
	if err != nil {
		t.Fatalf("parsing the redirect Location %q: %v", loc, err)
	}
	cb, err := (&http.Client{Timeout: requestDeadline}).Get(target.String())
	if err != nil {
		t.Fatalf("GET relay's OAuth callback: %v", err)
	}
	io.Copy(io.Discard, cb.Body)
	cb.Body.Close()
}
