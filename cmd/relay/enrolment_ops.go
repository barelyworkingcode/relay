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

	"github.com/barelyworkingcode/relay/internal/presence"
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
	if r.CLIAdmin != nil {
		b.BoolField("cli_admin", true, *r.CLIAdmin)
	} else {
		b.BoolField("cli_admin", false, false)
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
	// CertPEM and CAPEM are populated by Sign and Approve (never by Create,
	// whose caller has always read its bundle off Dir): the certificate,
	// unlike the key, is public, and carrying it here is what lets Approve
	// hand it back even when the bundle's on-disk write failed (§11.7) —
	// the certificate exists in memory whether or not the write did.
	CertPEM string
	CAPEM   string
}

// remoteConfigFields is the PUT /api/remote and update_remote_config
// request; JSON tags match ipcRemoteConfigMsg's.
//
// EnrolmentRequests/EnrolmentListen follow Enabled/Listen's own shape,
// carrying the operator's say-so for the third listener (spec §1). Neither
// door reimplements enrolment_requests:true-with-enabled:false as a
// save-time refusal here: that check lives once, at resolve time
// (RemoteConfig.resolveEnrolment), so a value this handler accepted and the
// value the supervisor later refuses to serve can never disagree about
// which one is authoritative.
type remoteConfigFields struct {
	Remove            bool   `json:"remove"`
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	EnrolmentRequests bool   `json:"enrolment_requests"`
	EnrolmentListen   string `json:"enrolment_listen"`
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
	Gate *presence.Gate
	// Requests is the pending enrolment-request table Approve, Refuse and
	// PendingRequests read and mutate (spec §3). Nil in every door that
	// predates this slice — Create/Sign/Update/Revoke never touch it, so
	// those keep working exactly as before with a zero Requests — and
	// Approve/Refuse/PendingRequests refuse instead of panicking when it is
	// unset (errEnrolmentRequestsNotWired).
	Requests EnrolmentRequestApprovalSink
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
// normative (§1.4, §11.2): the CSR is parsed BEFORE the gate, so a malformed
// CSR never makes an operator type a password for an act that was going to
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

	return o.completeSigning(enrolmentRequest{ClientID: clientID, ProjectIDs: f.ProjectIDs, Budget: f.Budget}, csr, grant, via, credID)
}

// completeSigning is the ordering §11.2 pins as normative — signEnrolment,
// then recordEnrolmentIssued's fail-closed undo — shared verbatim by Sign
// and Approve. Both callers have already parsed their own CSR, checked
// requireIssuanceAuditor and redeemed their own presence grant over
// "enrolment.sign" before reaching here; this is what runs identically once
// they have, so neither can drift from the other on the part that is
// actually fragile — getting the undo or the commit order wrong.
func (o *EnrolmentOps) completeSigning(req enrolmentRequest, csr *x509.CertificateRequest, grant presence.Grant, via, credID string) (EnrolmentCreated, error) {
	bundle, err := signEnrolment(o.Store, req, csr)
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		return EnrolmentCreated{}, err
	}
	bundleErr := err
	created := EnrolmentCreated{
		Enrolment: bundle.Enrolment,
		Dir:       bundle.Dir,
		CertPEM:   string(bundle.CertPEM),
		CAPEM:     string(bundle.CAPEM),
	}

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

// approveFields is Approve's request (spec §3). RequestID names the pending
// record whose STORED bytes are the ones fed to the gate — there is no
// field here that could carry a CSR, deliberately: a field like that would
// make the presence digest's SPKI binding decorative (§11.3), since the
// digest would no longer provably bind to what was lodged. An acceptance
// test asserts this absence by reflection. Budget and ProjectIDs are the
// human's choice at approval, same as Sign; CLIAdmin has no field here at
// all — ADR-018 decision 6 keeps that a separate, discrete act from any
// door (§3).
type approveFields struct {
	RequestID  string          `json:"request_id"`
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

// enrolmentApproveReason names the approval act distinctly from a plain
// sign (spec §3): an operator looking at the presence prompt sees that the
// CSR came from a network request, not a file handed to relay directly —
// "approve an enrolment request from 10.0.0.5 and sign a certificate for
// client ... with access to ...". The reason is not part of the digest
// (presenceDigest never reads it), so this naming freedom costs nothing.
func enrolmentApproveReason(remoteAddr, clientID string, projectIDs []string) string {
	return fmt.Sprintf("approve an enrolment request from %s and %s", remoteAddr, enrolmentSignReason(clientID, projectIDs))
}

var (
	// errEnrolmentRequestsNotWired mirrors errPresenceGateNotWired's
	// discipline: a caller with no Requests table refuses rather than
	// panics, and every door that predates this slice leaves it nil.
	errEnrolmentRequestsNotWired = errors.New("enrolment request table is not wired for this operation")
	errEnrolmentRequestNotFound  = errors.New("enrolment request not found")

	// errEnrolmentRequestExpired means completeSigning already committed
	// the enrolment (the record is real, on disk, in `relay enrol list`)
	// but MarkApproved found the row gone by the time it ran: the
	// presence prompt held long enough for enrolmentRequestTTL to pass
	// and some other table activity (another Lodge, a List/PendingRequests
	// read) swept it in between. Threaded through exactly like
	// errEnrolmentBundle -- a caller checks errors.Is and surfaces it
	// rather than treating a non-nil error as "nothing happened" -- because
	// the poll row missing that certificate is a fact the operator must be
	// told, not a fact that unwinds the issuance that already happened.
	errEnrolmentRequestExpired = errors.New("the pending request row expired before this approval could mark it collected")

	// errEnrolmentRequestRefused means completeSigning already committed the
	// enrolment (the record is real, on disk, in `relay enrol list`) but
	// MarkApproved found the row refused by the time it ran: the operator
	// declined this exact request, by name, from the pending list while
	// THIS approval's presence prompt was still open. Threaded through
	// exactly like errEnrolmentRequestExpired, and never collapsed into it
	// -- the row did not expire (it still exists) and the client's poll
	// answers "refused", not "unknown", so telling the operator "expired"
	// here would name a fact that did not happen and hide the one that did
	// (issue #93).
	errEnrolmentRequestRefused = errors.New("the operator refused this request while its approval was already in flight")

	// errEnrolmentRequestSASIncomplete is the host's own enforcement of the
	// comparison, independent of anything the client claims it did: a row
	// that lodged a commitment and never opened it, or opened it wrongly,
	// is unapprovable from EVERY door — CLI, IPC and HTTP alike. A row
	// lodged WITHOUT a commitment (`relayremote request`) is untouched by
	// this and stays approvable exactly as before; its control is the CA
	// fingerprint the operator carried.
	errEnrolmentRequestSASIncomplete = errors.New("this request has not completed its comparison handshake, so it cannot be approved")
)

// Approve is enrolment.sign's second door (spec §3) — NOT a new gated
// operation: presence.GatedOps gains no "enrolment.approve" entry (that
// would be exactly the second door into issuance ADR-018 forbids), and this
// asks the SAME gate under the SAME op name and the SAME digest shape Sign
// does, differing only in where the CSR bytes and the reason come from.
//
// Order matches Sign's, applied to a stored record instead of a request
// field (§11.2, §3 steps 1-4): read the pending record under the table
// lock (1), ParseClientCSR the STORED bytes before anything else runs (2),
// requireIssuanceAuditor, then the gate over norm.presenceDigest(csr) (3),
// then completeSigning — the identical signEnrolment/recordEnrolmentIssued
// body Sign uses (4) — and only once THAT has committed does MarkApproved
// run, so a failed audit-log undo (errEnrolmentUnrecorded, never
// errEnrolmentBundle) can never leave a poll answering "approved" for an
// enrolment that was just revoked (AC-24).
func (o *EnrolmentOps) Approve(ctx context.Context, f approveFields, via, credID string) (EnrolmentCreated, error) {
	if o.Requests == nil {
		return EnrolmentCreated{}, errEnrolmentRequestsNotWired
	}
	requestID := strings.TrimSpace(f.RequestID)
	if requestID == "" {
		return EnrolmentCreated{}, invalidEnrolment("request id is required")
	}
	clientID := strings.TrimSpace(f.ClientID)
	if clientID == "" {
		return EnrolmentCreated{}, invalidEnrolment("client id is required")
	}

	rec, found := o.Requests.Get(requestID)
	if !found {
		return EnrolmentCreated{}, fmt.Errorf("%w: %s", errEnrolmentRequestNotFound, requestID)
	}

	// Before the gate, deliberately: an operator should not be asked for
	// Touch ID for an act that is going to be refused either way.
	if rec.SASCommit != "" && (rec.SASOpen == "" || rec.SASFailed) {
		return EnrolmentCreated{}, fmt.Errorf("%w: %s", errEnrolmentRequestSASIncomplete, sasIncompleteDetail(rec))
	}

	// The stored bytes, never anything approveFields could carry — see its
	// own doc comment.
	csr, err := ParseClientCSR(rec.CSRPEM)
	if err != nil {
		return EnrolmentCreated{}, invalidEnrolment(err.Error())
	}

	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return EnrolmentCreated{}, err
	}
	norm := enrolmentSignFields{ClientID: clientID, ProjectIDs: f.ProjectIDs, Budget: f.Budget, CSRPEM: string(rec.CSRPEM)}
	reason := enrolmentApproveReason(rec.RemoteAddr, clientID, f.ProjectIDs)
	grant, err := requireGate(o.Gate, ctx, "enrolment.sign", norm.presenceDigest(csr), reason)
	if err != nil {
		return EnrolmentCreated{}, err
	}

	created, err := o.completeSigning(enrolmentRequest{ClientID: clientID, ProjectIDs: f.ProjectIDs, Budget: f.Budget}, csr, grant, via, credID)
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		return EnrolmentCreated{}, err
	}
	// Reachable only once completeSigning's own fail-closed undo has
	// already succeeded (err here is nil or errEnrolmentBundle — never
	// errEnrolmentUnrecorded, which returned above): the poll response must
	// never carry an approval the audit log could not record (AC-24). The
	// bundle write may still have failed (§11.7) — the certificate is
	// delivered from memory regardless.
	//
	// MarkApproved's outcome answers a question completeSigning cannot: is
	// the row STILL THERE, and if not, why. The gate above can hold the
	// operator's presence prompt open for as long as it takes a human to
	// answer it, and in that window either the row's own independent
	// 15-minute TTL can pass, or the operator can refuse this exact request
	// from the pending list — two different races with two different facts
	// to report. Either way the enrolment above has ALREADY committed (this
	// line runs after it, never before), so the right answer is never to
	// undo it; it is to say which of the two happened, the same way an
	// on-disk bundle failure already does with errEnrolmentBundle.
	switch o.Requests.MarkApproved(requestID, clientID, o.approvedProjects(f.ProjectIDs), o.relayAddr(), created.CertPEM, created.CAPEM) {
	case markApprovedRowGone:
		err = errors.Join(err, errEnrolmentRequestExpired)
	case markApprovedRowRefused:
		err = errors.Join(err, errEnrolmentRequestRefused)
	}
	return created, err
}

// approvedProjects pairs each granted id with the display name the operator
// saw on the approval sheet, so the requesting machine's closing report can
// name what its grant reaches instead of printing a bare UUID it has no way
// to resolve (spec §5.7): nothing else the client can reach carries a project
// name, and DescribeGrant is gated on cli_admin, which a plain registration
// does not hold.
//
// A project that is unnamed, or gone from settings between the sign and this
// read, degrades to its own id. Never to an empty string: the client prints
// "name  (id)", and a blank there reads as a broken relay rather than as a
// project nobody named.
//
// Only the approved payload carries these — see approvedProject's own comment
// for why that keeps ADR-018 §8 P2 intact.
func (o *EnrolmentOps) approvedProjects(ids []string) []approvedProject {
	if len(ids) == 0 {
		return nil
	}
	s := o.Store.Get()
	out := make([]approvedProject, 0, len(ids))
	for _, id := range ids {
		name := id
		if proj, _ := s.findProjectByID(id); proj != nil && strings.TrimSpace(proj.Name) != "" {
			name = proj.Name
		}
		out = append(out, approvedProject{ID: id, Name: name})
	}
	return out
}

// sasIncompleteDetail names which of the two happened, because the operator
// acts differently on each: a machine that never finished the handshake may
// simply need re-running, and one whose commitment did not open is a fact
// about the network path.
func sasIncompleteDetail(rec pendingRecordView) string {
	if rec.SASFailed {
		return "the requesting machine failed its comparison handshake — refuse this request and register again, and if it fails a second time something is on the network path between that machine and this one"
	}
	return "the requesting machine has not yet completed its comparison handshake; wait for its next poll, or refuse the request"
}

// suggestClientID is advisory only. ValidateEnrolment's uniqueness check
// inside store.With stays the authority — this runs outside any lock, so
// two operators approving at once can still both be offered the same name,
// and the loser is refused loudly there rather than quietly overwriting.
// Returns "" when the label is unusable or every suffix to -99 is taken,
// which the caller renders as "no suggestion", never as a chosen id.
func suggestClientID(s *Settings, label string) string {
	label = strings.TrimSpace(label)
	if label == "" || !isSafeID(label) || len(label) > maxEnrolmentLabelBytes {
		return ""
	}
	if s == nil {
		return label
	}
	if e := s.FindEnrolment(label); e == nil {
		return label
	}
	for n := 2; n <= 99; n++ {
		candidate := fmt.Sprintf("%s-%d", label, n)
		if e := s.FindEnrolment(candidate); e == nil {
			return candidate
		}
	}
	return ""
}

// relayAddr is what an approved poll response's relay_addr carries: the
// TOOL-plane listener's address (§5), not the enrolment-request listener's
// — a client that just collected a certificate has no other way to learn
// where to point relayremote list/call next.
func (o *EnrolmentOps) relayAddr() string {
	return o.Store.Get().Remote.resolve().Listen
}

// Refuse is the operator's explicit decline (spec §2, §3) — deliberately
// NOT gated by presence.Gate: declining a stranger's request from the list
// is not the act ADR-017 decision 3 protects (issue #68's boundary), and
// the table's own Refuse method already records it as a genuine,
// human-driven control.ControlDecision, unlike lodging.
func (o *EnrolmentOps) Refuse(requestID string) error {
	if o.Requests == nil {
		return errEnrolmentRequestsNotWired
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return invalidEnrolment("request id is required")
	}
	if !o.Requests.Refuse(o.Audit, requestID) {
		return fmt.Errorf("%w: %s", errEnrolmentRequestNotFound, requestID)
	}
	return nil
}

// lodgeGenerationReader is enrolmentRequestTable's monotonic insert counter,
// asked for through an interface rather than added to
// EnrolmentRequestApprovalSink: approving needs nothing from it, and the
// table's own doc comment forbids growing anything notification-shaped in
// the other direction. A sink that does not implement it reads as "nothing
// has ever arrived".
type lodgeGenerationReader interface {
	LodgeGeneration() uint64
}

// LodgeGeneration is the pull the tray's notifier reads on the poll it
// already runs. It is deliberately the only new fact crossing this boundary:
// a counter, read on a timer, never a callback the lodge path could invoke.
func (o *EnrolmentOps) LodgeGeneration() uint64 {
	r, ok := o.Requests.(lodgeGenerationReader)
	if !ok {
		return 0
	}
	return r.LodgeGeneration()
}

// PendingRequests is the read-only surface `relay enrol requests` and,
// later, Settings -> Remote Clients' pending panel use. A nil Requests
// table (every door that predates this slice) reads as "nothing pending"
// rather than refusing: listing is not a mutation and has nothing to fail
// closed about.
func (o *EnrolmentOps) PendingRequests() []enrolmentRequestView {
	if o.Requests == nil {
		return []enrolmentRequestView{}
	}
	return o.Requests.List()
}

// enrolmentUpdateReason names the actual act (§6.5.2), special-casing
// cli-admin the way projectGrantUpdateReason special-cases allow_cwd_auth:
// both carry outsized blast radius for a single boolean, and turning either
// off is still gated — the human is being told what changed, not asked to
// approve only the direction that widens.
func enrolmentUpdateReason(req enrolmentUpdateRequest) string {
	if req.CLIAdmin != nil {
		if *req.CLIAdmin {
			return fmt.Sprintf("grant the enrolment %q configuration authority over its own access profiles (cli-admin)", req.ClientID)
		}
		return fmt.Sprintf("withdraw configuration authority (cli-admin) from the enrolment %q", req.ClientID)
	}
	return fmt.Sprintf("update the enrolment %q's grant", req.ClientID)
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
	grant, err := requireGate(o.Gate, ctx, "enrolment.update", req.presenceDigest(), enrolmentUpdateReason(req))
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
	if req.CLIAdmin != nil {
		cliAdminGrant := "cli_admin=off"
		if after.CLIAdmin {
			cliAdminGrant = "cli_admin=on"
		}
		if auditErr := recordConfigChange(o.auditor(), auditCredentialEnrolment, after.ClientID,
			[]string{cliAdminGrant}, via, credID, grant.ID()); auditErr != nil {
			slog.Error("enrolment cli-admin toggled but not recorded in the audit log", "client_id", after.ClientID, "error", auditErr)
		}
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
	enrolListen := strings.TrimSpace(f.EnrolmentListen)
	if !f.Remove {
		if listen != "" {
			if err := validateRemoteListen(listen); err != nil {
				return remoteConfigView{}, invalidEnrolment(err.Error())
			}
		}
		if enrolListen != "" {
			if err := validateRemoteListen(enrolListen); err != nil {
				return remoteConfigView{}, invalidEnrolment(err.Error())
			}
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
		s.Remote.EnrolmentListen = enrolListen
		if f.EnrolmentRequests {
			enrolEnabled := true
			s.Remote.EnrolmentRequests = &enrolEnabled
		} else {
			// Same discipline as Enabled, and for the same reason: opening
			// the enrolment-request door is a thing the operator says, and
			// enrolment_requests:true-with-enabled:false is refused where
			// it has always been refused — at resolve time
			// (RemoteConfig.resolveEnrolment) — not duplicated here.
			s.Remote.EnrolmentRequests = nil
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
