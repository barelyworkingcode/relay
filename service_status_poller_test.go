package main

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"relaygo/bridge"
)

// FetchedAt ticks every poll; if it were part of the digest, the
// settings-window emit suppression would never fire and the WebView would
// re-render every 2s in steady state.
func TestBatchDigest_IgnoresFetchedAt(t *testing.T) {
	mk := func(when int64) []ServiceStatusSnapshot {
		return []ServiceStatusSnapshot{{
			ServiceID: "svc-a",
			Manifest:  bridge.Manifest{Routes: []string{"/api/a/"}},
			OK:        true,
			Status:    json.RawMessage(`{"uptimeSeconds":42}`),
			FetchedAt: when,
		}}
	}
	d1 := batchDigest(mk(1000))
	d2 := batchDigest(mk(time.Now().UnixMilli()))
	if d1 != d2 {
		t.Errorf("digest changed despite identical content (only FetchedAt differs); suppression won't fire")
	}
}

func TestBatchDigest_DetectsStatusChange(t *testing.T) {
	mk := func(body string) []ServiceStatusSnapshot {
		return []ServiceStatusSnapshot{{
			ServiceID: "svc-a",
			Manifest:  bridge.Manifest{Routes: []string{"/api/a/"}},
			OK:        true,
			Status:    json.RawMessage(body),
			FetchedAt: 0,
		}}
	}
	if batchDigest(mk(`{"x":1}`)) == batchDigest(mk(`{"x":2}`)) {
		t.Error("digest collision on different payloads — change detection is broken")
	}
}

// The poller iterates every registered service and emits a snapshot per
// service — including ones with no status declaration, so the UI can show
// "registered but no status" without inventing service IDs out of band.
func TestPollServiceStatuses_MultiService_IncludesStatuslessEntries(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)

	// svc-a has a status endpoint that returns JSON.
	srvA := newFakeServiceServer(t)
	srvA.script("GET", "/api/status", 200, `{"uptimeSeconds":10}`)
	mfA := bridge.Manifest{
		Routes: []string{"/api/a/"},
		Status: &bridge.StatusDecl{Path: "/api/status"},
	}
	if err := reg.RegisterManifest("svc-a", srvA.socket, "tok-a", mfA); err != nil {
		t.Fatalf("register svc-a: %v", err)
	}

	// svc-b is registered but declares no status. Should still appear in
	// the batch as OK with nil status.
	srvB := newFakeServiceServer(t)
	mfB := bridge.Manifest{Routes: []string{"/api/b/"}}
	if err := reg.RegisterManifest("svc-b", srvB.socket, "tok-b", mfB); err != nil {
		t.Fatalf("register svc-b: %v", err)
	}

	// svc-c declares status but returns 5xx. Should appear with OK=false
	// and an Error string — UI renders this as the "error" badge.
	srvC := newFakeServiceServer(t)
	srvC.script("GET", "/api/status", 503, `{"error":"unavailable"}`)
	mfC := bridge.Manifest{
		Routes: []string{"/api/c/"},
		Status: &bridge.StatusDecl{Path: "/api/status"},
	}
	if err := reg.RegisterManifest("svc-c", srvC.socket, "tok-c", mfC); err != nil {
		t.Fatalf("register svc-c: %v", err)
	}

	batch := pollServiceStatuses(context.Background(), reg)
	if len(batch) != 3 {
		t.Fatalf("batch size: got %d, want 3 (%+v)", len(batch), batch)
	}

	// Sort by id for stable assertions independent of poll-completion order.
	sort.Slice(batch, func(i, j int) bool { return batch[i].ServiceID < batch[j].ServiceID })

	if !batch[0].OK {
		t.Errorf("svc-a should be OK, got %+v", batch[0])
	}
	var aPayload map[string]interface{}
	if err := json.Unmarshal(batch[0].Status, &aPayload); err != nil {
		t.Errorf("svc-a payload not valid JSON: %v (%q)", err, string(batch[0].Status))
	}

	if !batch[1].OK {
		t.Errorf("svc-b (no status declared) should still be OK, got %+v", batch[1])
	}
	if len(batch[1].Status) != 0 {
		t.Errorf("svc-b status payload should be empty, got %q", string(batch[1].Status))
	}

	if batch[2].OK {
		t.Errorf("svc-c should be OK=false on 503, got %+v", batch[2])
	}
	if batch[2].Error == "" {
		t.Error("svc-c should populate Error string")
	}
}

func TestPollServiceStatuses_EmptyRegistry_ReturnsNil(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	if got := pollServiceStatuses(context.Background(), reg); got != nil {
		t.Errorf("expected nil batch for empty registry, got %+v", got)
	}
}

func TestPollServiceStatuses_NilRegistry_IsSafe(t *testing.T) {
	if got := pollServiceStatuses(context.Background(), nil); got != nil {
		t.Errorf("expected nil for nil registry, got %+v", got)
	}
}

// Manifest is carried in the batch so the UI can render action layouts
// without a separate manifest-list IPC roundtrip. The poller must
// faithfully attach what the registry holds.
func TestPollServiceStatuses_BatchCarriesManifest(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	srv := newFakeServiceServer(t)
	srv.script("GET", "/api/status", 200, `{}`)
	mf := bridge.Manifest{
		Routes:  []string{"/api/x/"},
		Status:  &bridge.StatusDecl{Path: "/api/status"},
		Actions: []bridge.ActionDecl{{ID: "do-thing", Label: "Do", Method: "POST", PathTemplate: "/api/x/do"}},
	}
	_ = reg.RegisterManifest("svc-x", srv.socket, "tok", mf)

	batch := pollServiceStatuses(context.Background(), reg)
	if len(batch) != 1 {
		t.Fatalf("want 1 entry, got %d", len(batch))
	}
	if len(batch[0].Manifest.Actions) != 1 || batch[0].Manifest.Actions[0].ID != "do-thing" {
		t.Errorf("manifest not carried into snapshot: %+v", batch[0].Manifest)
	}
}
