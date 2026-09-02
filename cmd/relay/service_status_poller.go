package main

import (
	"github.com/barelyworkingcode/relay/internal/service"
)

// pushServiceStatusBatch polls every registered service and emits a single
// onServiceStatusBatch event, skipping the emit when the digest matches the
// previously-emitted one -- this suppresses WebView re-renders for a
// steady-state system. Safe to call from any goroutine: polling runs
// in-place, then hops to main for the emit because WKWebView's
// evaluateJavaScript requires the main thread.
func (a *App) pushServiceStatusBatch() {
	if !a.settingsOpen.Load() || a.ipcCtx == nil || a.ipcCtx.Enhanced == nil {
		return
	}
	batch := service.PollStatuses(a.ctx, &enhancedServiceAdapter{a.ipcCtx.Enhanced})
	digest := service.BatchDigest(batch)
	if last := a.lastStatusBatchDigest.Load(); last != nil && *last == digest {
		return
	}
	a.lastStatusBatchDigest.Store(&digest)
	a.platform.DispatchToMain(func() {
		a.emitSettingsEvent("onServiceStatusBatch", batch)
	})
}

// enhancedServiceAdapter adapts EnhancedServiceRegistry to service.SnapshotPoller.
type enhancedServiceAdapter struct {
	*EnhancedServiceRegistry
}

func (a *enhancedServiceAdapter) All() []service.ServiceRecord {
	records := a.EnhancedServiceRegistry.All()
	result := make([]service.ServiceRecord, len(records))
	for i, rec := range records {
		result[i] = service.ServiceRecord{
			ServiceID:      rec.ServiceID,
			InternalSocket: rec.InternalSocket,
			InternalToken:  rec.InternalToken,
			Manifest:       rec.Manifest,
		}
	}
	return result
}
