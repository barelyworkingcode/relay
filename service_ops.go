package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"relaygo/presence"
)

var (
	errServiceNotFound = errors.New("service not found")
	errServiceInvalid  = errors.New("invalid service")
	// errServiceProcess means the settings mutation COMMITTED and only the
	// process side effect failed. Callers must not treat it as a failed
	// mutation: the returned config is the record that landed, and reporting
	// it as an error alone loses a service the operator can see in the file.
	errServiceProcess = errors.New("service process")
)

// Carries the reason text verbatim because the settings UI shows it as-is.
// Wrapping with %w instead would prefix the sentinel's own text.
type serviceValidationError struct{ reason string }

func (e *serviceValidationError) Error() string        { return e.reason }
func (e *serviceValidationError) Is(target error) bool { return target == errServiceInvalid }

func invalidService(reason string) error {
	return &serviceValidationError{reason: reason}
}

// JSON tags match ipcServiceMsg's because the settings UI's JS depends on them.
type serviceFields struct {
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url,omitempty"`
	// FrontendConsumer is a pointer for the same reason projectUpdateFields'
	// pointers are: nil means "leave whatever is already stored alone" (an
	// edit form that never mentions it must not silently re-enable
	// front-door credential injection for a backend that opted out via
	// `service register --no-frontend-creds`); non-nil sets it explicitly.
	FrontendConsumer *bool `json:"frontend_consumer,omitempty"`
}

func (f serviceFields) toConfig(id string) ServiceConfig {
	return ServiceConfig{
		ID:               id,
		DisplayName:      f.DisplayName,
		Command:          f.Command,
		Args:             f.Args,
		Env:              secretMapFromPlain(f.Env),
		WorkingDir:       f.WorkingDir,
		Autostart:        f.Autostart,
		URL:              f.URL,
		FrontendConsumer: f.FrontendConsumer,
	}
}

// presenceDigest binds a service.register grant to exactly the record being
// registered or updated (§6.4), id included so a grant answered for one
// service id cannot be spent on another.
func (f serviceFields) presenceDigest(id string) presence.Digest {
	b := presence.NewDigestBuilder("service.register").
		StringField("id", true, id).
		StringField("display_name", true, f.DisplayName).
		StringField("command", true, f.Command).
		StringField("working_dir", true, f.WorkingDir).
		StringField("url", true, f.URL).
		StringSeqField("args", true, f.Args).
		StringMapField("env", true, f.Env).
		BoolField("autostart", true, f.Autostart)
	if f.FrontendConsumer != nil {
		b.BoolField("frontend_consumer", true, *f.FrontendConsumer)
	} else {
		b.BoolField("frontend_consumer", false, false)
	}
	return b.Build()
}

// The one core behind both the HTTP door (service_routes.go) and the WebView
// IPC door (ipc_services.go); neither holds logic beyond decoding a request
// and spelling the result.
type ServiceOps struct {
	Store    SettingsStore
	Registry ServiceManager
	// Gate is the presence check Create, Update and Remove demand before
	// they touch the store (ADR-017 decisions 3 and 4): a service's
	// `command` is what relay will run, the caller's choice (ADR-015
	// decision 1). A nil Gate refuses all three — see requireGate.
	Gate *presence.Gate
	// Issuance records the config_change every register/unregister leaves
	// (§7.5) and is the hard dependency §7.4 checks before Gate.
	Issuance IssuanceAuditor
	OnChange func()
}

func (o *ServiceOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

func (o *ServiceOps) List() []ServiceConfig {
	svcs := o.Store.Get().Services
	if svcs == nil {
		return []ServiceConfig{}
	}
	return svcs
}

func (o *ServiceOps) Get(id string) (ServiceConfig, error) {
	svc, _ := o.Store.Get().findServiceByID(id)
	if svc == nil {
		return ServiceConfig{}, fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	return *svc, nil
}

func (o *ServiceOps) Create(ctx context.Context, f serviceFields, via, credID string) (ServiceConfig, error) {
	id := slugify(f.DisplayName)
	if id == "" {
		return ServiceConfig{}, invalidService("display name is required")
	}
	if f.Command == "" {
		return ServiceConfig{}, invalidService("command is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return ServiceConfig{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("register a service that runs %s", f.Command))
	if err != nil {
		return ServiceConfig{}, err
	}

	config := f.toConfig(id)
	if err := o.Store.With(func(s *Settings) { s.UpsertService(config) }); err != nil {
		return ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, grant.ID()); err != nil {
		slog.Error("service registered but not recorded in the audit log", "id", id, "error", err)
	}

	var startErr error
	if config.Autostart {
		startErr = o.Registry.Start(&config)
	}
	o.notify()
	if startErr != nil {
		return config, fmt.Errorf("%w: autostart failed: %v", errServiceProcess, startErr)
	}
	return config, nil
}

// Restart is conditional on current state, not on the request: starting a
// stopped service as a side effect of editing it would surprise a caller who
// asked only for an edit.
func (o *ServiceOps) Update(ctx context.Context, id string, f serviceFields, via, credID string) (ServiceConfig, error) {
	if f.Command == "" {
		return ServiceConfig{}, invalidService("command is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return ServiceConfig{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("update the service %q to run %s", id, f.Command))
	if err != nil {
		return ServiceConfig{}, err
	}

	// IsRunning is sampled before the commit, same as the config merge below;
	// what makes this race-safe is not when wasRunning is read but that a
	// stale true never reaches Reload, because the callback below resolves the
	// id atomically with the write and declines when it is gone.
	wasRunning := o.Registry.IsRunning(id)

	var config ServiceConfig
	if err := withDeclinable(o.Store, func(s *Settings) error {
		existing, idx := s.findServiceByID(id)
		if idx < 0 {
			return fmt.Errorf("%w: %s", errServiceNotFound, id)
		}
		config = f.toConfig(id)
		// FrontendConsumer stays whatever the request says when the request
		// says anything (toConfig already carried it through); an edit form
		// that omits the field leaves the existing value alone rather than
		// clearing it, which is what f.FrontendConsumer == nil already means.
		if f.FrontendConsumer == nil {
			config.FrontendConsumer = existing.FrontendConsumer
		}
		s.UpdateService(config)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) {
			return ServiceConfig{}, err
		}
		return ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, grant.ID()); err != nil {
		slog.Error("service updated but not recorded in the audit log", "id", id, "error", err)
	}

	var reloadErr error
	if wasRunning {
		reloadErr = o.Registry.Reload(id, &config)
	}
	o.notify()
	if reloadErr != nil {
		return config, fmt.Errorf("%w: restart failed: %v", errServiceProcess, reloadErr)
	}
	return config, nil
}

func (o *ServiceOps) Remove(ctx context.Context, id, via, credID string) error {
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return err
	}
	grant, err := requireGate(o.Gate, ctx, "service.unregister",
		singleStringDigest("service.unregister", "id", id), fmt.Sprintf("unregister the service %q", id))
	if err != nil {
		return err
	}

	if err := withDeclinable(o.Store, func(s *Settings) error {
		if _, idx := s.findServiceByID(id); idx < 0 {
			return fmt.Errorf("%w: %s", errServiceNotFound, id)
		}
		s.RemoveService(id)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) {
			return err
		}
		return fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, grant.ID()); err != nil {
		slog.Error("service unregistered but not recorded in the audit log", "id", id, "error", err)
	}
	o.Registry.Stop(id)
	o.notify()
	return nil
}

func (o *ServiceOps) SetAutostart(id string, on bool) error {
	if err := withDeclinable(o.Store, func(s *Settings) error {
		if _, idx := s.findServiceByID(id); idx < 0 {
			return fmt.Errorf("%w: %s", errServiceNotFound, id)
		}
		s.SetServiceAutostart(id, on)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) {
			return err
		}
		return fmt.Errorf("save service: %w", err)
	}
	o.notify()
	return nil
}

func (o *ServiceOps) Start(id string) error {
	svc, _ := o.Store.Get().findServiceByID(id)
	if svc == nil {
		return fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	if err := o.Registry.Start(svc); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	o.notify()
	return nil
}

// Blocks until the process has actually exited. Every ServiceOps method is
// synchronous by construction so the "does this need a goroutine" decision
// belongs to the envelope: HTTP already has a per-request goroutine, and IPC
// wraps blocking calls to keep the WebView responsive.
//
// Refusing an id absent from settings but PRESENT in the registry would leave
// a live process no door can stop — a service unregistered by the CLI while
// still running is exactly that state.
func (o *ServiceOps) Stop(id string) error {
	_, idx := o.Store.Get().findServiceByID(id)
	if idx < 0 && !o.Registry.IsRunning(id) {
		return fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	o.Registry.Stop(id)
	o.notify()
	return nil
}
