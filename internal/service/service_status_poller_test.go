package service

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// fakeSnapshotPoller is a SnapshotPoller over a fixed, test-supplied list of
// records -- standing in for EnhancedServiceRegistry (main), which is the
// only production implementer.
type fakeSnapshotPoller struct{ records []ServiceRecord }

func (f *fakeSnapshotPoller) All() []ServiceRecord { return f.records }

// FetchedAt ticks every poll; if it were part of the digest, the
// settings-window emit suppression would never fire and the WebView would
// re-render every 2s in steady state.
func TestBatchDigest_IgnoresFetchedAt(t *testing.T) {
	mk := func(when int64) []StatusSnapshot {
		return []StatusSnapshot{{
			ServiceID: "svc-a",
			Manifest:  bridge.Manifest{Routes: []string{"/api/a/"}},
			OK:        true,
			Status:    json.RawMessage(`{"uptimeSeconds":42}`),
			FetchedAt: when,
		}}
	}
	d1 := BatchDigest(mk(1000))
	d2 := BatchDigest(mk(time.Now().UnixMilli()))
	if d1 != d2 {
		t.Errorf("digest changed despite identical content (only FetchedAt differs); suppression won't fire")
	}
}

func TestBatchDigest_DetectsStatusChange(t *testing.T) {
	mk := func(body string) []StatusSnapshot {
		return []StatusSnapshot{{
			ServiceID: "svc-a",
			Manifest:  bridge.Manifest{Routes: []string{"/api/a/"}},
			OK:        true,
			Status:    json.RawMessage(body),
			FetchedAt: 0,
		}}
	}
	if BatchDigest(mk(`{"x":1}`)) == BatchDigest(mk(`{"x":2}`)) {
		t.Error("digest collision on different payloads — change detection is broken")
	}
}

func TestPollStatuses_MultiService_IncludesStatuslessEntries(t *testing.T) {
	srvA := newFakeServiceServer(t)
	srvA.script("GET", "/api/status", 200, `{"uptimeSeconds":10}`)
	mfA := bridge.Manifest{
		Routes: []string{"/api/a/"},
		Status: &bridge.StatusDecl{Path: "/api/status"},
	}

	srvB := newFakeServiceServer(t)
	mfB := bridge.Manifest{Routes: []string{"/api/b/"}}

	srvC := newFakeServiceServer(t)
	srvC.script("GET", "/api/status", 503, `{"error":"unavailable"}`)
	mfC := bridge.Manifest{
		Routes: []string{"/api/c/"},
		Status: &bridge.StatusDecl{Path: "/api/status"},
	}

	poller := &fakeSnapshotPoller{records: []ServiceRecord{
		{ServiceID: "svc-a", InternalSocket: srvA.socket, InternalToken: "tok-a", Manifest: mfA},
		{ServiceID: "svc-b", InternalSocket: srvB.socket, InternalToken: "tok-b", Manifest: mfB},
		{ServiceID: "svc-c", InternalSocket: srvC.socket, InternalToken: "tok-c", Manifest: mfC},
	}}

	batch := PollStatuses(context.Background(), poller)
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

func TestPollStatuses_EmptyRegistry_ReturnsNil(t *testing.T) {
	poller := &fakeSnapshotPoller{}
	if got := PollStatuses(context.Background(), poller); got != nil {
		t.Errorf("expected nil batch for empty registry, got %+v", got)
	}
}

func TestPollStatuses_NilRegistry_IsSafe(t *testing.T) {
	if got := PollStatuses(context.Background(), nil); got != nil {
		t.Errorf("expected nil for nil registry, got %+v", got)
	}
}

func TestPollStatuses_BatchCarriesManifest(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("GET", "/api/status", 200, `{}`)
	mf := bridge.Manifest{
		Routes:  []string{"/api/x/"},
		Status:  &bridge.StatusDecl{Path: "/api/status"},
		Actions: []bridge.ActionDecl{{ID: "do-thing", Label: "Do", Method: "POST", PathTemplate: "/api/x/do"}},
	}
	poller := &fakeSnapshotPoller{records: []ServiceRecord{
		{ServiceID: "svc-x", InternalSocket: srv.socket, InternalToken: "tok", Manifest: mf},
	}}

	batch := PollStatuses(context.Background(), poller)
	if len(batch) != 1 {
		t.Fatalf("want 1 entry, got %d", len(batch))
	}
	if len(batch[0].Manifest.Actions) != 1 || batch[0].Manifest.Actions[0].ID != "do-thing" {
		t.Errorf("manifest not carried into snapshot: %+v", batch[0].Manifest)
	}
}
