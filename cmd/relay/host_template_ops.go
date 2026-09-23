package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
)

// HostTemplateOps is TemplateOps's counterpart for a host's own templates
// (Host.TerminalTemplates, docs/ssh-hosts.md): the one core behind the HTTP
// door (host_template_routes.go) and the Hosts tab's IPC door
// (ipc_host_templates.go). Like TemplateOps it is not presence-gated. A host
// template runs on the host, can't opt into a sandbox or a model key
// (config.ValidateHostTemplate), so it widens nothing on the console; the
// `configure` class and the Settings window are the boundary.
type HostTemplateOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
}

func hostTemplateNotFound(hostID string) error {
	return &templateError{errHostNotFound, fmt.Sprintf("host %q not found", hostID)}
}

func (o *HostTemplateOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

// List returns hostID's templates as stored — invalid ones included, so the
// Settings window can show and fix them. Never nil.
func (o *HostTemplateOps) List(hostID string) ([]config.TerminalTemplate, error) {
	h, _ := config.FindHostByID(config.FreshSettings(o.Store), hostID)
	if h == nil {
		return nil, hostTemplateNotFound(hostID)
	}
	if h.TerminalTemplates == nil {
		return []config.TerminalTemplate{}, nil
	}
	return h.TerminalTemplates, nil
}

func (o *HostTemplateOps) Create(ctx context.Context, hostID string, t config.TerminalTemplate) error {
	return o.put(ctx, hostID, t, true)
}

func (o *HostTemplateOps) Update(ctx context.Context, hostID string, t config.TerminalTemplate) error {
	return o.put(ctx, hostID, t, false)
}

func (o *HostTemplateOps) put(ctx context.Context, hostID string, t config.TerminalTemplate, create bool) error {
	if !templateIDPattern.MatchString(t.ID) {
		return &templateError{errTemplateInvalid, "template id must be letters, digits, '.', '_' or '-'"}
	}
	// SetHostTemplates does not validate, so this is the only check a
	// template gets before it is persisted.
	if err := config.ValidateHostTemplate(t); err != nil {
		return &templateError{errTemplateInvalid, err.Error()}
	}
	return o.mutate(ctx, hostID, func(ts []config.TerminalTemplate) ([]config.TerminalTemplate, error) {
		i := slices.IndexFunc(ts, func(x config.TerminalTemplate) bool { return x.ID == t.ID })
		switch {
		case create && i >= 0:
			return nil, &templateError{errTemplateExists, fmt.Sprintf("template %q already exists", t.ID)}
		case !create && i < 0:
			return nil, &templateError{errTemplateNotFound, fmt.Sprintf("template %q not found", t.ID)}
		case create:
			return append(ts, t), nil
		default:
			ts[i] = t
			return ts, nil
		}
	})
}

func (o *HostTemplateOps) Remove(ctx context.Context, hostID, id string) error {
	return o.mutate(ctx, hostID, func(ts []config.TerminalTemplate) ([]config.TerminalTemplate, error) {
		out := slices.DeleteFunc(ts, func(x config.TerminalTemplate) bool { return x.ID == id })
		if len(out) == len(ts) {
			return nil, &templateError{errTemplateNotFound, fmt.Sprintf("template %q not found", id)}
		}
		return out, nil
	})
}

// mutate runs edit over a copy of hostID's templates inside one queued
// settings mutation. An edit error leaves the host untouched.
func (o *HostTemplateOps) mutate(ctx context.Context, hostID string, edit func([]config.TerminalTemplate) ([]config.TerminalTemplate, error)) error {
	return o.runQueued(ctx, func() error {
		var opErr error
		if err := o.Store.With(func(s *config.Settings) {
			h, _ := config.FindHostByID(s, hostID)
			if h == nil {
				opErr = hostTemplateNotFound(hostID)
				return
			}
			next, err := edit(slices.Clone(h.TerminalTemplates))
			if err != nil {
				opErr = err
				return
			}
			s.SetHostTemplates(hostID, next)
		}); err != nil {
			return fmt.Errorf("save host template: %w", err)
		}
		return opErr
	})
}
