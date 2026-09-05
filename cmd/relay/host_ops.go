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
	// Auditor records a host.probe event per probe; nil is safe (probes
	// still run, just unrecorded), matching AuditRecorder's nil-receiver
	// methods elsewhere.
	Auditor  *audit.AuditRecorder
	OnChange func()
}

func (o *HostOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

func (o *HostOps) List() []config.Host {
	hosts := o.Store.Get().Hosts
	if hosts == nil {
		return []config.Host{}
	}
	return hosts
}

func (o *HostOps) Get(id string) (config.Host, error) {
	h, _ := config.FindHostByID(o.Store.Get(), id)
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
	if err := o.Store.With(func(s *config.Settings) {
		created, createErr = s.AddHost(config.Host{
			Name: f.Name, Target: f.Target, Port: f.Port, IdentityFile: f.IdentityFile,
		})
	}); err != nil {
		return config.Host{}, fmt.Errorf("save host: %w", err)
	}
	if createErr != nil {
		return config.Host{}, invalidHost(createErr.Error())
	}

	probe, _ := sshhost.Probe(ctx, created)
	_ = o.Store.With(func(s *config.Settings) { s.SetHostProbe(created.ID, probe) })
	recordHostProbe(o.Auditor, created, probe)
	if h, _ := config.FindHostByID(o.Store.Get(), created.ID); h != nil {
		created = *h
	}
	o.notify()
	return created, nil
}

// Update patches the host, re-probing only when target, port or
// identity_file changed (docs/ssh-hosts.md) — a rename alone must not pay
// for a round trip to a machine that didn't change.
func (o *HostOps) Update(ctx context.Context, id string, f hostPatchFields) (config.Host, bool, error) {
	before, err := o.Get(id)
	if err != nil {
		return config.Host{}, false, nil
	}

	var updated config.Host
	var found bool
	var updateErr error
	if err := o.Store.With(func(s *config.Settings) {
		updated, found, updateErr = s.UpdateHost(id, config.HostPatch{
			Name: f.Name, Target: f.Target, Port: f.Port, IdentityFile: f.IdentityFile,
		})
	}); err != nil {
		return config.Host{}, false, fmt.Errorf("save host: %w", err)
	}
	if updateErr != nil {
		return config.Host{}, true, invalidHost(updateErr.Error())
	}
	if !found {
		return config.Host{}, false, nil
	}

	if f.touchesConnection(before) {
		probe, _ := sshhost.Probe(ctx, updated)
		_ = o.Store.With(func(s *config.Settings) { s.SetHostProbe(id, probe) })
		recordHostProbe(o.Auditor, updated, probe)
		if h, _ := config.FindHostByID(o.Store.Get(), id); h != nil {
			updated = *h
		}
	}
	o.notify()
	return updated, true, nil
}

// Remove refuses (found=true, refs non-empty) while any project still
// references the host, naming them — settings.RemoveHost's own contract.
func (o *HostOps) Remove(id string) (found bool, refs []string, err error) {
	if err := o.Store.With(func(s *config.Settings) {
		found, refs = s.RemoveHost(id)
	}); err != nil {
		return false, nil, fmt.Errorf("save settings: %w", err)
	}
	if found && len(refs) == 0 {
		o.notify()
	}
	return found, refs, nil
}

// Probe re-runs the ssh discovery and persists the fresh result
// unconditionally — the explicit "do it now" action, unlike Update's
// conditional re-probe.
func (o *HostOps) Probe(ctx context.Context, id string) (config.Host, bool, error) {
	h, err := o.Get(id)
	if err != nil {
		return config.Host{}, false, nil
	}
	probe, _ := sshhost.Probe(ctx, h)
	if err := o.Store.With(func(s *config.Settings) { s.SetHostProbe(id, probe) }); err != nil {
		return config.Host{}, true, fmt.Errorf("save probe result: %w", err)
	}
	recordHostProbe(o.Auditor, h, probe)
	updated, _ := o.Get(id)
	o.notify()
	return updated, true, nil
}

// Disconnect tears down the host's live ControlMaster (ssh -O exit). A
// transport error is swallowed the same way sshhost.Disconnect's own doc
// comment reasons about it: the caller wants "no master after this
// returns", and the hostView's status will read unreachable/unknown from
// the next Check either way.
func (o *HostOps) Disconnect(id string) (config.Host, bool, error) {
	h, err := o.Get(id)
	if err != nil {
		return config.Host{}, false, nil
	}
	_ = sshhost.Disconnect(h)
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
