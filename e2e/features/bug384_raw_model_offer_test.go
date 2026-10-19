package features

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"relaye2e/harness"
)

// bug384Models offers fake-echo and fake-other.
func bug384Models() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{ID: "acme-models", Models: []json.RawMessage{
		json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
		json.RawMessage(`{"id":"fake-other","object":"model","owned_by":"fake","context_length":8192}`),
	}}
}

func bug384Templates() map[string]any {
	return map[string]any{"allowed_templates": []string{"chat", "claude-code", "codex", "pi"}}
}

func bug384Launched(i *harness.Instance) int {
	return len(agStartArgvs(i, "claude")) + len(agStartArgvs(i, "codex")) + len(agStartArgvs(i, "pi"))
}

func bug384NoSession(t *testing.T, i *harness.Instance) {
	t.Helper()
	if list, _ := g1sListSessions(t, i); len(list.Sessions) != 0 {
		t.Fatalf("a refused start left %d sessions", len(list.Sessions))
	}
	if got := bug384Launched(i); got != 0 {
		t.Fatalf("a refused start launched %d agents", got)
	}
}

func TestBug384RawModelNotOfferedRefused(t *testing.T) {
	t.Parallel()
	i := agStart(t, func(o *harness.Options) { o.FakeModelHost = bug384Models() })
	i.WaitModelHost(agHostDeadline)
	extra := bug384Templates()
	extra["allowed_models"] = []string{"fake-echo", "codex", "nosuchmodel-xyz"}
	pid := agProject(t, i, "acme-raw", extra)

	// CLI door, with the kind name as the model.
	res := i.CLI("session", "start", "--project", pid, "--model", "codex", "--json")
	if res.Code != 1 {
		t.Fatalf("relay session start --model codex exited %d, want 1\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	for _, want := range []string{`"codex"`, "chat", "fake-echo", "codex/codex"} {
		if !strings.Contains(string(res.Stderr), want) {
			t.Fatalf("the CLI refusal %q lacks %q", string(res.Stderr), want)
		}
	}
	if strings.Contains(string(res.Stderr), "fake-other") {
		t.Fatalf("the CLI refusal %q lists fake-other, which the project does not allow", string(res.Stderr))
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: res.Trace,
		Fields: map[string]any{"status": "denied", "reason": "model_not_offered", "model": "codex"}})

	// HTTP door, with a model nobody offers.
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": pid, "model": "nosuchmodel-xyz"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	msg := agSimple(t, resp, http.StatusBadRequest)
	for _, want := range []string{`"nosuchmodel-xyz"`, "chat", "fake-echo"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the HTTP refusal %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "fake-other") || strings.Contains(msg, "codex/") {
		t.Fatalf("the HTTP refusal %q lists a model it should not, or carries a hint for another model", msg)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "model_not_offered", "model": "nosuchmodel-xyz"}})

	bug384NoSession(t, i)
}

func TestBug384RawCodexAndPiModelNotOffered(t *testing.T) {
	t.Parallel()
	i := agStart(t, func(o *harness.Options) { o.FakeModelHost = g1tEchoHost() })
	i.WaitModelHost(agHostDeadline)
	pid := agProject(t, i, "acme-kinds", bug384Templates())
	run := i.SocketHTTP(i.Credential("runner"))

	for _, c := range []struct{ model, kind, listed string }{
		{"codex/gpt-x", "codex", "codex/fake-echo"},
		{"pi/relay-router/nosuch", "pi", "pi/relay-router/fake-echo"},
	} {
		resp := run.Do("POST", "/api/sessions", map[string]any{"projectId": pid, "model": c.model}, harness.ReqOpts{Trace: harness.NewTrace(t)})
		msg := agSimple(t, resp, http.StatusBadRequest)
		for _, want := range []string{`"` + c.model + `"`, c.kind, c.listed} {
			if !strings.Contains(msg, want) {
				t.Fatalf("the refusal of %s is %q, want it to contain %q", c.model, msg, want)
			}
		}
		requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
			Fields: map[string]any{"status": "denied", "reason": "model_not_offered", "model": c.model}})
	}
	bug384NoSession(t, i)
}

func TestBug384RawModelListUnavailable(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-nolist", bug384Templates())

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": pid, "model": "fake-echo"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	agSimple(t, resp, http.StatusServiceUnavailable)
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "models_unavailable", "model": "fake-echo"}})
	bug384NoSession(t, i)
}

func TestBug384OfferedRawModelsStillStart(t *testing.T) {
	t.Parallel()
	i := agStart(t, func(o *harness.Options) { o.FakeModelHost = g1tEchoHost() })
	i.WaitModelHost(agHostDeadline)
	pid := agProject(t, i, "acme-offered", bug384Templates())
	run := i.SocketHTTP(i.Credential("runner"))

	for _, model := range []string{"fake-echo", "codex/fake-echo", "pi/relay-router/fake-echo", "claude-haiku-5-5"} {
		resp := run.Do("POST", "/api/sessions", map[string]any{"projectId": pid, "model": model}, harness.ReqOpts{Trace: harness.NewTrace(t)})
		if resp.Status != http.StatusCreated {
			t.Fatalf("a start with model %s answered %d, want 201: %s", model, resp.Status, resp.Body)
		}
	}
}

func TestBug384CosRawModelNotOffered(t *testing.T) {
	t.Parallel()
	i := cosStart(t, func(o *harness.Options) { o.FakeModelHost = bug384Models() })
	i.WaitModelHost(agHostDeadline)
	tmpl := []map[string]any{{"id": "t_odd", "name": "Oddchat", "model": "nosuchmodel-xyz", "system_prompt": "You are the acme helper."}}
	extra := bug384Templates()
	extra["chat_templates"] = tmpl
	pid := agProject(t, i, "acme-cos-raw", extra)

	for _, c := range []struct {
		name  string
		body  map[string]any
		model string
	}{
		{"a raw model", map[string]any{"projectId": pid, "prompt": "acme hello", "model": "codex"}, "codex"},
		{"a chat template", map[string]any{"projectId": pid, "prompt": "acme hello", "entry": "oddchat"}, "nosuchmodel-xyz"},
	} {
		resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions", c.body)
		e := cosRefusal(t, resp, http.StatusBadRequest, "model_not_offered")
		if !g2Equal(e.Choices, []string{"fake-echo", "fake-other"}) {
			t.Fatalf("%s answered choices %v, want [fake-echo fake-other]", c.name, e.Choices)
		}
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace,
			Fields: map[string]any{"status": "denied", "reason": "model_not_offered", "model": c.model}})
	}
	bug384NoSession(t, i)
}
