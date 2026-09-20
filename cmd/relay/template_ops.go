package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
)

var (
	errTemplateNotFound = errors.New("template not found")
	errTemplateExists   = errors.New("template already exists")
	errTemplateInvalid  = errors.New("invalid template")
)

// templateError carries the reason text verbatim; wrapping a sentinel with %w
// would prefix the sentinel's own text (the same shape as hostValidationError).
type templateError struct {
	kind   error
	reason string
}

func (e *templateError) Error() string        { return e.reason }
func (e *templateError) Is(target error) bool { return target == e.kind }

// The id is a URL path segment and a template-kind lookup key, so it is
// narrower than the non-empty check ValidateTerminalTemplate makes.
var templateIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// TemplateOps is the one core behind the HTTP door (template_routes.go) and
// the Settings window's IPC door (ipc_templates.go). Its mutations are
// deliberately not presence-gated: the `configure` class the routes require,
// and the Settings window itself, are the boundary. A template holds sandbox
// folders and a model-key opt-in, so a caller that reaches these routes can
// widen what a sandboxed session may touch; docs/session-host.md says so.
type TemplateOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
}

func (o *TemplateOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

func (o *TemplateOps) Create(ctx context.Context, t config.TerminalTemplate) error {
	return o.put(ctx, t, true)
}

func (o *TemplateOps) Update(ctx context.Context, t config.TerminalTemplate) error {
	return o.put(ctx, t, false)
}

func (o *TemplateOps) put(ctx context.Context, t config.TerminalTemplate, create bool) error {
	if !templateIDPattern.MatchString(t.ID) {
		return &templateError{errTemplateInvalid, "template id must be letters, digits, '.', '_' or '-'"}
	}
	if err := config.ValidateTerminalTemplate(t); err != nil {
		return &templateError{errTemplateInvalid, err.Error()}
	}
	return o.runQueued(ctx, func() error {
		var opErr error
		if err := o.Store.With(func(s *config.Settings) {
			i := slices.IndexFunc(s.TerminalTemplates, func(x config.TerminalTemplate) bool { return x.ID == t.ID })
			switch {
			case create && i >= 0:
				opErr = &templateError{errTemplateExists, fmt.Sprintf("template %q already exists", t.ID)}
			case !create && i < 0:
				opErr = &templateError{errTemplateNotFound, fmt.Sprintf("template %q not found", t.ID)}
			case create:
				s.TerminalTemplates = append(s.TerminalTemplates, t)
			default:
				s.TerminalTemplates[i] = t
			}
		}); err != nil {
			return fmt.Errorf("save template: %w", err)
		}
		return opErr
	})
}

func (o *TemplateOps) Remove(ctx context.Context, id string) error {
	return o.runQueued(ctx, func() error {
		found := false
		if err := o.Store.With(func(s *config.Settings) {
			before := len(s.TerminalTemplates)
			s.TerminalTemplates = slices.DeleteFunc(s.TerminalTemplates, func(x config.TerminalTemplate) bool { return x.ID == id })
			found = len(s.TerminalTemplates) != before
		}); err != nil {
			return fmt.Errorf("save template: %w", err)
		}
		if !found {
			return &templateError{errTemplateNotFound, fmt.Sprintf("template %q not found", id)}
		}
		return nil
	})
}
