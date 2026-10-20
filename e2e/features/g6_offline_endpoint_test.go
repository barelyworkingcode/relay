package features

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"relaye2e/harness"
)

// g6OfflineRows is a catalogue in which the acme-lab endpoint is offline: its
// last-known model and the model-map key that targets it carry
// endpoint_offline, and one model of another endpoint is online.
func g6OfflineRows(offline bool) []json.RawMessage {
	flag := ""
	if offline {
		flag = `,"endpoint_offline":"acme-lab"`
	}
	return []json.RawMessage{
		json.RawMessage(`{"id":"acme-lab/fake-echo","object":"model","owned_by":"acme-lab","context_length":8192` + flag + `}`),
		json.RawMessage(`{"id":"acme-sonnet","object":"model","owned_by":"anthropic-map","target":"acme-lab/fake-echo"` + flag + `}`),
		json.RawMessage(`{"id":"fake-other","object":"model","owned_by":"fake","context_length":8192}`),
	}
}

// g6OfflineStart boots a relay with the model endpoint on and a catalogue in
// which acme-lab is offline (or online), and returns the instance with a
// client holding a project token whose grant is allowed.
func g6OfflineStart(t *testing.T, offline bool, allowed []string) (*harness.Instance, *harness.Client) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Credentials:   g6Creds,
		Presence:      g6Approve,
		Settings:      map[string]json.RawMessage{"model_endpoint": g6Endpoint},
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: g6OfflineRows(offline)},
	})
	i.WaitModelHost(g6Deadline)
	i.WaitSessionHost(g6Deadline)
	return i, g6ProjectClient(t, i, "acme-offline", allowed)
}

// g6ProjectClient creates a project with the given allowed_models and returns
// a model-endpoint client bearing its token.
func g6ProjectClient(t *testing.T, i *harness.Instance, name string, allowed []string) *harness.Client {
	t.Helper()
	p := g6Project(t, i, name, map[string]any{
		"allowed_templates": []string{"chat"},
		"allowed_models":    allowed,
	})
	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}
	return i.ModelHTTP(tok.Token)
}

func g6Messages(model string) map[string]any {
	return map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
}

func g6ForwardedPosts(i *harness.Instance) int {
	n := 0
	for _, c := range i.FakeModelHostCalls() {
		if strings.HasPrefix(c.Method, "POST ") {
			n++
		}
	}
	return n
}

func TestModelEndpointOffline503(t *testing.T) {
	t.Parallel()
	i, client := g6OfflineStart(t, true, []string{"*"})

	trace := harness.NewTrace(t)
	chat := client.Do("POST", "/v1/chat/completions", g6Completion("acme-lab/fake-echo"), harness.ReqOpts{Trace: trace})
	if chat.Status != 503 {
		t.Fatalf("chat completions on an offline endpoint's model answered %d, want 503: %s", chat.Status, chat.Body)
	}
	var openai struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	chat.JSON(t, &openai)
	if !strings.Contains(openai.Error.Message, "acme-lab") {
		t.Fatalf("the OpenAI-shape 503 does not name the endpoint acme-lab: %s", chat.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: trace,
		Fields: map[string]any{"http_status": float64(503), "status": "error", "reason": "unavailable"}}, g6Deadline)

	mTrace := harness.NewTrace(t)
	msg := client.Do("POST", "/v1/messages", g6Messages("acme-sonnet"), harness.ReqOpts{Trace: mTrace})
	if msg.Status != 503 {
		t.Fatalf("/v1/messages on a model-map key of an offline endpoint answered %d, want 503: %s", msg.Status, msg.Body)
	}
	var anthropic struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg.JSON(t, &anthropic)
	if anthropic.Type != "error" || !strings.Contains(anthropic.Error.Message, "acme-lab") {
		t.Fatalf("the Anthropic-shape 503 is not an error body naming acme-lab: %s", msg.Body)
	}
	if strings.Contains(string(msg.Body), "fake-echo") {
		t.Fatalf("the 503 for a model-map key shows the target id: %s", msg.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: mTrace,
		Fields: map[string]any{"http_status": float64(503), "status": "error", "reason": "unavailable"}}, g6Deadline)

	if n := g6ForwardedPosts(i); n != 0 {
		t.Fatalf("the fake model host logged %d forwarded POSTs for offline models, want 0", n)
	}

	// A caller outside the grant never sees the 503: its 404 is byte-identical
	// to the 404 for a model that exists nowhere.
	narrow := g6ProjectClient(t, i, "acme-narrow", []string{"fake-other"})
	denied := narrow.Do("POST", "/v1/chat/completions", g6Completion("acme-lab/fake-echo"))
	absent := narrow.Do("POST", "/v1/chat/completions", g6Completion("no-such-model"))
	if denied.Status != 404 || absent.Status != 404 {
		t.Fatalf("outside the grant: offline model answered %d, absent model answered %d, want 404 for both", denied.Status, absent.Status)
	}
	if !bytes.Equal(denied.Body, absent.Body) {
		t.Fatalf("the 404 for an offline model outside the grant differs from the absent-model 404:\n%s\n%s", denied.Body, absent.Body)
	}

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
	if hasAll(listed, "acme-lab/fake-echo") || hasAll(listed, "acme-sonnet") || !hasAll(listed, "fake-other") {
		t.Fatalf("GET /v1/models lists %v, want fake-other and neither offline row", listed)
	}
}

func TestModelAbsentEverywhere404(t *testing.T) {
	t.Parallel()
	i, client := g6OfflineStart(t, true, []string{"*"})

	trace := harness.NewTrace(t)
	resp := client.Do("POST", "/v1/chat/completions", g6Completion("no-such-model"), harness.ReqOpts{Trace: trace})
	if resp.Status != 404 {
		t.Fatalf("a granted call for a model in no endpoint answered %d, want 404: %s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: trace,
		Fields: map[string]any{"http_status": float64(404)}}, g6Deadline)

	// The 404 is the same body a caller outside the grant gets: the offline
	// rows change nothing for a model that is absent.
	narrow := g6ProjectClient(t, i, "acme-narrow", []string{"fake-other"})
	same := narrow.Do("POST", "/v1/chat/completions", g6Completion("no-such-model"))
	if same.Status != 404 || !bytes.Equal(resp.Body, same.Body) {
		t.Fatalf("absent-model 404 bodies differ between callers (%d):\n%s\n%s", same.Status, resp.Body, same.Body)
	}
	if n := g6ForwardedPosts(i); n != 0 {
		t.Fatalf("the fake model host logged %d forwarded POSTs, want 0", n)
	}
}

func TestModelEndpointOfflineClears(t *testing.T) {
	t.Parallel()
	i, client := g6OfflineStart(t, true, []string{"*"})

	trace := harness.NewTrace(t)
	first := client.Do("POST", "/v1/chat/completions", g6Completion("acme-lab/fake-echo"), harness.ReqOpts{Trace: trace})
	if first.Status != 503 {
		t.Fatalf("the call while the endpoint is offline answered %d, want 503: %s", first.Status, first.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.request", Trace: trace, Fields: map[string]any{"http_status": float64(503)}}, g6Deadline)

	// The host lists the same rows online again. Relay's clock is not moved:
	// the refetch on an offline hit must clear the mark.
	i.SetFakeModelHostModels(g6OfflineRows(false))

	second := client.Do("POST", "/v1/chat/completions", g6Completion("acme-lab/fake-echo"))
	if second.Status != 200 {
		t.Fatalf("the first call after the host lists the endpoint online answered %d, want 200: %s", second.Status, second.Body)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	second.JSON(t, &body)
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "echo: hi" {
		t.Fatalf("the served call did not echo: %s", second.Body)
	}
}
