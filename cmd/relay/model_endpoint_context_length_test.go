package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// A row's context_length is passed on from relayLLM when it is positive and
// omitted otherwise: a client that sees 0 would treat it as a real window.
func TestModelEndpoint_ListsContextLengthOnlyWhenKnown(t *testing.T) {
	const catalog = `{"object":"list","data":[
 {"id":"known","owned_by":"llama.cpp","context_length":32768},
 {"id":"zero","owned_by":"llama.cpp","context_length":0},
 {"id":"absent","owned_by":"llama.cpp"},
 {"id":"negative","owned_by":"llama.cpp","context_length":-5}]}`

	m, _, launches, hosts := newModelEndpointTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalog))
	})
	sock := newFakeRouterSocket(t, mux)
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	ctx := bindModelIdentity(t, launches, "relaysessions", []config.ServiceCapability{config.ServiceCapabilitySessions}, 52010)

	for _, path := range []string{"/v1/models", "/models"} {
		t.Run(path, func(t *testing.T) {
			w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, path, "", ctx, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			has := map[string]bool{}
			for _, row := range body.Data {
				id, _ := row["id"].(string)
				v, ok := row["context_length"]
				got[id], has[id] = v, ok
			}
			if !has["known"] || got["known"] != float64(32768) {
				t.Errorf("known: context_length = %v (present=%v), want 32768", got["known"], has["known"])
			}
			for _, id := range []string{"zero", "absent", "negative"} {
				if has[id] {
					t.Errorf("%s: context_length = %v emitted, want omitted", id, got[id])
				}
			}
			if len(body.Data) != 4 {
				t.Errorf("got %d rows, want 4", len(body.Data))
			}
		})
	}
}
