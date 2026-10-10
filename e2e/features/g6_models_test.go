package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const (
	// g6Deadline bounds the wait for a host to register after boot, and for an
	// event that a background relay-sessions write lands after its answer.
	g6Deadline = 60 * time.Second
	// g6Refused is the 401 the model endpoint answers for every cause.
	g6Refused = 401
)

// g6Creds are the planted credentials these tests may use.
var g6Creds = []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}}

// g6Approve approves the project grant and the project token reveal, the two
// owner prompts the model tests reach.
var g6Approve = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
}

// g6Endpoint turns on the model endpoint's TCP listener on a kernel port.
var g6Endpoint = json.RawMessage(`{"listen":"127.0.0.1:0"}`)

// g6Models is a model host with a chat model and a second chat model that a
// test may leave outside a project's grant.
func g6Models() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
		json.RawMessage(`{"id":"fake-other","object":"model","owned_by":"fake","context_length":8192}`),
	}
}

// g6Completion is a one-message chat-completions body for model.
func g6Completion(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
}

type g6Proj struct {
	ID string `json:"id"`
}

// g6Project creates a local project with the given grant fields. The folder
// is created under the instance home.
func g6Project(t *testing.T, i *harness.Instance, name string, grant map[string]any) g6Proj {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range grant {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p g6Proj
	r := i.CLIWith(harness.CLIOpts{Stdin: b}, "project", "create", "--file", "-", "--json")
	r.JSON(t, &p)
	if r.Code != 0 || p.ID == "" {
		t.Fatalf("project create exited %d and printed no id: %s", r.Code, r.Stdout)
	}
	return p
}

// g6Event returns the first stored event matching q, failing the test when
// none is stored.
func g6Event(t *testing.T, i *harness.Instance, q harness.EventQuery) harness.Event {
	t.Helper()
	got := i.Events(q)
	if len(got) == 0 {
		t.Fatalf("no %s event matched trace %q fields %v", q.Key, q.Trace, q.Fields)
	}
	return got[0]
}

// g6AuditCount counts the audit rows of an event and outcome.
func g6AuditCount(i *harness.Instance, event, outcome string) int {
	return len(i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}))
}

// g6AuditByTrace returns the audit rows of an event and outcome that carry
// the trace.
func g6AuditByTrace(i *harness.Instance, event, outcome, trace string) []map[string]any {
	var out []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}) {
		if row["trace_id"] == trace {
			out = append(out, row)
		}
	}
	return out
}

func TestModelListOmitsSystem(t *testing.T) {
	t.Parallel()
	models := append(g6Models(), json.RawMessage(`{"id":"fake-system","object":"model","owned_by":"fake","system":true,"context_length":8192}`))
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: models},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)

	trace := harness.NewTrace(t)
	var list struct {
		Status string `json:"status"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	r := i.CLIWith(harness.CLIOpts{Trace: trace}, "model", "list", "--json")
	r.JSON(t, &list)
	if r.Code != 0 || list.Status != "ok" {
		t.Fatalf("model list exited %d with status %q, want 0 and ok", r.Code, list.Status)
	}
	var ids []string
	for _, m := range list.Models {
		ids = append(ids, m.ID)
	}
	if !hasAll(ids, "fake-echo") {
		t.Fatalf("model list holds no fake-echo: %v", ids)
	}
	if hasAll(ids, "fake-system") {
		t.Fatalf("model list holds the system-only model fake-system: %v", ids)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.list", Trace: trace,
		Fields: map[string]any{"status": "ok", "count": float64(len(list.Models))}}, g6Deadline)
}

func TestModelOutsideAllowedRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		Presence:      g6Approve,
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: g6Models()},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)
	p := g6Project(t, i, "acme-grant", map[string]any{
		"allowed_templates": []string{"chat"},
		"allowed_models":    []string{"fake-echo"},
	})
	runner := i.SocketHTTP(i.Credential("runner"))

	before := g6AuditCount(i, "session_launch", "denied")
	trace := harness.NewTrace(t)
	refused := runner.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-other"}, harness.ReqOpts{Trace: trace})
	if refused.Status != 403 {
		t.Fatalf("a launch on a model outside allowed_models answered %d, want 403", refused.Status)
	}
	g6Event(t, i, harness.EventQuery{Key: "session.launch", Trace: trace, Fields: map[string]any{"status": "denied"}})
	if after := g6AuditCount(i, "session_launch", "denied"); after != before+1 {
		t.Fatalf("the refused launch wrote %d session_launch denied rows, want 1", after-before)
	}

	allowedTrace := harness.NewTrace(t)
	allowed := runner.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-echo"}, harness.ReqOpts{Trace: allowedTrace})
	if allowed.Status != 201 {
		t.Fatalf("a launch on the allowed model fake-echo answered %d, want 201", allowed.Status)
	}
	g6Event(t, i, harness.EventQuery{Key: "session.launch", Trace: allowedTrace, Fields: map[string]any{"status": "ok"}})
}

func TestModelEndpointRoutes(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		Presence:      g6Approve,
		Settings:      map[string]json.RawMessage{"model_endpoint": g6Endpoint},
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: g6Models()},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)
	p := g6Project(t, i, "acme-endpoint", map[string]any{
		"allowed_templates": []string{"chat"},
		"allowed_models":    []string{"fake-echo"},
	})

	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}
	client := i.ModelHTTP(tok.Token)

	list := client.Do("GET", "/v1/models", nil)
	if list.Status != 200 {
		t.Fatalf("GET /v1/models answered %d, want 200", list.Status)
	}
	var listBody struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	list.JSON(t, &listBody)
	var listed []string
	for _, row := range listBody.Data {
		listed = append(listed, row.ID)
	}
	if !hasAll(listed, "fake-echo") || hasAll(listed, "fake-other") {
		t.Fatalf("GET /v1/models lists %v, want fake-echo and not fake-other", listed)
	}

	router := client.Do("GET", "/models", nil)
	if router.Status != 200 {
		t.Fatalf("GET /models answered %d, want 200", router.Status)
	}
	var routerBody struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	router.JSON(t, &routerBody)
	if len(routerBody.Data) == 0 {
		t.Fatalf("GET /models listed no models, want the fake host's catalogue")
	}
	for _, row := range routerBody.Data {
		if row.Status.Value != "loaded" {
			t.Fatalf("GET /models row %s has status %q, want loaded", row.ID, row.Status.Value)
		}
	}

	if props := client.Do("GET", "/props", nil); props.Status != 200 {
		t.Fatalf("GET /props answered %d, want 200", props.Status)
	}

	trace := harness.NewTrace(t)
	completion := client.Do("POST", "/v1/chat/completions", g6Completion("fake-echo"), harness.ReqOpts{Trace: trace})
	if completion.Status != 200 {
		t.Fatalf("POST /v1/chat/completions on fake-echo answered %d, want 200", completion.Status)
	}
	g6Event(t, i, harness.EventQuery{Key: "model.request", Trace: trace, Fields: map[string]any{"status": "ok"}})
	if got := g6AuditByTrace(i, "model_call", "ok", trace); len(got) != 1 {
		t.Fatalf("the completion wrote %d model_call ok rows, want 1", len(got))
	}

	hit := false
	for _, c := range i.FakeModelHostCalls() {
		hit = hit || c.Method == "POST /v1/chat/completions"
	}
	if !hit {
		t.Fatalf("the fake model host logged no chat completion")
	}
}

func TestModelKeyRequired(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		Presence:      g6Approve,
		Settings:      map[string]json.RawMessage{"model_endpoint": g6Endpoint},
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: g6Models()},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)
	p := g6Project(t, i, "acme-keys", map[string]any{
		"allowed_templates": []string{"chat"},
		"allowed_models":    []string{"fake-echo"},
	})

	var s struct {
		SessionID string `json:"sessionId"`
	}
	i.MustCLI("session", "start", "--project", p.ID, "--model", "fake-echo", "--json").JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start printed no sessionId")
	}
	i.MustCLI("session", "message", "--id", s.SessionID, "--text", "hi", "--json")

	keyed := 0
	for _, row := range i.Audit(harness.AuditQuery{Event: "model_call", Outcome: "ok"}) {
		actor, _ := row["actor"].(map[string]any)
		if actor["auth"] == "model_key" && actor["project_id"] == p.ID {
			keyed++
		}
	}
	if keyed == 0 {
		t.Fatalf("no model_call ok row was made with the session's model key")
	}
	// session_id is only for a session admitted by launch identity or as a
	// member of one. A model-key call never carries it, so the line must omit it.
	okLines := i.Events(harness.EventQuery{Key: "model.request", Fields: map[string]any{"status": "ok"}})
	if len(okLines) == 0 {
		t.Fatalf("no model.request ok event for the session's model-key call")
	}
	for _, line := range okLines {
		if _, has := line["session_id"]; has {
			t.Fatalf("model.request line for a model-key call carries session_id: %v", line["session_id"])
		}
		if line.Str("caller_kind") == "" || line.Str("caller") == "" {
			t.Fatalf("model.request line for a model-key call lacks caller_kind or caller")
		}
	}

	unknown := "rmk_" + strings.Repeat("ab", 32)
	cases := []struct {
		name   string
		client *harness.Client
		opts   harness.ReqOpts
	}{
		{"no header", i.ModelHTTP(""), harness.ReqOpts{}},
		{"an unknown model key", i.ModelHTTP(""), harness.ReqOpts{Header: http.Header{"X-Relay-Key": {unknown}}}},
		{"a wrong bearer", i.ModelHTTP("acme-not-a-token"), harness.ReqOpts{}},
	}
	for _, c := range cases {
		opts := c.opts
		opts.Trace = harness.NewTrace(t)
		resp := c.client.Do("POST", "/v1/chat/completions", g6Completion("fake-echo"), opts)
		if resp.Status != g6Refused {
			t.Fatalf("a model call with %s answered %d, want 401", c.name, resp.Status)
		}
		g6Event(t, i, harness.EventQuery{Key: "model.request", Trace: resp.Trace,
			Fields: map[string]any{"status": "denied", "reason": "unauthorized"}})
		if got := g6AuditByTrace(i, "model_call", "unauthorized", resp.Trace); len(got) == 0 {
			t.Fatalf("a model call with %s wrote no model_call unauthorized row", c.name)
		}
	}
}

func TestModelHostRegisterOwnIdentityOnly(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: g6Models()},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitEvent(harness.EventQuery{Key: "model.host.register", Fields: map[string]any{"status": "ok", "service_id": "acme-models"}}, g6Deadline)

	reply := i.BridgeSend(map[string]any{
		"type": "RegisterModelHost",
		"arguments": map[string]any{
			"service_id":    "acme-models",
			"router_socket": filepath.Join(i.Tmp, "router.sock"),
		},
	})
	if reply.Type != "Error" || reply.Code != -32001 {
		t.Fatalf("a RegisterModelHost from a caller with no launch identity answered %s code %d, want Error -32001", reply.Type, reply.Code)
	}
	g6Event(t, i, harness.EventQuery{Key: "model.host.register", Trace: reply.Trace, Fields: map[string]any{"status": "denied"}})
}
