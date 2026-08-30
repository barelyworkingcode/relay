package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"relaygo/presence"
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

// presenceDigest binds an enrolment.create grant to exactly the client id,
// grant list and budget being issued (§6.4).
func (f enrolmentFields) presenceDigest() presence.Digest {
	return presence.NewDigestBuilder("enrolment.create").
		StringField("client_id", true, f.ClientID).
		StringSetField("project_ids", true, f.ProjectIDs).
		DurationField("budget.window_seconds", true, time.Duration(f.Budget.WindowSeconds)).
		DurationField("budget.max_calls", true, time.Duration(f.Budget.MaxCalls)).
		DurationField("budget.max_result_bytes", true, time.Duration(f.Budget.MaxResultBytes)).
		Build()
}

// presenceDigest binds an enrolment.update grant to exactly the fields the
// request touches, absent-aware: a budget-only update must not be spendable
// on a grant-list change and the reverse (§6.4).
func (r enrolmentUpdateRequest) presenceDigest() presence.Digest {
	b := presence.NewDigestBuilder("enrolment.update").StringField("client_id", true, r.ClientID)
	if r.ProjectIDs != nil {
		b.StringSetField("project_ids", true, *r.ProjectIDs)
	} else {
		b.StringSetField("project_ids", false, nil)
	}
	if r.Budget.WindowSeconds != nil {
		b.DurationField("budget.window_seconds", true, time.Duration(*r.Budget.WindowSeconds))
	} else {
		b.DurationField("budget.window_seconds", false, 0)
	}
	if r.Budget.MaxCalls != nil {
		b.DurationField("budget.max_calls", true, time.Duration(*r.Budget.MaxCalls))
	} else {
		b.DurationField("budget.max_calls", false, 0)
	}
	if r.Budget.MaxResultBytes != nil {
		b.DurationField("budget.max_result_bytes", true, time.Duration(*r.Budget.MaxResultBytes))
	} else {
		b.DurationField("budget.max_result_bytes", false, 0)
	}
	return b.Build()
}

// enrolmentCreateReason names the actual act (§6.5.2).
func enrolmentCreateReason(clientID string, projectIDs []string) string {
	if len(projectIDs) == 0 {
		return fmt.Sprintf("create an enrolment for client %q with no project access", clientID)
	}
	return fmt.Sprintf("create an enrolment for client %q with access to %s", clientID, joinWithAnd(projectIDs))
}

// enrolmentSignFields is the sign request — a separate type from
// enrolmentFields, deliberately: enrolmentFields is the HTTP/IPC create
// body and carries a digest bound to enrolment.create. A sign request that
// decoded into it would be one field away from being handed to Create.
type enrolmentSignFields struct {
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
	CSRPEM     string          `json:"csr_pem"`
}

// presenceDigest binds an enrolment.sign grant to exactly the client id,
// grant list, budget and the CSR's own public key — the one addition over
// enrolment.create's digest, and the point of the operation: a presence
// grant answered for one public key must not be redeemable for another.
func (f enrolmentSignFields) presenceDigest(csr *x509.CertificateRequest) presence.Digest {
	return presence.NewDigestBuilder("enrolment.sign").
		StringField("client_id", true, f.ClientID).
		StringSetField("project_ids", true, f.ProjectIDs).
		DurationField("budget.window_seconds", true, time.Duration(f.Budget.WindowSeconds)).
		DurationField("budget.max_calls", true, time.Duration(f.Budget.MaxCalls)).
		DurationField("budget.max_result_bytes", true, time.Duration(f.Budget.MaxResultBytes)).
		StringField("csr_spki_sha256", true, SPKISHA256Hex(csr.RawSubjectPublicKeyInfo)).
		Build()
}

// enrolmentSignReason names the actual act, mirroring enrolmentCreateReason.
func enrolmentSignReason(clientID string, projectIDs []string) string {
	if len(projectIDs) == 0 {
		return fmt.Sprintf("sign a certificate for client %q with no project access", clientID)
	}
	return fmt.Sprintf("sign a certificate for client %q with access to %s", clientID, joinWithAnd(projectIDs))
}

// errEnrolmentUnrecorded means the create committed to settings, the audit
// write then failed, and recordEnrolmentIssued's own fail-closed rule
// already undid it — a distinguishable sentinel so a door can choose the
// right status without inspecting the message.
var errEnrolmentUnrecorded = errors.New("enrolment created but could not be recorded in the audit log and has been revoked")

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
	Audit *AuditRecorder
	// Issuance, when set, overrides Audit as the sink Create/Update/Revoke
	// record into and requireIssuanceAuditor checks — see auditor()'s doc
	// comment. Production leaves this nil.
	Issuance IssuanceAuditor
	// Gate is the presence check Create, Update and Revoke demand before
	// they touch the store (ADR-017 decisions 3 and 4): an enrolment issues
	// or destroys a remote identity. A nil Gate refuses all three — see
	// requireGate.
	Gate     *presence.Gate
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
func (o *EnrolmentOps) Create(ctx context.Context, f enrolmentFields, via, credID string) (EnrolmentCreated, error) {
	clientID := strings.TrimSpace(f.ClientID)
	if clientID == "" {
		return EnrolmentCreated{}, invalidEnrolment("client id is required")
	}

	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return EnrolmentCreated{}, err
	}
	norm := enrolmentFields{ClientID: clientID, ProjectIDs: f.ProjectIDs, Budget: f.Budget}
	grant, err := requireGate(o.Gate, ctx, "enrolment.create", norm.presenceDigest(), enrolmentCreateReason(clientID, f.ProjectIDs))
	if err != nil {
		return EnrolmentCreated{}, err
	}

	bundle, err := createEnrolment(o.Store, enrolmentRequest{
		ClientID:   clientID,
		ProjectIDs: f.ProjectIDs,
		Budget:     f.Budget,
	})
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		return EnrolmentCreated{}, err
	}
	bundleErr := err
	created := EnrolmentCreated{Enrolment: bundle.Enrolment, Dir: bundle.Dir}

	// Recorded before the bundle directory is announced, and the enrolment
	// is revoked if the record cannot be written (recordEnrolmentIssued's
	// own undo): the client key on disk is the credential, so an unrecorded
	// create must not stand.
	if auditErr := recordEnrolmentIssued(o.auditor(), o.Store, bundle.Enrolment, via, credID, grant.ID()); auditErr != nil {
		return EnrolmentCreated{}, fmt.Errorf("%w: %v", errEnrolmentUnrecorded, auditErr)
	}
	o.notify()
	if bundleErr != nil {
		return created, bundleErr // errEnrolmentBundle: the record landed, the bundle write didn't
	}
	return created, nil
}

// Sign issues a certificate over a CSR the client generated itself: the
// private key it proves possession of never reaches this process (§0.1 —
// "signLeaf takes *ecdsa.PublicKey, not crypto.PublicKey"). Order is
// normative (§1.4): the CSR is parsed BEFORE the gate, so a malformed CSR
// never makes an operator type a password for an act that was going to
// refuse anyway.
func (o *EnrolmentOps) Sign(ctx context.Context, f enrolmentSignFields, via, credID string) (EnrolmentCreated, error) {
	clientID := strings.TrimSpace(f.ClientID)
	if clientID == "" {
		return EnrolmentCreated{}, invalidEnrolment("client id is required")
	}

	csr, err := ParseClientCSR([]byte(f.CSRPEM))
	if err != nil {
		return EnrolmentCreated{}, invalidEnrolment(err.Error())
	}

	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return EnrolmentCreated{}, err
	}
	norm := enrolmentSignFields{ClientID: clientID, ProjectIDs: f.ProjectIDs, Budget: f.Budget, CSRPEM: f.CSRPEM}
	grant, err := requireGate(o.Gate, ctx, "enrolment.sign", norm.presenceDigest(csr), enrolmentSignReason(clientID, f.ProjectIDs))
	if err != nil {
		return EnrolmentCreated{}, err
	}

	bundle, err := signEnrolment(o.Store, enrolmentRequest{
		ClientID:   clientID,
		ProjectIDs: f.ProjectIDs,
		Budget:     f.Budget,
	}, csr)
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		return EnrolmentCreated{}, err
	}
	bundleErr := err
	created := EnrolmentCreated{Enrolment: bundle.Enrolment, Dir: bundle.Dir}

	// Same fail-closed undo as Create: recordEnrolmentIssued's rationale now
	// covers both artifacts a caller might hold — a key relay wrote, or a
	// certificate over a key the client generated (audit_issuance.go).
	if auditErr := recordEnrolmentIssued(o.auditor(), o.Store, bundle.Enrolment, via, credID, grant.ID()); auditErr != nil {
		return EnrolmentCreated{}, fmt.Errorf("%w: %v", errEnrolmentUnrecorded, auditErr)
	}
	o.notify()
	if bundleErr != nil {
		return created, bundleErr // errEnrolmentBundle: the record landed, the bundle write didn't
	}
	return created, nil
}

// Update changes budget and/or grants without touching the certificate
// (updateEnrolment's own doc comment on why that's a different operation
// from revoke+recreate). Gated because a grant-list replacement is exactly
// the "widens one" case §6.4's table calls out, even though ADR-017's own
// table names only create and revoke.
func (o *EnrolmentOps) Update(ctx context.Context, req enrolmentUpdateRequest, via, credID string) (before, after Enrolment, err error) {
	req.ClientID = strings.TrimSpace(req.ClientID)
	if req.ClientID == "" {
		return Enrolment{}, Enrolment{}, invalidEnrolment("client id is required")
	}

	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return Enrolment{}, Enrolment{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "enrolment.update", req.presenceDigest(),
		fmt.Sprintf("update the enrolment %q's grant", req.ClientID))
	if err != nil {
		return Enrolment{}, Enrolment{}, err
	}

	before, after, err = updateEnrolment(o.Store, req)
	if err != nil {
		return before, after, err
	}
	// Reported and not undone, matching the passkey-revoke and login-signout
	// balance: unlike create, there is no side artifact (a private key
	// already on disk) that an unrecorded update would leave dangling.
	if auditErr := recordIssuance(o.auditor(), CredentialIssuance{
		Credential: auditCredentialEnrolment,
		Subject:    after.ClientID,
		Grants:     after.ProjectIDs,
		Via:        via,
		CredID:     credID,
		PresenceID: grant.ID(),
	}); auditErr != nil {
		slog.Error("enrolment updated but not recorded in the audit log", "client_id", after.ClientID, "error", auditErr)
	}
	o.notify()
	return before, after, nil
}

// Returns the revoked record: once it is gone the fingerprint is the only
// thing tying this client's past calls to an identity, and re-reading it
// beforehand would race a concurrent revoke of the same id.
func (o *EnrolmentOps) Revoke(ctx context.Context, clientID, via, credID string) (Enrolment, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return Enrolment{}, invalidEnrolment("client id is required")
	}

	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return Enrolment{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "enrolment.revoke",
		singleStringDigest("enrolment.revoke", "client_id", clientID), fmt.Sprintf("revoke the enrolment %q", clientID))
	if err != nil {
		return Enrolment{}, err
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
	// Reported and not undone: a revocation narrows, and refusing to narrow
	// one because the log is broken would make a failing disk the reason a
	// compromised client stays enrolled.
	if auditErr := recordIssuance(o.auditor(), CredentialIssuance{
		Revoked:    true,
		Credential: auditCredentialEnrolment,
		Subject:    revoked.ClientID,
		Grants:     revoked.ProjectIDs,
		Via:        via,
		CredID:     credID,
		PresenceID: grant.ID(),
	}); auditErr != nil {
		slog.Error("enrolment revoked but not recorded in the audit log", "client_id", revoked.ClientID, "error", auditErr)
	}
	o.notify()
	return revoked, nil
}

func (o *EnrolmentOps) auditEnabled() bool {
	return o.Audit.Enabled()
}

// auditor prefers Issuance when set. This is subtle: production always
// leaves Issuance nil and relies on Audit alone (which RemoteConfig also
// reads directly, for Enabled() rather than for recording), but a test
// exercising §7.4's hard dependency needs to inject a sink that fails in a
// specific way without being a real *AuditRecorder, and Issuance is the
// seam for that.
func (o *EnrolmentOps) auditor() IssuanceAuditor {
	if o.Issuance != nil {
		return o.Issuance
	}
	return issuanceAuditorOrNil(o.Audit)
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
