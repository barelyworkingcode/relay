package main

// Regression coverage for the ADR-015 verify finding: ServiceOps.Update read
// and validated outside the settings lock and wrote inside it, so a DELETE
// landing in that gap made Update report success for nothing persisted and
// relaunch the caller-supplied command for a service settings no longer
// named. sorHookStore and sorRaceRegistry force that interleaving
// deterministically (a synchronous callback, not real goroutines) so these
// tests never flake. See enrolment_ops.go's enrolment.Create/enrolment.Revoke
// for the pattern being restored here: validate at commit time, not before.

import (
	"context"
	"errors"
	"github.com/barelyworkingcode/relay/internal/config"
	"testing"
)

// sorHookStore wraps a real SettingsStore and runs preWith exactly once,
// immediately before the wrapped store's own With -- the same point a
// concurrent writer's commit would land relative to a caller that read and
// validated before calling With. Every other method is promoted straight
// through the embedded interface.
type sorHookStore struct {
	config.SettingsStore
	preWith func()
}

func (h *sorHookStore) With(fn func(*config.Settings)) error {
	if h.preWith != nil {
		trigger := h.preWith
		h.preWith = nil
		trigger()
	}
	return h.SettingsStore.With(fn)
}

// sorRaceRegistry is a ServiceManager whose IsRunning runs a caller-supplied
// trigger exactly once -- the deterministic stand-in for "a concurrent
// Remove completes between Update's read and its commit" that the ADR-015
// finding calls for, in place of a timing-dependent goroutine race.
type sorRaceRegistry struct {
	noopServiceManager
	trigger  func(id string) bool
	started  []string
	reloaded []string
	stopped  []string
}

func (r *sorRaceRegistry) IsRunning(id string) bool {
	if r.trigger != nil {
		t := r.trigger
		r.trigger = nil
		return t(id)
	}
	return false
}

func (r *sorRaceRegistry) Start(c *config.ServiceConfig) error {
	r.started = append(r.started, c.ID)
	return nil
}

func (r *sorRaceRegistry) Reload(id string, _ *config.ServiceConfig) error {
	r.reloaded = append(r.reloaded, id)
	return nil
}

func (r *sorRaceRegistry) Stop(id string) {
	r.stopped = append(r.stopped, id)
}

func sorSeedService(t *testing.T, store config.SettingsStore, cfg config.ServiceConfig) {
	t.Helper()
	if err := store.With(func(s *config.Settings) { s.UpsertService(cfg) }); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// TestServiceOpsRace_UpdateLosesToConcurrentRemove drives the exact
// interleaving from the bug report: Update samples IsRunning (stale-true),
// a DELETE lands and fully commits before Update's own write, and Update
// must answer errServiceNotFound rather than 200-with-a-record -- and it
// must never hand the stale wasRunning to Reload, which would relaunch the
// caller-supplied command for a service settings no longer has.
func TestServiceOpsRace_UpdateLosesToConcurrentRemove(t *testing.T) {
	store := newCLISandboxStore(t)
	sorSeedService(t, store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/old"})

	reg := &sorRaceRegistry{}
	ops := &ServiceOps{Store: store, Registry: reg, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	reg.trigger = func(id string) bool {
		if err := ops.Remove(context.Background(), id, auditViaIPC, ""); err != nil {
			t.Fatalf("concurrent remove: %v", err)
		}
		return true // stale: it WAS running before the remove committed
	}

	_, err := ops.Update(context.Background(), "svc", serviceFields{Command: "/bin/new"}, auditViaIPC, "")
	if !errors.Is(err, errServiceNotFound) {
		t.Fatalf("Update err = %v, want errServiceNotFound", err)
	}
	if svcs := store.Get().Services; len(svcs) != 0 {
		t.Fatalf("nothing should be persisted by a losing Update; got %+v", svcs)
	}
	if len(reg.started) != 0 || len(reg.reloaded) != 0 {
		t.Fatalf("registry must never Start or Reload a removed service; started=%v reloaded=%v", reg.started, reg.reloaded)
	}
}

// TestServiceOpsRace_RemoveDuringConcurrentRemove covers ServiceOps.Remove's
// own check-then-act shape: two DELETEs for the same id land close enough
// that the second's existence check would have passed against pre-removal
// state. Only the removal that actually commits may report success.
func TestServiceOpsRace_RemoveDuringConcurrentRemove(t *testing.T) {
	store := newCLISandboxStore(t)
	sorSeedService(t, store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/x"})

	reg := &sorRaceRegistry{}
	hooked := &sorHookStore{SettingsStore: store}
	ops := &ServiceOps{Store: hooked, Registry: reg, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	other := &ServiceOps{Store: store, Registry: reg, Gate: ops.Gate, Issuance: ops.Issuance}

	hooked.preWith = func() {
		if err := other.Remove(context.Background(), "svc", auditViaIPC, ""); err != nil {
			t.Fatalf("concurrent remove: %v", err)
		}
	}

	if err := ops.Remove(context.Background(), "svc", auditViaIPC, ""); !errors.Is(err, errServiceNotFound) {
		t.Fatalf("Remove err = %v, want errServiceNotFound", err)
	}
}

// TestServiceOpsRace_SetAutostartDuringConcurrentRemove covers
// ServiceOps.SetAutostart's check-then-act shape: a DELETE lands after the
// autostart call's own (former) existence check but before its write.
func TestServiceOpsRace_SetAutostartDuringConcurrentRemove(t *testing.T) {
	store := newCLISandboxStore(t)
	sorSeedService(t, store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/x"})

	reg := &sorRaceRegistry{}
	hooked := &sorHookStore{SettingsStore: store}
	ops := &ServiceOps{Store: hooked, Registry: reg, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	other := &ServiceOps{Store: store, Registry: reg, Gate: ops.Gate, Issuance: ops.Issuance}

	hooked.preWith = func() {
		if err := other.Remove(context.Background(), "svc", auditViaIPC, ""); err != nil {
			t.Fatalf("concurrent remove: %v", err)
		}
	}

	if err := ops.SetAutostart("svc", true); !errors.Is(err, errServiceNotFound) {
		t.Fatalf("SetAutostart err = %v, want errServiceNotFound", err)
	}
}

// TestServiceOpsRace_McpRemoveDuringConcurrentRemove covers McpOps.Remove's
// identical check-then-act shape, same mechanism as the ServiceOps.Remove
// case above.
func TestServiceOpsRace_McpRemoveDuringConcurrentRemove(t *testing.T) {
	store := newCLISandboxStore(t)
	if err := store.With(func(s *config.Settings) {
		s.UpsertExternalMcp(config.ExternalMcp{ID: "mcp1", DisplayName: "MCP One", Command: "/bin/x"})
	}); err != nil {
		t.Fatalf("seed mcp: %v", err)
	}

	hooked := &sorHookStore{SettingsStore: store}
	ops := &McpOps{Store: hooked, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	other := &McpOps{Store: store, Gate: ops.Gate, Issuance: ops.Issuance}

	hooked.preWith = func() {
		if err := other.Remove(context.Background(), "mcp1", auditViaIPC, ""); err != nil {
			t.Fatalf("concurrent remove: %v", err)
		}
	}

	if err := ops.Remove(context.Background(), "mcp1", auditViaIPC, ""); !errors.Is(err, errMcpNotFound) {
		t.Fatalf("Remove err = %v, want errMcpNotFound", err)
	}
}
