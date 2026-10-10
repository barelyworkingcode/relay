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

const g6Deadline = 60 * time.Second

var g6Runner = []harness.CredentialSpec{{Name: "runner", Classes: []string{"execute"}}}

var g6GrantAndReveal = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
}

// g6Models is a fake model host with an ordinary row, a second ordinary row
// and a system-only row.
func g6Models() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{
		ID: "acme-models",
		Models: []json.RawMessage{
			json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
			json.RawMessage(`{"id":"fake-other","object":"model","owned_by":"fake","context_length":8192}`),
			json.RawMessage(`{"id":"fake-system","object":"model","owned_by":"fake","system":true,"context_length":8192}`),
		},
	}
}

// g6Project creates a project that allows the given models and the chat template.
func g6Project(t *testing.T, i *harness.Instance, allowedModels []string) project {
	t.Helper()
	dir := filepath.Join(i.Home, "work", "acme-models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, err := json.Marshal(map[string]any{
		"name": "Acme models", "path": dir,
		"allowed_models": allowedModels, "allowed_templates": []string{"chat"},
	})
	if err != nil {
		t.Fatalf("encoding the project: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id")
	}
	return p
}

func g6Token(t *testing.T, i *harness.Instance, projectID string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", projectID, "--json").JSON(t, &out)
	if out.Token == "" {
		t.Fatalf("project token printed no token")
	}
	return out.Token
}

func g6Completion(model string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
}

func g6ModelRows(t *testing.T, r harness.Response) []struct {
	ID     string `json:"id"`
	Status struct {
		Value string `json:"value"`
	} `json:"status"`
} {
	t.Helper()
	var doc struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	r.JSON(t, &doc)
	return doc.Data
}

// g6AuditRows returns the model_call rows with the outcome.
func g6AuditRows(i *harness.Instance, outcome string) []map[string]any {
	return i.Audit(harness.AuditQuery{Event: "model_call", Outcome: outcome})
}

func TestModelListOmitsSystem(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{FakeModelHost: g6Models()})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)

	res := i.MustCLI("model", "list", "--json")
	var ml modelList
	res.JSON(t, &ml)
	var ids []string
	for _, m := range ml.Models {
		ids = append(ids, m.ID)
	}
	if ml.Status != "ok" || !hasAll(ids, "fake-echo", "fake-other") {
		t.Fatalf("model list status %q ids %v, want ok with fake-echo and fake-other", ml.Status, ids)
	}
	for _, id := range ids {
		if strings.Contains(id, "fake-system") {
			t.Fatalf("model list holds the system-only model: %v", ids)
		}
	}
	ev := i.WaitEvent(harness.EventQuery{Key: "model.list", Trace: res.Trace, Fields: map[string]any{"status": "ok"}}, g6Deadline)
	if count, _ := ev["count"].(float64); int(count) != len(ml.Models) {
		t.Fatalf("model.list count %v, want %d (the models printed)", ev["count"], len(ml.Models))
	}
}

func TestModelOutsideAllowedRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Runner,
		Presence:      approveGrant,
		FakeModelHost: g6Models(),
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)
	p := g6Project(t, i, []string{"fake-echo"})
	door := i.SocketHTTP(i.Credential("runner"))

	allowed := door.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-echo"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if allowed.Status != 201 {
		t.Fatalf("a launch on the allowed model answered %d, want 201: %s", allowed.Status, allowed.Body)
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "denied"}); len(rows) != 0 {
		t.Fatalf("%d denied session_launch rows before the refused launch, want 0", len(rows))
	}

	refused := door.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-other"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if refused.Status != 403 {
		t.Fatalf("a launch on a model outside allowed_models answered %d, want 403", refused.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: refused.Trace, Fields: map[string]any{"status": "denied", "reason": "model_not_allowed", "project_id": p.ID}})
	if rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "denied"}); len(rows) != 1 {
		t.Fatalf("%d denied session_launch rows after the refused launch, want 1", len(rows))
	}
}

func TestModelEndpointRoutes(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g6GrantAndReveal, FakeModelHost: g6Models()})
	i.WaitModelHost(g6Deadline)
	p := g6Project(t, i, []string{"fake-echo"})
	api := i.ModelHTTP(g6Token(t, i, p.ID))

	list := api.Do("GET", "/v1/models", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if list.Status != 200 {
		t.Fatalf("GET /v1/models answered %d, want 200", list.Status)
	}
	var ids []string
	for _, r := range g6ModelRows(t, list) {
		ids = append(ids, r.ID)
	}
	if len(ids) != 1 || ids[0] != "fake-echo" {
		t.Fatalf("GET /v1/models named %v, want only the granted fake-echo", ids)
	}

	router := api.Do("GET", "/models", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if router.Status != 200 {
		t.Fatalf("GET /models answered %d, want 200", router.Status)
	}
	rows := g6ModelRows(t, router)
	if len(rows) != 1 || rows[0].ID != "fake-echo" || rows[0].Status.Value != "loaded" {
		t.Fatalf("GET /models rows %+v, want fake-echo with status.value loaded", rows)
	}

	props := api.Do("GET", "/props", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	var pr struct {
		Autoload *bool `json:"models_autoload"`
	}
	if props.Status != 200 {
		t.Fatalf("GET /props answered %d, want 200", props.Status)
	}
	props.JSON(t, &pr)
	if pr.Autoload == nil || *pr.Autoload {
		t.Fatalf("GET /props models_autoload %v, want false", pr.Autoload)
	}

	done := api.Do("POST", "/v1/chat/completions", g6Completion("fake-echo"), harness.ReqOpts{Trace: harness.NewTrace(t)})
	if done.Status != 200 {
		t.Fatalf("POST /v1/chat/completions answered %d, want 200", done.Status)
	}
	var comp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	done.JSON(t, &comp)
	if len(comp.Choices) != 1 || comp.Choices[0].Message.Content != "echo: hi" {
		t.Fatalf("completion choices %+v, want one reply echo: hi", comp.Choices)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: done.Trace, Fields: map[string]any{
		"status": "ok", "path": "/v1/chat/completions", "http_status": float64(200), "model": "fake-echo",
	}}, g6Deadline)

	var call map[string]any
	for _, row := range g6AuditRows(i, "ok") {
		if row["path"] == "/v1/chat/completions" {
			call = row
		}
	}
	if call == nil {
		t.Fatalf("no ok model_call row for /v1/chat/completions in %v", g6AuditRows(i, "ok"))
	}
	if call["model"] != "fake-echo" {
		t.Fatalf("model_call row model %v, want fake-echo", call["model"])
	}

	reached := false
	for _, c := range i.FakeModelHostCalls() {
		reached = reached || c.Method == "POST /v1/chat/completions"
	}
	if !reached {
		t.Fatalf("the fake model host logged no POST /v1/chat/completions")
	}

	for _, poll := range []harness.Response{list, router, props} {
		if got := i.Events(harness.EventQuery{Key: "model.request", Trace: poll.Trace}); len(got) != 0 {
			t.Fatalf("a successful poll wrote %d model.request events, want 0", len(got))
		}
	}
}

func TestModelKeyRequired(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, FakeModelHost: g6Models()})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)

	p := g6Project(t, i, []string{"fake-echo"})
	r := i.MustCLI("session", "start", "--project", p.ID, "--model", "fake-echo", "--json")
	var started struct {
		SessionID string `json:"sessionId"`
	}
	r.JSON(t, &started)
	sessionID := started.SessionID
	var reply struct {
		Text string `json:"text"`
	}
	i.MustCLI("session", "message", "--id", sessionID, "--text", "hi", "--json").JSON(t, &reply)
	if reply.Text != "echo: hi" {
		t.Fatalf("chat reply %q, want %q", reply.Text, "echo: hi")
	}

	var minted map[string]any
	for _, row := range g6AuditRows(i, "ok") {
		if actor, _ := row["actor"].(map[string]any); actor["auth"] == "model_key" {
			minted = row
		}
	}
	if minted == nil {
		t.Fatalf("no ok model_call row with actor.auth model_key after a chat turn: %v", g6AuditRows(i, "ok"))
	}
	if minted["model_key_label"] != "session:"+sessionID {
		t.Fatalf("the model_call row key label %v, want session:%s", minted["model_key_label"], sessionID)
	}

	api := i.ModelHTTP("")
	cases := []struct {
		name   string
		token  string
		header http.Header
	}{
		{"no header", "", nil},
		{"unknown key", "", http.Header{"X-Relay-Key": {"rmk_" + strings.Repeat("0", 64)}}},
		{"wrong bearer", "acme-not-a-token", nil},
	}
	for _, c := range cases {
		client := api
		if c.token != "" {
			client = i.ModelHTTP(c.token)
		}
		resp := client.Do("POST", "/v1/chat/completions", g6Completion("fake-echo"), harness.ReqOpts{Trace: harness.NewTrace(t), Header: c.header})
		if resp.Status != 401 {
			t.Fatalf("%s answered %d, want 401", c.name, resp.Status)
		}
		i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: resp.Trace, Fields: map[string]any{
			"status": "denied", "reason": "unauthorized", "http_status": float64(401),
		}}, g6Deadline)
	}
	if rows := g6AuditRows(i, "unauthorized"); len(rows) != len(cases) {
		t.Fatalf("%d unauthorized model_call rows, want %d", len(rows), len(cases))
	}
}

func TestModelHostRegisterOwnIdentityOnly(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{FakeModelHost: g6Models()})
	i.WaitModelHost(g6Deadline)
	requireEvent(t, i, harness.EventQuery{Key: "model.host.register", Fields: map[string]any{"status": "ok", "service_id": "acme-models"}})

	reply := i.BridgeSend(map[string]any{
		"type":      "RegisterModelHost",
		"arguments": map[string]any{"service_id": "acme-models", "router_socket": filepath.Join(i.Tmp, "acme-router.sock")},
	})
	if reply.Type != "Error" || reply.Code != -32001 {
		t.Fatalf("a RegisterModelHost from a caller with no launch identity answered %s code %d, want Error -32001", reply.Type, reply.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "model.host.register", Trace: reply.Trace, Fields: map[string]any{"status": "denied"}})
	if got := i.Events(harness.EventQuery{Key: "model.host.register", Fields: map[string]any{"status": "ok"}}); len(got) != 1 {
		t.Fatalf("%d ok model.host.register events after the refusal, want the one registration", len(got))
	}
}
