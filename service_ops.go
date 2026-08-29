package main

import (
	"errors"
	"fmt"
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
}

func (f serviceFields) toConfig(id string) ServiceConfig {
	return ServiceConfig{
		ID:          id,
		DisplayName: f.DisplayName,
		Command:     f.Command,
		Args:        f.Args,
		Env:         f.Env,
		WorkingDir:  f.WorkingDir,
		Autostart:   f.Autostart,
		URL:         f.URL,
	}
}

// The one core behind both the HTTP door (service_routes.go) and the WebView
// IPC door (ipc_services.go); neither holds logic beyond decoding a request
// and spelling the result.
type ServiceOps struct {
	Store    SettingsStore
	Registry ServiceManager
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

func (o *ServiceOps) Create(f serviceFields) (ServiceConfig, error) {
	id := slugify(f.DisplayName)
	if id == "" {
		return ServiceConfig{}, invalidService("display name is required")
	}
	if f.Command == "" {
		return ServiceConfig{}, invalidService("command is required")
	}

	config := f.toConfig(id)
	if err := o.Store.With(func(s *Settings) { s.UpsertService(config) }); err != nil {
		return ServiceConfig{}, fmt.Errorf("save service: %w", err)
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
func (o *ServiceOps) Update(id string, f serviceFields) (ServiceConfig, error) {
	if f.Command == "" {
		return ServiceConfig{}, invalidService("command is required")
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
		// FrontendConsumer is set by `service register --no-frontend-creds`, never
		// by an edit form, and UpdateService replaces the whole record. Dropping it
		// here silently re-enables front-door credential injection for a backend
		// that opted out.
		config.FrontendConsumer = existing.FrontendConsumer
		s.UpdateService(config)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) {
			return ServiceConfig{}, err
		}
		return ServiceConfig{}, fmt.Errorf("save service: %w", err)
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

func (o *ServiceOps) Remove(id string) error {
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
