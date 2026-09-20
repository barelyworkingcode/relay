package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/service"
)

var (
	errServiceNotFound = errors.New("service not found")
	errServiceInvalid  = errors.New("invalid service")
	// errServiceProcess means the settings mutation COMMITTED and only the
	// process side effect failed. Callers must not treat it as a failed
	// mutation: the returned config is the record that landed, and reporting
	// it as an error alone loses a service the operator can see in the file.
	errServiceProcess = errors.New("service process")
	// errServiceChangedDuringApproval means the record moved between the
	// presence decision and the queued commit so the request now needs an
	// approval it never obtained. Nothing was written; retry against the
	// current record.
	errServiceChangedDuringApproval = errors.New("service changed while the request was being approved; retry the request")
)

// Carries the reason text verbatim because the settings UI shows it as-is.
// Wrapping with %w instead would prefix the sentinel's own text.
type serviceValidationError struct{ reason string }

func (e *serviceValidationError) Error() string        { return e.reason }
func (e *serviceValidationError) Is(target error) bool { return target == errServiceInvalid }

// serviceConfigError carries the exact text the service config editor shows,
// tagged with the sentinel callers branch on.
type serviceConfigError struct {
	reason string
	kind   error
}

func (e *serviceConfigError) Error() string        { return e.reason }
func (e *serviceConfigError) Is(target error) bool { return target == e.kind }

func invalidService(reason string) error {
	return &serviceValidationError{reason: reason}
}

// JSON tags match ipcServiceMsg's because the settings UI's JS depends on
// them, except ID (the CLI's own field: the Settings window edits a record
// in place and passes id out of band, never re-derives it) and the four
// pointer fields below.
//
// WorkingDir, URL, Autostart and Capabilities are pointers because on
// Update, nil means "leave whatever is
// already stored alone" so a CLI flag the operator did not repeat is not
// read as "clear this." A door that always represents the record's complete
// state (the Settings window, a well-behaved HTTP client) sets all three on
// every request, present or not, exactly as it always could; only a request
// that genuinely omits a field -- the CLI flag left off the command line --
// gets the preserving nil. Args and Env need no such change: encoding/json
// already leaves a slice or map nil when its key is absent, which is the
// same absent-vs-empty distinction the pointer gives the scalar fields.
//
// Env's value type is *string, not string: a sealed value can never be
// displayed, so the Settings window can never resubmit one, and a key it
// shows only a masked placeholder for is carried on the wire as present
// with a null value -- "no new value for this key" -- rather than omitted.
// Omitting a key entirely still means what it always has for this
// whole-map-replace field: that key is gone. resolveServiceEnv is the one
// place that reads this map; see its own comment for the three cases.
type serviceFields struct {
	ID          string             `json:"id,omitempty"`
	DisplayName string             `json:"display_name"`
	Command     string             `json:"command"`
	Args        []string           `json:"args"`
	Env         map[string]*string `json:"env"`
	WorkingDir  *string            `json:"working_dir,omitempty"`
	Autostart   *bool              `json:"autostart,omitempty"`
	URL         *string            `json:"url,omitempty"`
	// Capabilities nil means "leave whatever is stored alone" on Update, so an
	// edit form that never mentions it cannot change what a service's launch
	// identity may do; on Create it means the empty set. A non-nil pointer to
	// an empty slice sets the empty set explicitly.
	Capabilities *[]config.ServiceCapability `json:"capabilities,omitempty"`
	// AllowedModels follows Capabilities' same nil-preserves-existing shape
	// on Update; on Create nil means the empty grant (config.ServiceConfig's
	// own default: no models). A non-nil pointer to an empty slice sets that
	// same empty grant explicitly.
	AllowedModels *[]string `json:"allowed_models,omitempty"`
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
// what "existing" is. Env is deliberately NOT set here: resolving it needs
// resolveServiceEnv's existing-vs-null logic (and can fail, which toConfig's
// signature has no way to report), so every caller sets cfg.Env itself
// immediately after calling this.
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
	capabilities := []config.ServiceCapability{}
	if f.Capabilities != nil {
		capabilities = append(capabilities, *f.Capabilities...)
	}
	// trimModelIDs runs here, the one place every door's AllowedModels
	// funnels through on its way into a config.ServiceConfig, so a stray
	// leading/trailing space from any caller never reaches storage.
	// serviceAllowedModelsChanged applies the same trim before comparing, so
	// an untrimmed resend of an already-stored id reads as "unchanged," not
	// "changed." config.ServiceConfig.Validate independently refuses what
	// trimming reduces to empty, so a whitespace-only entry is caught rather
	// than silently dropped.
	var allowedModels []string
	if f.AllowedModels != nil {
		allowedModels = trimModelIDs(*f.AllowedModels)
	}
	return config.ServiceConfig{
		ID:            id,
		DisplayName:   f.DisplayName,
		Command:       f.Command,
		Args:          f.Args,
		WorkingDir:    workingDir,
		Autostart:     autostart,
		URL:           url,
		Capabilities:  capabilities,
		AllowedModels: allowedModels,
	}
}

// envSerializationArtifact is the exact string a stale, unpatched client's
// `key + '=' + value` string concatenation produces when value is the
// sealed envelope object the API used to return (JavaScript's default
// Object-to-string coercion) -- refusing exactly this literal is what stops
// a client that has not picked up the masked-env editor from silently
// overwriting a real secret with eight characters that were never a value
// anyone typed.
const envSerializationArtifact = "[object Object]"

// resolveServiceEnv turns the wire's per-key optional-value map into the
// sealed-in-memory map config.ServiceConfig.Env stores, given whatever env
// the record already holds (nil on Create, where there is nothing to keep).
// Three cases, one per key in requested:
//
//   - value is non-nil: an explicit new value -- sealed fresh, refused if it
//     is envSerializationArtifact.
//   - value is nil (JSON null): no new value. The Settings window sends
//     this for every key it only shows a masked placeholder for, and the
//     stored sealed value for that key -- untouched, never round-tripped
//     through plaintext -- carries forward. A key with no stored value to
//     carry forward (Create, or a key requested but never previously
//     stored) becomes an empty secret rather than inventing content.
//   - key absent from requested entirely: Env replaces the whole map (same
//     as before this field existed), so an omitted key is removed, exactly
//     the way an operator deleting a row in the editor means it.
func resolveServiceEnv(existing map[string]config.Secret, requested map[string]*string) (map[string]config.Secret, error) {
	out := make(map[string]config.Secret, len(requested))
	for k, v := range requested {
		if v == nil {
			if s, ok := existing[k]; ok {
				out[k] = s
				continue
			}
			out[k] = config.NewSecret("")
			continue
		}
		if *v == envSerializationArtifact {
			return nil, invalidService(fmt.Sprintf("env %q: refusing a value that looks like a stale client's stringified object (%q); re-enter the value", k, *v))
		}
		out[k] = config.NewSecret(*v)
	}
	return out, nil
}

// serviceEnvChanges answers Update's gating question directly off the wire
// shapes, without resolving a single Secret: a key present in existing but
// dropped from requested is a removal, an explicit (non-nil) value is
// always counted as a set even if it happens to match what is already
// stored (the same literal reading project.grant's own allow_external/
// access widen checks use -- an operator who retyped a value chose to pin
// it, not to leave it alone), and a null value for a key that was never
// stored is still a change (a new, empty-valued key). requested == nil
// means the request does not touch env at all.
func serviceEnvChanges(existing map[string]config.Secret, requested map[string]*string) bool {
	if requested == nil {
		return false
	}
	for k := range existing {
		if _, ok := requested[k]; !ok {
			return true
		}
	}
	for k, v := range requested {
		if v != nil {
			return true
		}
		if _, ok := existing[k]; !ok {
			return true
		}
	}
	return false
}

// setEqual reports whether a and b hold the same elements in the same
// multiplicity, ignoring order (multiset equality, which implies set
// equality: two genuinely different sets can never compare equal here, only
// a duplicate-heavy resend of an identical set can compare unequal to
// itself reordered -- the safe direction for a presence-gate decision).
// Used to decide "did this field actually change" for fields whose stored
// and requested representations may list the same members in a different
// order (capabilities, allowed_models) -- a reorder alone must never read as
// a change and reach the gate needlessly.
//
// Constrained to cmp.Ordered for slices.Sort's sake; every instantiation
// today is a string-kinded type (config.ServiceCapability, string), so
// float64's NaN-breaks-sort-and-equality edge never applies -- if a future
// caller ever instantiates this with a float type, that guarantee would
// need re-checking.
func setEqual[T cmp.Ordered](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	a2, b2 := slices.Clone(a), slices.Clone(b)
	slices.Sort(a2)
	slices.Sort(b2)
	return slices.Equal(a2, b2)
}

// serviceCapabilitiesChanged reports whether requested's capability set
// differs from existing's at all, added or dropped. An earlier revision of
// this function (serviceAddsCapability, addition-only: dropping one only
// narrows what a service's launch identity may do, ADR-018 decision 1) was
// correct while only an operator-minted credential could reach this route.
// Since eve's frontend launch identity was granted execute
// (plan-broker-and-sessions.md's F1 decision), any frontend-capable service
// can call Update for ANY service, not just itself, so a narrowing that used
// to be self-inflicted operator convenience is now a same-uid service able
// to silently strip a sibling service's own capabilities -- an availability
// exposure, gated now on the user's own explicit call
// (STATUS-relay-security.md).
func serviceCapabilitiesChanged(existing, requested []config.ServiceCapability) bool {
	return !setEqual(existing, requested)
}

// modelAllowedWildcard is the literal that means "every model" in an
// AllowedModels list (docs/model-endpoint.md), matching internal/modelbroker's
// own spelling. Not imported from that package: this file has no other
// reason to depend on it, and the spelling is part of the wire contract, not
// an implementation detail that package could change out from under this one.
const modelAllowedWildcard = "*"

// trimModelIDs trims every entry. The one funnel every door's AllowedModels
// passes through on the way into a config.ServiceConfig (serviceFields.
// toConfig) and the one this package's own change check re-derives from
// before comparing, so a caller's stray leading/trailing space never reads
// as a different model id than the trimmed one already on record.
func trimModelIDs(ids []string) []string {
	if ids == nil {
		return nil
	}
	out := make([]string, len(ids))
	for i, m := range ids {
		out[i] = strings.TrimSpace(m)
	}
	return out
}

// serviceAllowedModelsChanged reports whether requested's model grant
// differs from existing's in what it actually reaches, trimming both first
// so whitespace alone never counts as a change. If both already hold the
// wildcard, nothing else listed alongside it changes what's reachable
// (every model, either way), so a raw list difference there is not
// gate-worthy -- the one case a plain set-equality check would wrongly gate
// on (TestServiceOps_ExistingWildcardNeverGatesFurtherAdditions). Every
// other change -- adding an id, adding the wildcard, dropping an id, or
// dropping the wildcard down to a scoped set -- gates. An earlier revision
// (serviceWidensAllowedModels, addition-only) did not gate a narrowing; see
// serviceCapabilitiesChanged's comment for why that's no longer the read
// this route can afford now that a frontend-capable service, not only an
// operator, can reach it for any service's record.
func serviceAllowedModelsChanged(existing, requested []string) bool {
	existing = trimModelIDs(existing)
	requested = trimModelIDs(requested)
	if slices.Contains(existing, modelAllowedWildcard) && slices.Contains(requested, modelAllowedWildcard) {
		return false
	}
	return !setEqual(existing, requested)
}

// serviceUpdateNeedsGate decides whether an Update actually needs the
// presence gate, judging each field against what f carries rather than
// gating unconditionally the way every prior Update did. command, args,
// working_dir, url and autostart have no narrower reading -- any actual
// change to what the service runs or how is "the caller chooses what runs"
// regardless of direction, so those gate on simple inequality. A request
// that changes nothing at all -- a resend of the exact stored record, which
// the Settings window's save button always sends -- needs no gate either.
//
// display_name, capabilities and allowed_models now ALL gate on any actual
// change, in either direction. This file's earlier revision treated
// capabilities/allowed_models as narrow-safe (dropping one only shrinks what
// the launched identity may do) and never looked at display_name at all --
// correct while only an operator-minted credential could reach this route.
// Since eve's frontend launch identity was granted execute
// (plan-broker-and-sessions.md's F1 decision), any frontend-capable service
// can call this route for ANY service, not just itself: a frontend service
// silently renaming a sibling service (an operator-facing spoofing
// primitive in the tray menu and `service list`) or silently narrowing
// another service's own capabilities (an availability primitive: it could
// strip relay-llm's model_host capability with no prompt) are both real
// exposure that widening a background service's reach introduced. Gating
// narrowing again reintroduces the original operator-convenience cost this
// file's history already paid to avoid -- accepted deliberately, as the
// user's own explicit call once the trade-off was raised, not overlooked.
func serviceUpdateNeedsGate(existing config.ServiceConfig, f serviceFields) bool {
	if f.Command != existing.Command {
		return true
	}
	if f.Args != nil && !slices.Equal(f.Args, existing.Args) {
		return true
	}
	if f.WorkingDir != nil && *f.WorkingDir != existing.WorkingDir {
		return true
	}
	if f.URL != nil && *f.URL != existing.URL {
		return true
	}
	if f.Autostart != nil && *f.Autostart != existing.Autostart {
		return true
	}
	if f.DisplayName != existing.DisplayName {
		return true
	}
	if serviceEnvChanges(existing.Env, f.Env) {
		return true
	}
	if f.Capabilities != nil && serviceCapabilitiesChanged(existing.Capabilities, *f.Capabilities) {
		return true
	}
	if f.AllowedModels != nil && serviceAllowedModelsChanged(existing.AllowedModels, *f.AllowedModels) {
		return true
	}
	return false
}

// envDigestField renders env's per-key optional-value shape into
// RawJSONMapField's input: json.Marshal of a *string produces `null` for a
// nil pointer and a quoted string otherwise, so "no new value" and "set to
// this value" -- including the empty string, a legitimate value -- encode
// to distinguishable, length-prefixed bytes. Reusing StringMapField instead
// would need a sentinel plaintext value to stand for "unchanged," and any
// sentinel is a string an operator could also legitimately type.
func envDigestField(env map[string]*string) map[string][]byte {
	if env == nil {
		return nil
	}
	out := make(map[string][]byte, len(env))
	for k, v := range env {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}

// presenceDigest binds a service.register grant to exactly the record being
// registered or updated (§6.4), id included so a grant answered for one
// service id cannot be spent on another. Every field that Update treats as
// absent-preserves-existing (working_dir, url, autostart, args, env,
// capabilities) is absent-aware here too: the presence bit is itself part of what a
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
	b.RawJSONMapField("env", f.Env != nil, envDigestField(f.Env))
	if f.Autostart != nil {
		b.BoolField("autostart", true, *f.Autostart)
	} else {
		b.BoolField("autostart", false, false)
	}
	if f.Capabilities != nil {
		names := make([]string, 0, len(*f.Capabilities))
		for _, c := range *f.Capabilities {
			names = append(names, string(c))
		}
		b.StringSeqField("capabilities", true, names)
	} else {
		b.StringSeqField("capabilities", false, nil)
	}
	if f.AllowedModels != nil {
		b.StringSeqField("allowed_models", true, *f.AllowedModels)
	} else {
		b.StringSeqField("allowed_models", false, nil)
	}
	return b.Build()
}

// The one core behind both the HTTP door (service_routes.go) and the WebView
// IPC door (ipc_services.go); neither holds logic beyond decoding a request
// and spelling the result.
type ServiceOps struct {
	Store    config.SettingsStore
	Registry service.Manager
	// Enhanced holds the manifests SaveConfigFile resolves a service's
	// declared config file from. nil refuses every config save.
	Enhanced *EnhancedServiceRegistry
	Queue    *config.CommandQueue
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

func (o *ServiceOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.Do(ctx, func(context.Context) error { return fn() })
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

// serviceApproval is what the presence prompt, run before the queue, obtained.
// Create and update bind the same digest (serviceFields.presenceDigest), so an
// approval for either covers the other for the same request.
type serviceApproval struct {
	granted    bool
	presenceID string
}

// Register decides whether a request creates or updates twice: before the
// queue, from a snapshot, only to know whether to prompt (a human prompt must
// not hold the lane); and again inside the queue against the live record,
// which is the decision that counts. The caller must not resolve this choice
// from its own read.
func (o *ServiceOps) Register(ctx context.Context, f serviceFields, via, credID string) (cfg config.ServiceConfig, err error) {
	id := f.resolvedID()
	var approval serviceApproval
	if snapshot, _ := config.FindServiceByID(o.Store.Get(), id); snapshot != nil {
		if err := o.preflightUpdate(id, f); err != nil {
			return config.ServiceConfig{}, err
		}
		if approval, err = o.approveUpdate(ctx, id, f, *snapshot); err != nil {
			return config.ServiceConfig{}, err
		}
	} else {
		if err := o.preflightCreate(id, f); err != nil {
			return config.ServiceConfig{}, err
		}
		if approval, err = o.approveCreate(ctx, id, f); err != nil {
			return config.ServiceConfig{}, err
		}
	}
	err = o.runQueued(ctx, func() error {
		if svc, _ := config.FindServiceByID(o.Store.Get(), id); svc != nil {
			if err := o.preflightUpdate(id, f); err != nil {
				return err
			}
			cfg, err = o.commitUpdate(id, f, via, credID, approval)
			return err
		}
		if err := o.preflightCreate(id, f); err != nil {
			return err
		}
		cfg, err = o.commitCreate(id, f, via, credID, approval)
		return err
	})
	return cfg, err
}

func (o *ServiceOps) Create(ctx context.Context, f serviceFields, via, credID string) (cfg config.ServiceConfig, err error) {
	id := f.resolvedID()
	if err := o.preflightCreate(id, f); err != nil {
		return config.ServiceConfig{}, err
	}
	approval, err := o.approveCreate(ctx, id, f)
	if err != nil {
		return config.ServiceConfig{}, err
	}
	err = o.runQueued(ctx, func() error {
		var innerErr error
		cfg, innerErr = o.commitCreate(id, f, via, credID, approval)
		return innerErr
	})
	return cfg, err
}

// preflightCreate and preflightUpdate refuse a request before any prompt. They
// have no side effects, so the queued step repeats them against a request
// whose create-versus-update outcome may have flipped.
func (o *ServiceOps) preflightCreate(id string, f serviceFields) error {
	if id == "" {
		return invalidService("display name is required")
	}
	if id == config.RelaySessionsServiceID {
		// SH §2.1: relay-sessions is built in, synthesized at every start,
		// never user-registered. Refusing this id here is what actually
		// closes the loophole validateCapabilities alone leaves open --
		// capability sessions is refused for every OTHER id already, so the
		// only way a register call could ever hold it is by also claiming
		// this reserved one.
		return invalidService(fmt.Sprintf("%q is relay's built-in session host and cannot be registered", id))
	}
	if f.Command == "" {
		return invalidService("command is required")
	}
	return requireIssuanceAuditor(o.Issuance)
}

func (o *ServiceOps) preflightUpdate(id string, f serviceFields) error {
	if id == config.RelaySessionsServiceID {
		return invalidService(fmt.Sprintf("%q is relay's built-in session host and cannot be edited", id))
	}
	if f.Command == "" {
		return invalidService("command is required")
	}
	return requireIssuanceAuditor(o.Issuance)
}

func (o *ServiceOps) approveCreate(ctx context.Context, id string, f serviceFields) (serviceApproval, error) {
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("register the service %q (%s) that runs %s", f.DisplayName, id, f.Command))
	if err != nil {
		return serviceApproval{}, err
	}
	return serviceApproval{granted: true, presenceID: grant.ID()}, nil
}

// approveUpdate prompts only when the update changes something serviceUpdateNeedsGate
// names, judged against snapshot. snapshot is the zero record when the id is
// unknown, which over-prompts and then loses to errServiceNotFound in the commit.
func (o *ServiceOps) approveUpdate(ctx context.Context, id string, f serviceFields, snapshot config.ServiceConfig) (serviceApproval, error) {
	if !serviceUpdateNeedsGate(snapshot, f) {
		return serviceApproval{}, nil
	}
	grant, err := requireGate(o.Gate, ctx, "service.register", f.presenceDigest(id),
		fmt.Sprintf("update the service %q to run %s", id, f.Command))
	if err != nil {
		return serviceApproval{}, err
	}
	return serviceApproval{granted: true, presenceID: grant.ID()}, nil
}

// Runs on the lane: no prompt, no network. A create always needs the approval;
// one that arrives without it means the record vanished after the snapshot
// said update-without-prompt.
func (o *ServiceOps) commitCreate(id string, f serviceFields, via, credID string, approval serviceApproval) (config.ServiceConfig, error) {
	if !approval.granted {
		return config.ServiceConfig{}, fmt.Errorf("%w: %s", errServiceChangedDuringApproval, id)
	}

	cfg := f.toConfig(id)
	env, err := resolveServiceEnv(nil, f.Env)
	if err != nil {
		return config.ServiceConfig{}, err
	}
	cfg.Env = env
	if err := cfg.Validate(); err != nil {
		return config.ServiceConfig{}, invalidService(err.Error())
	}
	if err := o.Store.With(func(s *config.Settings) { s.UpsertService(cfg) }); err != nil {
		return config.ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, approval.presenceID); err != nil {
		slog.Error("service registered but not recorded in the audit log", "id", id, "error", err)
	}

	var startErr error
	if cfg.Autostart {
		startErr = o.Registry.Start(&cfg)
	}
	o.notify()
	if startErr != nil {
		return cfg, fmt.Errorf("%w: autostart failed: %w", errServiceProcess, startErr)
	}
	return cfg, nil
}

// Restart is conditional on current state, not on the request: starting a
// stopped service as a side effect of editing it would surprise a caller who
// asked only for an edit.
//
// The prompt runs before the queue against a snapshot of the record; the
// queued commit re-derives serviceUpdateNeedsGate on the live record and
// refuses with errServiceChangedDuringApproval when it now needs an approval
// that was not obtained. A stale snapshot that over-prompted needs no handling.
func (o *ServiceOps) Update(ctx context.Context, id string, f serviceFields, via, credID string) (cfg config.ServiceConfig, err error) {
	if err := o.preflightUpdate(id, f); err != nil {
		return config.ServiceConfig{}, err
	}

	// This is deliberate: an empty display_name is not refused here, before
	// the prompt. It must lose to errServiceNotFound for an id that was never
	// there (TestOpsThatFindNothingWriteNothing), and this read of the
	// snapshot is the wrong place to give it a second meaning.
	var snapshot config.ServiceConfig
	if e, _ := config.FindServiceByID(o.Store.Get(), id); e != nil {
		snapshot = *e
	}
	approval, err := o.approveUpdate(ctx, id, f, snapshot)
	if err != nil {
		return config.ServiceConfig{}, err
	}
	err = o.runQueued(ctx, func() error {
		var innerErr error
		cfg, innerErr = o.commitUpdate(id, f, via, credID, approval)
		return innerErr
	})
	return cfg, err
}

// Runs on the lane: no prompt, no network.
func (o *ServiceOps) commitUpdate(id string, f serviceFields, via, credID string, approval serviceApproval) (config.ServiceConfig, error) {
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
		if !approval.granted && serviceUpdateNeedsGate(*existing, f) {
			return fmt.Errorf("%w: %s", errServiceChangedDuringApproval, id)
		}
		cfg = f.toConfig(id)
		// Every pointer/nil-able field on serviceFields means the same thing
		// on Update: the request didn't mention it, so the stored value
		// carries forward unchanged rather than being reset to that field's
		// zero value.
		if f.Capabilities == nil {
			cfg.Capabilities = existing.Capabilities
		}
		if f.AllowedModels == nil {
			cfg.AllowedModels = existing.AllowedModels
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
		} else {
			env, envErr := resolveServiceEnv(existing.Env, f.Env)
			if envErr != nil {
				return envErr
			}
			cfg.Env = env
		}
		if err := cfg.Validate(); err != nil {
			return invalidService(err.Error())
		}
		s.UpdateService(cfg)
		return nil
	}); err != nil {
		if errors.Is(err, errServiceNotFound) || errors.Is(err, errServiceInvalid) || errors.Is(err, errServiceChangedDuringApproval) {
			return config.ServiceConfig{}, err
		}
		return config.ServiceConfig{}, fmt.Errorf("save service: %w", err)
	}
	if err := recordConfigChange(o.Issuance, auditCredentialService, id, nil, via, credID, approval.presenceID); err != nil {
		slog.Error("service updated but not recorded in the audit log", "id", id, "error", err)
	}

	var reloadErr error
	if wasRunning {
		reloadErr = o.Registry.Reload(id, &cfg)
	}
	o.notify()
	if reloadErr != nil {
		return cfg, fmt.Errorf("%w: restart failed: %w", errServiceProcess, reloadErr)
	}
	return cfg, nil
}

func (o *ServiceOps) Remove(id, via, credID string) error {
	return o.runQueued(context.Background(), func() error {
		return o.remove(id, via, credID)
	})
}

func (o *ServiceOps) remove(id, via, credID string) error {
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
	return o.runQueued(context.Background(), func() error {
		return o.setAutostart(id, on)
	})
}

func (o *ServiceOps) setAutostart(id string, on bool) error {
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
	return o.runQueued(context.Background(), func() error {
		return o.start(id)
	})
}

func (o *ServiceOps) start(id string) error {
	if id == config.RelaySessionsServiceID {
		// The stored record for this id is deliberately bare (no Command --
		// see EnsureBuiltinRelaySessionsRecord), so a bare pass-through to
		// Registry.Start would reach cfg.Validate() and fail with "service
		// command is required," a correct but opaque answer for a caller who
		// has no way to know this record is special. StartAllAutostart is
		// the only path that starts it, using its own fully-populated
		// synthesis (EnsureBuiltinRelaySessionsService), never this one.
		return invalidService(fmt.Sprintf("%q is relay's built-in session host and cannot be started manually", id))
	}
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
	return o.runQueued(context.Background(), func() error {
		return o.stop(id)
	})
}

func (o *ServiceOps) stop(id string) error {
	_, idx := config.FindServiceByID(o.Store.Get(), id)
	if idx < 0 && !o.Registry.IsRunning(id) {
		return fmt.Errorf("%w: %s", errServiceNotFound, id)
	}
	o.Registry.Stop(id)
	o.notify()
	return nil
}

func (o *ServiceOps) Restart(id string) error {
	return o.runQueued(context.Background(), func() error {
		svc, _ := config.FindServiceByID(o.Store.Get(), id)
		if svc == nil {
			return fmt.Errorf("%w: %s", errServiceNotFound, id)
		}
		if err := o.Registry.Reload(id, svc); err != nil {
			return fmt.Errorf("restart service: %w", err)
		}
		o.notify()
		return nil
	})
}

// ConfigSaveResult reports what a committed SaveConfigFile did to the process.
type ConfigSaveResult struct {
	Restarted bool
}

// SaveConfigFile writes a service-owned config file and restarts the service
// as one queued step, so the record, the path, the file and the process all
// come from the same instant: a save cannot outlive a queued remove or update
// of its service, and cannot revive one a queued stop just stopped.
//
// An error wrapping errServiceProcess means the file WAS written and only the
// restart failed; every other error means nothing was written.
func (o *ServiceOps) SaveConfigFile(ctx context.Context, id, text string) (res ConfigSaveResult, err error) {
	err = o.runQueued(ctx, func() error {
		var innerErr error
		res, innerErr = o.saveConfigFile(id, text)
		return innerErr
	})
	return res, err
}

func (o *ServiceOps) saveConfigFile(id, text string) (ConfigSaveResult, error) {
	if o.Enhanced == nil {
		return ConfigSaveResult{}, invalidService("no enhanced registry")
	}
	svc, _ := config.FindServiceByID(o.Store.Get(), id)
	if svc == nil {
		return ConfigSaveResult{}, &serviceConfigError{reason: fmt.Sprintf("service %q not registered", id), kind: errServiceNotFound}
	}
	rec := o.Enhanced.Get(id)
	if rec == nil {
		return ConfigSaveResult{}, &serviceConfigError{reason: fmt.Sprintf("service %q not registered", id), kind: errServiceNotFound}
	}
	decl := rec.Manifest.Config
	if decl == nil {
		return ConfigSaveResult{}, invalidService(fmt.Sprintf("service %q declares no config file", id))
	}
	// Validate BEFORE resolving/writing so a malformed edit never touches the
	// file (load-bearing safety property).
	if err := service.ValidateConfigText([]byte(text), decl.Format); err != nil {
		return ConfigSaveResult{}, invalidService("config does not parse: " + err.Error())
	}
	realPath, info, err := service.ResolveConfigPath(decl, svc.WorkingDir)
	if err != nil {
		return ConfigSaveResult{}, err
	}
	// Write the ORIGINAL edited bytes (not a re-marshal) so comments and key
	// order survive on disk; the mode comes from the FileInfo just validated,
	// avoiding a re-stat race.
	if err := service.WriteConfigFile(realPath, []byte(text), info.Mode().Perm()); err != nil {
		return ConfigSaveResult{}, err
	}
	// A stopped service stays stopped: only a live process needs the new file.
	if decl.ApplyMode == bridge.ConfigApplyLive || !o.Registry.IsRunning(id) {
		return ConfigSaveResult{}, nil
	}
	cfg := *svc
	if err := o.Registry.Reload(id, &cfg); err != nil {
		slog.Error("service restart after config save failed", "id", id, "error", err)
		return ConfigSaveResult{}, &serviceConfigError{reason: fmt.Sprintf("config saved but restart failed: %v", err), kind: errServiceProcess}
	}
	o.notify()
	return ConfigSaveResult{Restarted: true}, nil
}
