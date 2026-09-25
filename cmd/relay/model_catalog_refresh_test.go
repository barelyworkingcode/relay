package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// mutableCatalog is a model host whose /v1/models listing can grow mid-test
// and which records every path it was asked for.
type mutableCatalog struct {
	mu    sync.Mutex
	ids   []string
	paths []string
}

func (c *mutableCatalog) add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
}

func (c *mutableCatalog) catalogFetches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.paths {
		if p == "/v1/models" {
			n++
		}
	}
	return n
}

func (c *mutableCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	type row struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	}
	body := struct {
		Object string `json:"object"`
		Data   []row  `json:"data"`
	}{Object: "list"}
	for _, id := range c.ids {
		body.Data = append(body.Data, row{ID: id, OwnedBy: "mlx-omni"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestModelEndpoint_CatalogShowsUpstreamAdditionAfterTTL(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	upstream := &mutableCatalog{ids: []string{"omlx/Chat"}}
	registerFakeHost(t, hosts, launches, "relayllm", newFakeRouterSocket(t, upstream), selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", []string{"*"}, false)
	now := time.Unix(1_800_000_000, 0)
	m.catalog.SetClock(func() time.Time { return now })

	list := func(at string) string {
		t.Helper()
		w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", tok, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: GET /v1/models status = %d, body=%s", at, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	if body := list("first listing"); !strings.Contains(body, "omlx/Chat") {
		t.Fatalf("first listing lacks the upstream model: %s", body)
	}
	upstream.add("omlx/Added")
	fetched := upstream.catalogFetches()

	now = now.Add(29 * time.Second)
	if body := list("+29s"); strings.Contains(body, "omlx/Added") {
		t.Fatalf("+29s: catalog refreshed before its TTL: %s", body)
	}
	if n := upstream.catalogFetches() - fetched; n != 0 {
		t.Fatalf("+29s: %d catalog fetches, want 0", n)
	}
	now = now.Add(time.Second)
	if body := list("+30s"); !strings.Contains(body, "omlx/Added") {
		t.Fatalf("+30s: upstream addition still missing: %s", body)
	}
	if n := upstream.catalogFetches() - fetched; n != 1 {
		t.Fatalf("+30s: %d catalog fetches, want 1", n)
	}
}
