package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

var (
	errPersistProjectNotFound = errors.New("project not found")
	errPersistSessionNotFound = errors.New("persistent session not found")
	errPersistNameInvalid     = errors.New("invalid persistent session name")
	errPersistNoTmux          = errors.New("host has no tmux")
	errPersistHostUnreachable = errors.New("host unreachable")
)

// persistSessionError carries its reason verbatim, the same shape as
// templateError: the sentinel decides the status, the reason is the body.
type persistSessionError struct {
	kind   error
	reason string
}

func (e *persistSessionError) Error() string        { return e.reason }
func (e *persistSessionError) Is(target error) bool { return target == e.kind }

// PersistentSession is one of a project's persistent host terminals as the
// host's tmux reports it (docs/ssh-hosts.md, Persistent terminals).
type PersistentSession struct {
	Name         string `json:"name"`
	TemplateID   string `json:"template_id"`
	N            int    `json:"n"`
	Created      int64  `json:"created"`
	Attached     int    `json:"attached"`
	AttachedHere bool   `json:"attached_here"`
}

// PersistentSessionOps lists and kills the tmux sessions relay started for a
// hosted project's persist templates. It stores nothing: the host's own
// `tmux ls` is the record (docs/ssh-hosts.md). Not presence-gated — a kill
// ends work the operator started on a host they configured, and widens
// nothing; the `configure` class on the route is the boundary.
type PersistentSessionOps struct {
	Store config.SettingsStore
	// ActiveNames returns the names of the live relay terminals, which for a
	// persist terminal is its tmux session name. nil reads as none attached.
	ActiveNames func(projectID string) map[string]bool
}

// List returns projectID's persistent sessions on its host. Never nil.
func (o *PersistentSessionOps) List(ctx context.Context, projectID string) ([]PersistentSession, error) {
	h, err := o.projectHost(projectID)
	if err != nil {
		return nil, err
	}
	sessions, err := listTmux(ctx, h)
	if err != nil {
		return nil, err
	}
	var active map[string]bool
	if o.ActiveNames != nil {
		active = o.ActiveNames(projectID)
	}
	out := []PersistentSession{}
	for _, s := range sessions {
		templateID, n, ok := config.ParseProjectPersistSessionName(s.Name, projectID)
		if !ok {
			continue
		}
		out = append(out, PersistentSession{
			Name:         s.Name,
			TemplateID:   templateID,
			N:            n,
			Created:      s.Created,
			Attached:     s.Attached,
			AttachedHere: active[s.Name],
		})
	}
	return out, nil
}

// Kill ends one of projectID's persistent sessions on its host. The name is
// confirmed present in the host's list first because tmux resolves -t by
// prefix when no session matches exactly: killing a gone relay-…-1 would
// otherwise end relay-…-10. tmux's exact-match `=name` form would close that
// in one step, but psmux ignores it.
func (o *PersistentSessionOps) Kill(ctx context.Context, projectID, name string) error {
	if _, _, _, ok := config.ParsePersistSessionName(name); !ok {
		return &persistSessionError{errPersistNameInvalid, fmt.Sprintf("%q is not a relay persistent session name", name)}
	}
	h, err := o.projectHost(projectID)
	if err != nil {
		return err
	}
	if _, _, ok := config.ParseProjectPersistSessionName(name, projectID); !ok {
		return &persistSessionError{errPersistSessionNotFound, fmt.Sprintf("session %q does not belong to project %q", name, projectID)}
	}
	sessions, err := listTmux(ctx, h)
	if err != nil {
		return err
	}
	found := false
	for _, s := range sessions {
		if s.Name == name {
			found = true
			break
		}
	}
	if !found {
		return &persistSessionError{errPersistSessionNotFound, fmt.Sprintf("session %q not found on host %s", name, h.Name)}
	}
	if err := sshhost.KillTmuxSession(ctx, h, h.EffectiveTmuxPath(), name); err != nil {
		return &persistSessionError{errPersistHostUnreachable, err.Error()}
	}
	return nil
}

// projectHost resolves projectID to its host, refusing a project that is
// unknown, not hosted, or whose host has no tmux to ask.
func (o *PersistentSessionOps) projectHost(projectID string) (config.Host, error) {
	s := config.FreshSettings(o.Store)
	proj, _ := config.FindProjectByID(s, projectID)
	if proj == nil || !proj.IsHosted() {
		return config.Host{}, &persistSessionError{errPersistProjectNotFound, fmt.Sprintf("hosted project %q not found", projectID)}
	}
	h, _ := config.FindHostByID(s, proj.HostID)
	if h == nil {
		return config.Host{}, &persistSessionError{errPersistProjectNotFound, fmt.Sprintf("host %q of project %q not found", proj.HostID, projectID)}
	}
	return *h, nil
}

// persistSessionNames returns every relay persistent session name on h,
// whichever project it belongs to — the input NextPersistSessionN takes.
func persistSessionNames(ctx context.Context, h config.Host) ([]string, error) {
	sessions, err := listTmux(ctx, h)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, s := range sessions {
		if _, _, _, ok := config.ParsePersistSessionName(s.Name); ok {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

func listTmux(ctx context.Context, h config.Host) ([]sshhost.TmuxSession, error) {
	tmuxPath := h.EffectiveTmuxPath()
	if tmuxPath == "" {
		return nil, &persistSessionError{errPersistNoTmux, fmt.Sprintf("host %s has no tmux: install it or set tmux_path", h.Name)}
	}
	sessions, err := sshhost.ListTmuxSessions(ctx, h, tmuxPath)
	if err != nil {
		return nil, &persistSessionError{errPersistHostUnreachable, err.Error()}
	}
	return sessions, nil
}
