package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// StatusSnapshot captures the state of a service at a point in time.
type StatusSnapshot struct {
	ServiceID string          `json:"serviceId"`
	Manifest  bridge.Manifest `json:"manifest"`
	OK        bool            `json:"ok"`
	Status    json.RawMessage `json:"status,omitempty"`
	Error     string          `json:"error,omitempty"`
	FetchedAt int64           `json:"fetchedAt"` // unix ms
}

// ServiceRecord describes a registered service for polling.
type ServiceRecord struct {
	ServiceID      string
	InternalSocket string
	InternalToken  string
	Manifest       bridge.Manifest
}

// SnapshotPoller can fetch status snapshots from registered services.
type SnapshotPoller interface {
	// All returns the list of currently registered services.
	All() []ServiceRecord
}

// PollStatuses fetches each registered service's status endpoint and
// returns one snapshot per service; services without a Status declaration
// still appear so the UI can show "registered, no status". Fetches run
// concurrently, each bounded by the client's statusFetchTimeout so a slow
// service can't stall the whole tick.
func PollStatuses(ctx context.Context, poller SnapshotPoller) []StatusSnapshot {
	if poller == nil {
		return nil
	}
	records := poller.All()
	if len(records) == 0 {
		return nil
	}

	out := make([]StatusSnapshot, len(records))
	var wg sync.WaitGroup
	wg.Add(len(records))
	for i, rec := range records {
		i, rec := i, rec
		go func() {
			defer wg.Done()
			snap := StatusSnapshot{
				ServiceID: rec.ServiceID,
				Manifest:  rec.Manifest,
				FetchedAt: time.Now().UnixMilli(),
			}
			if rec.Manifest.Status == nil || rec.Manifest.Status.Path == "" {
				snap.OK = true
				out[i] = snap
				return
			}
			client := NewStatusClient(rec.InternalSocket, rec.InternalToken)
			defer client.CloseIdleConnections()
			body, err := client.GetStatus(ctx, rec.Manifest.Status.Path)
			if err != nil {
				snap.OK = false
				snap.Error = "service " + rec.ServiceID + ": " + err.Error()
			} else {
				snap.OK = true
				snap.Status = body
			}
			out[i] = snap
		}()
	}
	wg.Wait()
	return out
}

// BatchDigest zeroes FetchedAt before hashing -- it ticks every poll and
// would otherwise defeat change-detection suppression.
func BatchDigest(batch []StatusSnapshot) [32]byte {
	stripped := make([]StatusSnapshot, len(batch))
	for i, s := range batch {
		s.FetchedAt = 0
		stripped[i] = s
	}
	raw, _ := json.Marshal(stripped)
	return sha256.Sum256(raw)
}
