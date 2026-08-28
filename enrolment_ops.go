package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

var (
	errEnrolmentNotFound = errors.New("enrolment not found")
	errEnrolmentInvalid  = errors.New("invalid enrolment")
	// errEnrolmentBundle means the enrolment record COMMITTED to settings
	// and only the on-disk bundle (client key, client cert, CA cert) failed
	// to write. Callers must not treat it as a failed creation: the
	// returned enrolment is the record that landed, and reporting it as a
	// plain error would lose a credential the operator can already see in
	// settings.json but has no bundle to hand to the client.
	errEnrolmentBundle = errors.New("enrolment bundle")
)

// Carries the reason text verbatim, the same trick serviceValidationError
// uses: wrapping with %w would prefix the sentinel's own text, and
// enrolment.go's messages already name the offending grant or client id for
// the operator to act on as-is.
type enrolmentValidationError struct{ reason string }

func (e *enrolmentValidationError) Error() string        { return e.reason }
func (e *enrolmentValidationError) Is(target error) bool { return target == errEnrolmentInvalid }

func invalidEnrolment(reason string) error {
	return &enrolmentValidationError{reason: reason}
}

// enrolmentFields is the create request; JSON tags match ipcCreateEnrolmentMsg's.
type enrolmentFields struct {
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

// EnrolmentCreated is deliberately narrower than enrolmentBundle, the type
// createEnrolment writes to disk: that one also carries KeyPath, CertPath
// and CACertPath, and a caller across an API boundary has no legitimate use
// for any of them. The client private key must never cross that boundary,
// and giving this type no field that names the key file is what makes that
// true by construction rather than by discipline at every call site.
type EnrolmentCreated struct {
	Enrolment Enrolment
	Dir       string
}

// remoteConfigFields is the PUT /api/remote and update_remote_config
// request; JSON tags match ipcRemoteConfigMsg's.
type remoteConfigFields struct {
	Remove  bool   `json:"remove"`
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

// The one core behind both the HTTP door (enrolment_routes.go) and the
// WebView IPC door (ipc_enrolments.go); neither holds validation or logic
// beyond decoding a request and spelling the result.
type EnrolmentOps struct {
	Store SettingsStore
	// Audit answers whether the tool-call audit log is on. Remote access is
	// gated on it (ADR-010 decision 5), so the remote-config view reports it
	// alongside the block's own state, the same pair pushFullSettings has
	// always sent. Nil-safe like every AuditRecorder method; nil reads as
	// "auditing is off".
	Audit    *AuditRecorder
	OnChange func()
}

func (o *EnrolmentOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

func (o *EnrolmentOps) List() []Enrolment {
	e := o.Store.Get().Enrolments
	if e == nil {
		return []Enrolment{}
	}
	return e
}

func (o *EnrolmentOps) Get(clientID string) (Enrolment, error) {
	e := o.Store.Get().FindEnrolment(clientID)
	if e == nil {
		return Enrolment{}, fmt.Errorf("%w: %s", errEnrolmentNotFound, clientID)
	}
	return *e, nil
}

// Create delegates grant and client-id legality to createEnrolment, which
// runs them inside the same store.With as the write so two concurrent
// creates cannot both claim a client id — a second check out here could
// only ever disagree with the one that counts. The only validation that
// belongs at this layer is the client-id-required check, which needs no
// lock because there is nothing yet to race over.
func (o *EnrolmentOps) Create(f enrolmentFields) (EnrolmentCreated, error) {
	clientID := strings.TrimSpace(f.ClientID)
	if clientID == "" {
		return EnrolmentCreated{}, invalidEnrolment("client id is required")
	}

	bundle, err := createEnrolment(o.Store, enrolmentRequest{
		ClientID:   clientID,
		ProjectIDs: f.ProjectIDs,
		Budget:     f.Budget,
	})
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		return EnrolmentCreated{}, err
	}
	o.notify()
	created := EnrolmentCreated{Enrolment: bundle.Enrolment, Dir: bundle.Dir}
	if err != nil {
		return created, err // errEnrolmentBundle: the record landed, the bundle write didn't
	}
	return created, nil
}

// Returns the revoked record: once it is gone the fingerprint is the only
// thing tying this client's past calls to an identity, and re-reading it
// beforehand would race a concurrent revoke of the same id.
func (o *EnrolmentOps) Revoke(clientID string) (Enrolment, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return Enrolment{}, invalidEnrolment("client id is required")
	}
	// Goes through revokeEnrolment, never RemoveEnrolment directly:
	// revokeEnrolment fires the hook that severs LIVE connections holding
	// the revoked certificate. A compromised agent sitting in a persistent
	// scanner loop never reconnects on its own, so deleting only the
	// settings record would leave it working indefinitely.
	revoked, err := revokeEnrolment(o.Store, clientID)
	if err != nil {
		return Enrolment{}, err
	}
	o.notify()
	return revoked, nil
}

func (o *EnrolmentOps) auditEnabled() bool {
	return o.Audit.Enabled()
}

func (o *EnrolmentOps) RemoteConfig() (remoteConfigView, error) {
	return remoteConfigViewOf(o.Store.Get(), o.auditEnabled()), nil
}

func (o *EnrolmentOps) SetRemoteConfig(f remoteConfigFields) (remoteConfigView, error) {
	listen := strings.TrimSpace(f.Listen)
	if !f.Remove && listen != "" {
		if err := validateRemoteListen(listen); err != nil {
			return remoteConfigView{}, invalidEnrolment(err.Error())
		}
	}

	if err := o.Store.With(func(s *Settings) {
		if f.Remove {
			s.Remote = nil
			return
		}
		if s.Remote == nil {
			s.Remote = &RemoteConfig{}
		}
		// Empty stays empty rather than being filled with the default: the
		// listener applies resolve()'s loopback default itself, and writing
		// it out here would freeze today's default into every settings.json.
		s.Remote.Listen = listen
		if f.Enabled {
			enabled := true
			s.Remote.Enabled = &enabled
		} else {
			// Off collapses to an absent key rather than an explicit false:
			// a block created by setting only an address must never read as
			// `enabled: true` — opening a network listener is a thing the
			// operator says, not a thing relay infers.
			s.Remote.Enabled = nil
		}
	}); err != nil {
		return remoteConfigView{}, fmt.Errorf("save remote config: %w", err)
	}

	o.notify()
	return remoteConfigViewOf(o.Store.Get(), o.auditEnabled()), nil
}

// validateRemoteListen refuses an address the listener could only fail to
// bind, at the moment the operator is still looking at the field, so a typo
// cannot persist silently and surface only as an absent listener after the
// next restart.
//
// An empty HOST (":9910") is NOT refused: binding every interface is a
// deliberate, documented act (the default is loopback so that reaching relay
// from a VM has to be chosen, not so that choosing it is forbidden). The UI
// warns about it instead.
func validateRemoteListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port (e.g. %s)", addr, defaultRemoteListen)
	}
	if port == "" {
		return fmt.Errorf("listen address %q has no port (e.g. %s)", addr, defaultRemoteListen)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("listen address %q has an invalid port %q", addr, port)
	}
	return nil
}
