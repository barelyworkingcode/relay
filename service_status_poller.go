package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"relaygo/bridge"
)

// ServiceStatusSnapshot is one tick of one service's polled status, plus a
// snapshot of the manifest the UI needs to render action buttons. Sent to
// the settings window as part of the per-tick batch.
type ServiceStatusSnapshot struct {
	ServiceID string          `json:"serviceId"`
	Manifest  bridge.Manifest `json:"manifest"`
	OK        bool            `json:"ok"`
	Status    json.RawMessage `json:"status,omitempty"`
	Error     string          `json:"error,omitempty"`
	FetchedAt int64           `json:"fetchedAt"` // unix ms
}

// pollServiceStatuses fetches each registered service's status endpoint
// (if declared) and returns one snapshot per service. Services without a
// Status declaration still appear in the batch so the UI shows them as
// "registered, no status". Services whose fetch fails get OK=false and
// Error populated.
//
// Fetches run concurrently with a per-call ctx; the per-client
// statusFetchTimeout caps each one, so a slow service can't stall the tick.
func pollServiceStatuses(ctx context.Context, enhanced *EnhancedServiceRegistry) []ServiceStatusSnapshot {
	if enhanced == nil {
		return nil
	}
	records := enhanced.All()
	if len(records) == 0 {
		return nil
	}

	out := make([]ServiceStatusSnapshot, len(records))
	var wg sync.WaitGroup
	wg.Add(len(records))
	for i, rec := range records {
		i, rec := i, rec
		go func() {
			defer wg.Done()
			snap := ServiceStatusSnapshot{
				ServiceID: rec.ServiceID,
				Manifest:  rec.Manifest,
				FetchedAt: time.Now().UnixMilli(),
			}
			if rec.Manifest.Status == nil || rec.Manifest.Status.Path == "" {
				snap.OK = true
				out[i] = snap
				return
			}
			client := NewServiceStatusClient(rec.InternalSocket, rec.InternalToken)
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

// pushServiceStatusBatch polls every registered service and emits a single
// onServiceStatusBatch event to the settings window. Skipped when the
// settings window is closed (no consumer) or when the digest matches the
// previously-emitted one — the latter prevents 30/min WebView re-renders
// for a steady-state system.
//
// Safe to call from any goroutine: HTTP polling runs in-place (off-main
// is the expected caller context), then hops to main for the WebView emit
// because WKWebView's evaluateJavaScript requires the main thread.
//
// FetchedAt is excluded from the change-detection digest because it ticks
// every poll and would defeat suppression.
func (a *App) pushServiceStatusBatch() {
	if !a.settingsOpen || a.ipcCtx == nil || a.ipcCtx.Enhanced == nil {
		return
	}
	batch := pollServiceStatuses(a.ctx, a.ipcCtx.Enhanced)
	digest := batchDigest(batch)
	if last := a.lastStatusBatchDigest.Load(); last != nil && *last == digest {
		return
	}
	a.lastStatusBatchDigest.Store(&digest)
	a.platform.DispatchToMain(func() {
		a.emitSettingsEvent("onServiceStatusBatch", batch)
	})
}

// batchDigest fingerprints a status batch excluding the per-tick FetchedAt
// timestamps. Two ticks with identical service / manifest / status / error
// content collapse to the same digest, so the emit is suppressed.
func batchDigest(batch []ServiceStatusSnapshot) [32]byte {
	stripped := make([]ServiceStatusSnapshot, len(batch))
	for i, s := range batch {
		s.FetchedAt = 0
		stripped[i] = s
	}
	raw, _ := json.Marshal(stripped)
	return sha256.Sum256(raw)
}
