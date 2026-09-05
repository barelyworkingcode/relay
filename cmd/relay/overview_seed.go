package main

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
)

// nativeMcpHealthView is HealthEvent projected for the WebView: Err is an
// error interface (opaque to json across restarts of the same process, and
// unmarshal-able to nothing meaningful), so it becomes a plain string here,
// and Connected rides along from Manager.IsConnected because the Overview
// and MCP Servers tabs need "is it up right now", not only "what was the
// last reported transition".
type nativeMcpHealthView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Connected   bool   `json:"connected"`
	// State is one of mcpbroker's Health* constants, or "" when this MCP has
	// never reported a death, restart or abandonment.
	State      string `json:"state,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	DowntimeMS int64  `json:"downtime_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// buildMcpHealth returns one entry per configured external MCP, not only
// the ones that have ever reported a health event -- an MCP relay has never
// seen die still needs a row so "connected" and "no news yet" render
// differently from "relay doesn't know this MCP exists".
func (a *App) buildMcpHealth(s *config.Settings) map[string]nativeMcpHealthView {
	a.mcpHealthMu.Lock()
	defer a.mcpHealthMu.Unlock()
	out := make(map[string]nativeMcpHealthView, len(s.ExternalMcps))
	for _, m := range s.ExternalMcps {
		view := nativeMcpHealthView{
			ID:          m.ID,
			DisplayName: m.DisplayName,
			Connected:   a.extMgr.IsConnected(m.ID),
		}
		if ev, ok := a.lastHealth[m.ID]; ok {
			view.State = ev.State
			view.Attempt = ev.Attempt
			if ev.Downtime > 0 {
				view.DowntimeMS = ev.Downtime.Milliseconds()
			}
			if ev.Err != nil {
				view.Error = ev.Err.Error()
			}
		}
		out[m.ID] = view
	}
	return out
}

// recordHealthEvent is SetHealthObserver's callback, alongside
// recordMcpSupervision: the audit trail gets the event, and this keeps the
// live snapshot the Overview and MCP Servers tabs read. Runs on whatever
// goroutine the supervisor happens to be on (SetHealthObserver's doc
// comment), so the WebView emit hops to main the same way
// pushServiceStatusBatch's does.
func (a *App) recordHealthEvent(ev mcpbroker.HealthEvent) {
	a.mcpHealthMu.Lock()
	a.lastHealth[ev.ID] = ev
	a.mcpHealthMu.Unlock()
	a.platform.DispatchToMain(func() {
		a.emitSettingsEvent("onMcpHealth", a.buildMcpHealth(a.store.Get()))
	})
}

// nativeServiceRuntimeView is service.ServiceRuntime projected for the
// WebView: StartedAt as RFC3339 rather than Go's default time encoding, to
// match every other timestamp the settings UI already renders.
type nativeServiceRuntimeView struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

func serviceRuntimeToNativeView(rt map[string]service.ServiceRuntime) map[string]nativeServiceRuntimeView {
	out := make(map[string]nativeServiceRuntimeView, len(rt))
	for id, r := range rt {
		out[id] = nativeServiceRuntimeView{PID: r.PID, StartedAt: r.StartedAt.UTC().Format(rfc3339Milli)}
	}
	return out
}

// rfc3339Milli matches the precision every other seeded timestamp in this
// package uses (enrolments, audit) -- second precision would round two
// services started in the same second onto the same displayed time.
const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"

// pathsView is the Overview footer's "Reveal" targets: relay's config
// directory (settings.json, ca.crt, ca.key.sealed) and its log directory
// (relay's own log, the audit log, and every managed service's).
type pathsView struct {
	Config string `json:"config"`
	Logs   string `json:"logs"`
}

// paths resolves pathsView. Logs is best-effort: serviceLogDir() only fails
// if the directory can't be created, which would already have broken
// logging elsewhere, so an empty string here just means "Reveal logs" can't
// find anything to open -- not a reason to fail building the rest of the
// payload.
func (a *App) paths() pathsView {
	logs, _ := serviceLogDir()
	return pathsView{Config: a.configDir, Logs: logs}
}

// overviewSeed bundles every payload the Overview tab needs beyond what
// projects/services/MCPs/hosts already carry -- passed to
// renderSettingsDocument for the first paint and rebuilt identically for
// pushFullSettings so an open window and a fresh one never disagree.
type overviewSeed struct {
	MCPHealth      map[string]nativeMcpHealthView
	ServiceRuntime map[string]nativeServiceRuntimeView
	// SealStatus is store.SealStatus()'s reason, or "" when the sealed
	// store is healthy. A pointer would let Go's json render `null`
	// natively; an empty string does the same job with one fewer type for
	// callers to reason about, and the UI already treats "" as "no issue"
	// for every other status string it renders (auditStatus.Path, etc.).
	SealStatus string
	Version    string
	Paths      pathsView
}

func (a *App) buildOverviewSeed(s *config.Settings) overviewSeed {
	return overviewSeed{
		MCPHealth:      a.buildMcpHealth(s),
		ServiceRuntime: serviceRuntimeToNativeView(a.registry.Runtime()),
		SealStatus:     a.sealStatus,
		Version:        buildVersion,
		Paths:          a.paths(),
	}
}
