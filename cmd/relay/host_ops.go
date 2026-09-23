package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

var (
	errHostNotFound = errors.New("host not found")
	errHostInvalid  = errors.New("invalid host")
)

// Carries the reason text verbatim, the same trick mcpValidationError and
// serviceValidationError use: wrapping with %w would prefix the sentinel's
// own text.
type hostValidationError struct{ reason string }

func (e *hostValidationError) Error() string        { return e.reason }
func (e *hostValidationError) Is(target error) bool { return target == errHostInvalid }

func invalidHost(reason string) error {
	return &hostValidationError{reason: reason}
}

// hostFields is the transport-agnostic create body; both the HTTP route and
// the IPC handler decode into it.
type hostFields struct {
	Name         string `json:"name"`
	Target       string `json:"target"`
	Port         int    `json:"port,omitempty"`
	IdentityFile string `json:"identity_file,omitempty"`
}

// hostPatchFields is the transport-agnostic patch body: nil means "not in
// the request", matching project.UpdateFields' discipline.
type hostPatchFields struct {
	Name         *string `json:"name,omitempty"`
	Target       *string `json:"target,omitempty"`
	Port         *int    `json:"port,omitempty"`
	IdentityFile *string `json:"identity_file,omitempty"`
}

func (f hostPatchFields) touchesConnection(before config.Host) bool {
	if f.Target != nil && *f.Target != before.Target {
		return true
	}
	if f.Port != nil && *f.Port != before.Port {
		return true
	}
	if f.IdentityFile != nil && *f.IdentityFile != before.IdentityFile {
		return true
	}
	return false
}

// HostOps is the one core behind the HTTP door (host_routes.go) and the
// WebView IPC door (ipc_hosts.go), mirroring how ProjectOps and McpOps are
// shared. Unlike ProjectOps, host mutations are not presence-gated: a host
// record names where ssh may reach, not what a tool may call, and the
// control-plane `configure` capability class (checked by the route
// registrar before either door's handler runs) is the boundary docs/ssh-hosts.md
// specifies for it.
type HostOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	// Auditor records a host.probe event per probe; nil is safe (probes
	// still run, just unrecorded), matching AuditRecorder's nil-receiver
	// methods elsewhere.
	Auditor *audit.AuditRecorder
}

func (o *HostOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

func (o *HostOps) List() []config.Host {
	hosts := config.DisplaySettings(o.Store).Hosts
	if hosts == nil {
		return []config.Host{}
	}
	return hosts
}

func (o *HostOps) Get(id string) (config.Host, error) {
	h, _ := config.FindHostByID(config.FreshSettings(o.Store), id)
	if h == nil {
		return config.Host{}, fmt.Errorf("%w: %s", errHostNotFound, id)
	}
	return *h, nil
}

// Create adds the host, then probes it synchronously (docs/ssh-hosts.md:
// "runs a probe synchronously, result included") so the caller sees whether
// the host is reachable without a second round trip.
func (o *HostOps) Create(ctx context.Context, f hostFields) (config.Host, error) {
	// Checked before the receiver is ever touched, the same discipline
	// ServiceOps.Create and McpOps.Add follow: a malformed request (name or
	// target empty) must return without dereferencing o.Store, so a caller
	// holding a zero-value *HostOps for every other purpose (a test harness
	// exercising just the decode path) never panics.
	if f.Name == "" {
		return config.Host{}, invalidHost("host name is required")
	}
	if f.Target == "" {
		return config.Host{}, invalidHost("host target is required")
	}

	var created config.Host
	var createErr error
	if err := o.runQueued(ctx, func() error {
		if err := o.Store.With(func(s *config.Settings) {
			created, createErr = s.AddHost(config.Host{Name: f.Name, Target: f.Target, Port: f.Port, IdentityFile: f.IdentityFile})
		}); err != nil {
			return fmt.Errorf("save host: %w", err)
		}
		if createErr != nil {
			return invalidHost(createErr.Error())
		}
		return nil
	}); err != nil {
		return config.Host{}, err
	}

	probe, _ := sshhost.Probe(ctx, created)
	recordHostProbe(o.Auditor, created, probe)
	committed, found, err := o.commitProbe(created.ID, created.ProbeGeneration, probe)
	if err != nil {
		return created, err
	}
	if found {
		created = committed
	}
	return created, nil
}

// Update patches the host, re-probing only when target, port or
// identity_file changed (docs/ssh-hosts.md) — a rename alone must not pay
// for a round trip to a machine that didn't change.
func (o *HostOps) Update(ctx context.Context, id string, f hostPatchFields) (config.Host, bool, error) {
	var updated config.Host
	var found bool
	var connectionChanged bool
	var updateErr error
	if err := o.runQueued(ctx, func() error {
		if err := o.Store.With(func(s *config.Settings) {
			updated, found, connectionChanged, updateErr = s.UpdateHostAndReserveProbe(id, config.HostPatch{Name: f.Name, Target: f.Target, Port: f.Port, IdentityFile: f.IdentityFile})
		}); err != nil {
			return fmt.Errorf("save host: %w", err)
		}
		if updateErr != nil {
			if errors.Is(updateErr, config.ErrHostProbeGenerationOverflow) {
				return updateErr
			}
			return invalidHost(updateErr.Error())
		}
		return nil
	}); err != nil {
		return config.Host{}, found, err
	}
	if !found {
		return config.Host{}, false, nil
	}
	if connectionChanged {
		probe, _ := sshhost.Probe(ctx, updated)
		recordHostProbe(o.Auditor, updated, probe)
		committed, current, err := o.commitProbe(updated.ID, updated.ProbeGeneration, probe)
		if err != nil {
			return updated, true, err
		}
		if current {
			updated = committed
		}
	}
	return updated, true, nil
}

// Remove refuses (found=true, refs non-empty) while any project still
// references the host, naming them — settings.RemoveHost's own contract.
func (o *HostOps) Remove(ctx context.Context, id string) (found bool, refs []string, err error) {
	err = o.runQueued(ctx, func() error {
		if err := o.Store.With(func(s *config.Settings) { found, refs = s.RemoveHost(id) }); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return found, refs, nil
}

// Probe re-runs the ssh discovery and persists the fresh result
// unconditionally — the explicit "do it now" action, unlike Update's
// conditional re-probe.
func (o *HostOps) Probe(ctx context.Context, id string) (config.Host, bool, error) {
	var h config.Host
	var found bool
	if err := o.runQueued(ctx, func() error {
		var reserveErr error
		if err := o.Store.With(func(s *config.Settings) { h, found, reserveErr = s.ReserveHostProbe(id) }); err != nil {
			return fmt.Errorf("save host: %w", err)
		}
		return reserveErr
	}); err != nil {
		return config.Host{}, found, err
	}
	if !found {
		return config.Host{}, false, nil
	}
	probe, _ := sshhost.Probe(ctx, h)
	recordHostProbe(o.Auditor, h, probe)
	updated, current, err := o.commitProbe(id, h.ProbeGeneration, probe)
	if err != nil {
		return config.Host{}, true, err
	}
	return updated, current, nil
}

func (o *HostOps) commitProbe(id string, generation uint64, probe config.HostProbe) (config.Host, bool, error) {
	var committed config.Host
	var found bool
	err := o.runQueued(context.Background(), func() error {
		if err := o.Store.With(func(s *config.Settings) {
			committed, found = s.SetHostProbeIfGeneration(id, generation, probe)
			// Seed Shell + Claude Code on the first good probe, in the same
			// mutation so no reader sees a probed host without them. Only an
			// empty list is seeded: an operator's edits, however few, stand.
			if found && probe.OK && len(committed.TerminalTemplates) == 0 {
				committed.TerminalTemplates = config.DefaultHostTemplates(probe)
				s.SetHostTemplates(id, committed.TerminalTemplates)
			}
		}); err != nil {
			return fmt.Errorf("save probe result: %w", err)
		}
		return nil
	})
	return committed, found, err
}

// Disconnect tears down the host's live ControlMaster (ssh -O exit). A
// transport error is swallowed the same way sshhost.Disconnect's own doc
// comment reasons about it: the caller wants "no master after this
// returns", and the hostView's status will read unreachable/unknown from
// the next Check either way.
func (o *HostOps) Disconnect(id string) (config.Host, bool, error) { //nolint:unparam // deliberate: matches Probe's (host, found, error) shape so host_routes.go's two action handlers stay parallel
	h, err := o.Get(id)
	if err != nil {
		return config.Host{}, false, nil //nolint:nilerr // Get's only error is not-found; found=false is the signal here, not the error return
	}
	_ = sshhost.Disconnect(h)
	invalidateHostStatusCache(h.ID)
	return h, true, nil
}

// hostProbeAuditArgs is what host.probe's Args field carries: the target
// reached and, on success, what was discovered. Reusing Args (rather than
// adding one AuditEvent field per datum) matches how a tool call's own
// Args already carries call-specific structured detail this log does not
// otherwise need to index on.
type hostProbeAuditArgs struct {
	HostID     string `json:"host_id"`
	Name       string `json:"name"`
	Target     string `json:"target"`
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	NodePath   string `json:"node_path,omitempty"`
	ClaudePath string `json:"claude_path,omitempty"`
}

// recordHostProbe is the audit instrumentation for one ssh probe
// (docs/ssh-hosts.md: "The probe emits an audit event per run"). The actor
// is always Operator: a probe only ever runs from something already
// authenticated to relay's control plane (the tray, or a `configure`-class
// credential), never from a tool call.
func recordHostProbe(r *audit.AuditRecorder, h config.Host, probe config.HostProbe) {
	if !r.Enabled() {
		return
	}
	args, _ := json.Marshal(hostProbeAuditArgs{
		HostID: h.ID, Name: h.Name, Target: h.Target,
		OS: probe.OS, Arch: probe.Arch, NodePath: probe.NodePath, ClaudePath: probe.ClaudePath,
	})
	outcome := audit.AuditOutcomeOK
	if !probe.OK {
		outcome = audit.AuditOutcomeError
	}
	r.Record(audit.AuditEvent{
		ID:      audit.NewAuditID(),
		TS:      time.Now(),
		Event:   audit.AuditEventHostProbe,
		Actor:   audit.AuditActor{Kind: audit.AuditActorOperator, Auth: audit.AuditAuthNone},
		Outcome: outcome,
		Error:   probe.Error,
		Args:    args,
	})
}
