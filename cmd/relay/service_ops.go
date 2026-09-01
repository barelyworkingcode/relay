package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
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

// JSON tags match ipcServiceMsg's because the settings UI's JS depends on
// them, except ID (the CLI's own field: the Settings window edits a record
// in place and passes id out of band, never re-derives it) and the four
// pointer fields below.
//
// WorkingDir, URL and Autostart are pointers for the same reason
// FrontendConsumer already is: on Update, nil means "leave whatever is
// already stored alone" so a CLI flag the operator did not repeat is not
// read as "clear this." A door that always represents the record's complete
// state (the Settings window, a well-behaved HTTP client) sets all three on
// every request, present or not, exactly as it always could; only a request
// that genuinely omits a field -- the CLI flag left off the command line --
// gets the preserving nil. Args and Env need no such change: encoding/json
// already leaves a slice or map nil when its key is absent, which is the
// same absent-vs-empty distinction the pointer gives the scalar fields.
type serviceFields struct {
	ID          string            `json:"id,omitempty"`
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  *string           `json:"working_dir,omitempty"`
	Autostart   *bool             `json:"autostart,omitempty"`
	URL         *string           `json:"url,omitempty"`
	// FrontendConsumer is a pointer for the same reason project.UpdateFields'
	// pointers are: nil means "leave whatever is already stored alone" (an
	// edit form that never mentions it must not silently re-enable
	// front-door credential injection for a backend that opted out via
	// `service register --no-frontend-creds`); non-nil sets it explicitly.
	FrontendConsumer *bool `json:"frontend_consumer,omitempty"`
}

// resolvedID is the id Create/Update commit under: the caller's explicit
// choice when given, slugify(DisplayName) otherwise -- the same fallback
// every other door (HTTP, IPC) always used.
func (f serviceFields) resolvedID() string {
	if f.ID != "" {
		return f.ID
	}
	return slugify(f.DisplayName)
}

// toConfig applies Create's own semantics: a pointer field left nil (the
// flag never given) becomes its zero value, same as before these fields
// were pointers. Update's absent-preserves-existing behaviour is layered on
// top of this in ServiceOps.Update, not here -- toConfig alone cannot know
// what "existing" is.
func (f serviceFields) toConfig(id string) config.ServiceConfig {
	var workingDir string
	if f.WorkingDir != nil {
		workingDir = *f.WorkingDir
	}
	var url string
	if f.URL != nil {
		url = *f.URL
	}
	var autostart bool
	if f.Autostart != nil {
		autostart = *f.Autostart
	}
	return config.ServiceConfig{
		ID:               id,
		DisplayName:      f.DisplayName,
		Command:          f.Command,
		Args:             f.Args,
		Env:              secretMapFromPlain(f.Env),
		WorkingDir:       workingDir,
		Autostart:        autostart,
		URL:              url,
		FrontendConsumer: f.FrontendConsumer,
	}
}

// presenceDigest binds a service.register grant to exactly the record being
// registered or updated (§6.4), id included so a grant answered for one
// service id cannot be spent on another. Every field that Update treats as
// absent-preserves-existing (working_dir, url, autostart, args, env,
// frontend_consumer) is absent-aware here too, on the same footing as
// frontend_consumer already was: the presence bit is itself part of what a
// grant binds to, so a request that leaves a field alone and one that sets
// it to that field's zero value produce different digests, and a grant
// approved for one can never be redeemed for the other.
func (f serviceFields) presenceDigest(id string) presence.Digest {
	b := presence.NewDigestBuilder("service.register").
		StringField("id", true, id).
		StringField("display_name", true, f.DisplayName).
		StringField("command", true, f.Command)
	if f.WorkingDir != nil {
		b.StringField("working_dir", true, *f.WorkingDir)
	} else {
		b.StringField("working_dir", false, "")
	}
	if f.URL != nil {
		b.StringField("url", true, *f.URL)
	} else {
		b.StringField("url", false, "")
	}
	b.StringSeqField("args", f.Args != nil, f.Args)
	b.StringMapField("env", f.Env != nil, f.Env)
	if f.Autostart != nil {
		b.BoolField("autostart", true, *f.Autostart)
	} else {
		b.BoolField("autostart", false, false)
	}
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
	Store    config.SettingsStore
	Registry ServiceManager
	// Gate is the presence check Create and Update demand before they
	// touch the store (ADR-017 decisions 3 and 4): a service's `command`
	// is what relay will run, the caller's choice (ADR-015 decision 1).
	// Remove is deliberately ungated (ADR-018 step 3): removal narrows,
	// never widens, and stopping a running service is already ungated
	// `configure` (POST /api/services/{id}/stop) — only the record's
	// deletion is new here, and it still calls requireIssuanceAuditor
	// below. A nil Gate refuses Create and Update — see requireGate.
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

func (o *ServiceOps) List() []config.ServiceConfig {
	svcs := o.Store.Get().Services
	if svcs == nil {
		return []config.ServiceConfig{}
	}
	return svcs
}

func (o *ServiceOps) Get(id string) (config.ServiceConfig, error) {
	svc, _ := config.FindServiceByID(o.Store.Get(), id)
	if svc == nil {
		return config.ServiceConfig{}, fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	return *svc, nil
}

func (o *ServiceOps) Create(ctx context.Context, f serviceFields, via, credID string) (config.ServiceConfig, error) {
	id := f.resolvedID()
	if id == "" {
		return config.ServiceConfig{}, invalidService("display name is required")
	}
	if f.Command == "" {
		return config.ServiceConfig{}, invalidService("command is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.ServiceConfig{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("register the service %q (%s) that runs %s", f.DisplayName, id, f.Command))
	if err != nil {
		return config.ServiceConfig{}, err
	}

	cfg := f.toConfig(id)
	if err := o.Store.With(func(s *config.Settings) { s.UpsertService(cfg) }); err != nil {
		return config.ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, grant.ID()); err != nil {
		slog.Error("service registered but not recorded in the audit log", "id", id, "error", err)
	}

	var startErr error
	if cfg.Autostart {
		startErr = o.Registry.Start(&cfg)
	}
	o.notify()
	if startErr != nil {
		return cfg, fmt.Errorf("%w: autostart failed: %v", errServiceProcess, startErr)
	}
	return cfg, nil
}

// Restart is conditional on current state, not on the request: starting a
// stopped service as a side effect of editing it would surprise a caller who
// asked only for an edit.
func (o *ServiceOps) Update(ctx context.Context, id string, f serviceFields, via, credID string) (config.ServiceConfig, error) {
	if f.Command == "" {
		return config.ServiceConfig{}, invalidService("command is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.ServiceConfig{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("update the service %q to run %s", id, f.Command))
	if err != nil {
		return config.ServiceConfig{}, err
	}

	// IsRunning is sampled before the commit, same as the config merge below;
	// what makes this race-safe is not when wasRunning is read but that a
	// stale true never reaches Reload, because the callback below resolves the
	// id atomically with the write and declines when it is gone.
	wasRunning := o.Registry.IsRunning(id)

	var cfg config.ServiceConfig
	if err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
		existing, idx := config.FindServiceByID(s, id)
		if idx < 0 {
			return fmt.Errorf("%w: %s", errServiceNotFound, id)
		}
		cfg = f.toConfig(id)
		// Every pointer/nil-able field on serviceFields means the same thing
		// on Update: the request didn't mention it, so the stored value
		// carries forward unchanged rather than being reset to that field's
		// zero value. FrontendConsumer already worked this way; the rest
		// (added to close the same hole for --workdir, --url, --autostart,
		// --env and --args) follow it exactly.
		if f.FrontendConsumer == nil {
			cfg.FrontendConsumer = existing.FrontendConsumer
		}
		if f.WorkingDir == nil {
			cfg.WorkingDir = existing.WorkingDir
		}
		if f.URL == nil {
			cfg.URL = existing.URL
		}
		if f.Autostart == nil {
			cfg.Autostart = existing.Autostart
		}
		if f.Args == nil {
			cfg.Args = existing.Args
		}
		if f.Env == nil {
			cfg.Env = existing.Env
		}
		s.UpdateService(cfg)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) {
			return config.ServiceConfig{}, err
		}
		return config.ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, grant.ID()); err != nil {
		slog.Error("service updated but not recorded in the audit log", "id", id, "error", err)
	}

	var reloadErr error
	if wasRunning {
		reloadErr = o.Registry.Reload(id, &cfg)
	}
	o.notify()
	if reloadErr != nil {
		return cfg, fmt.Errorf("%w: restart failed: %v", errServiceProcess, reloadErr)
	}
	return cfg, nil
}

func (o *ServiceOps) Remove(ctx context.Context, id, via, credID string) error {
	// No requireGate call here (ADR-018 step 3, §5.2): unregistering only
	// narrows what the caller already reaches -- stopping the process is
	// already ungated configure, and re-registering under the same id
	// still hits Create/Update's gate. requireIssuanceAuditor and
	// recordConfigChange below still run unconditionally, so the act is
	// still detected -- only presence_id comes back empty.
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return err
	}

	if err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
		if _, idx := config.FindServiceByID(s, id); idx < 0 {
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
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, ""); err != nil {
		slog.Error("service unregistered but not recorded in the audit log", "id", id, "error", err)
	}
	o.Registry.Stop(id)
	o.notify()
	return nil
}

func (o *ServiceOps) SetAutostart(id string, on bool) error {
	if err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
		if _, idx := config.FindServiceByID(s, id); idx < 0 {
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
	svc, _ := config.FindServiceByID(o.Store.Get(), id)
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
	_, idx := config.FindServiceByID(o.Store.Get(), id)
	if idx < 0 && !o.Registry.IsRunning(id) {
		return fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	o.Registry.Stop(id)
	o.notify()
	return nil
}
