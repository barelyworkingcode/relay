package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The vocabulary of things relay issues. Each names a kind of credential, not
// the door it came from — `relay credential mint` and a completed login
// ceremony both produce an api_credential, and an operator asking "what was
// issued" wants them in one answer.
const (
	auditCredentialAPI       = "api_credential"
	auditCredentialEnrolment = "enrolment"
	auditCredentialPasskey   = "passkey"
	auditCredentialBootstrap = "bootstrap_code"
	auditCredentialProject   = "project_token"
)

// How an act was initiated. A separate axis from the actor kind: `cli` and
// `ipc` are both the operator, and knowing which one told relay to do this is
// what tells a terminal session apart from a click in the Settings window.
const (
	auditViaCLI  = "cli"
	auditViaIPC  = "ipc"
	auditViaTray = "tray"
	auditViaHTTP = "http"
)

// Subject, Name and Grants can all be caller-chosen — an enrolment's client id
// arrives in the body of POST /api/enrolments — so they are bounded here for
// the reason audit_control.go bounds Path and Method: what matters is that the
// on-disk record's size is decided by relay and not by whoever is driving the
// door.
const (
	auditMaxIssuanceFieldBytes = 256
	auditMaxIssuanceGrants     = 64
)

// CredentialIssuance is one issuance or revocation. There is deliberately no
// field for a plaintext, a hash, key material, or a public key: this type is
// the whole input to the record, so a leak would have to be added here first,
// where it is visible, rather than slipping in at one of the seven doors that
// build one.
type CredentialIssuance struct {
	// Revoked picks the event kind. False is issuance, which is the direction
	// that widens and therefore the one that is fail-closed below.
	Revoked bool

	// Credential is one of the auditCredential* values; Subject is the
	// identifier of the thing issued or revoked.
	Credential string
	Subject    string

	// Name is the human-readable label where the kind has one distinct from
	// its identifier — a credential's --name, a passkey's display name.
	Name string

	// Grants is the class set for an api_credential, or the granted
	// access-profile ids for an enrolment. Nil for a kind that has neither.
	Grants []string

	// Via is one of the auditVia* values.
	Via string

	// CredID names the control-plane credential that asked for this, and is
	// set only for Via == auditViaHTTP: the other doors authorize by
	// ownership of the config dir, where there is no credential to name.
	CredID string
}

// IssuanceAuditor is the issuance counterpart to ControlAuditor, and a
// separate interface rather than a method added to that one: the doors that
// issue are not the doors that route, and a control-plane door with no
// issuance to record should not have to implement this.
//
// It returns an error where RecordDecision does not, because its callers are
// fail-closed: "I could not record this" and "I recorded this" must not be
// indistinguishable at a door that is about to hand someone a credential.
type IssuanceAuditor interface {
	RecordIssuance(iss CredentialIssuance) error
}

var _ IssuanceAuditor = (*AuditRecorder)(nil)

// issuanceAuditorOrNil is controlAuditorOrNil's twin and exists for the same
// subtlety: a nil *AuditRecorder boxed into this interface produces a non-nil
// IssuanceAuditor, and every nil check against this type is written on the
// interface.
func issuanceAuditorOrNil(rec *AuditRecorder) IssuanceAuditor {
	if rec == nil {
		return nil
	}
	return rec
}

// recordIssuance is the front door every issuing site calls.
//
// This is deliberate: a nil auditor returns nil rather than an error. Auditing
// off is a state an operator reaches by writing `"enabled": false` into
// settings.json, and refusing to mint in it would make relay unusable in a
// configuration relay explicitly supports (docs/audit-log.md, "Turning it off,
// and what it costs"). A sink that exists and FAILS is the opposite case and
// is what the error return is for.
func recordIssuance(a IssuanceAuditor, iss CredentialIssuance) error {
	if a == nil {
		return nil
	}
	return a.RecordIssuance(iss)
}

// recordEnrolmentIssued records a created enrolment and, when that record
// cannot be written, revokes what was just created.
//
// This is deliberate, and is the one issuing path that needs an undo: the
// artifact an enrolment produces is a client private key already written to
// disk, so withholding the bundle path from the caller would not withhold the
// credential. revokeEnrolment removes the record AND the emitted bundle,
// which is what makes the refusal real.
func recordEnrolmentIssued(a IssuanceAuditor, store SettingsStore, e Enrolment, via, credID string) error {
	err := recordIssuance(a, CredentialIssuance{
		Credential: auditCredentialEnrolment,
		Subject:    e.ClientID,
		Grants:     e.ProjectIDs,
		Via:        via,
		CredID:     credID,
	})
	if err == nil {
		return nil
	}
	if _, undoErr := revokeEnrolment(store, e.ClientID); undoErr != nil {
		return fmt.Errorf("%w (and revoking the unrecorded enrolment %q also failed: %v)", err, e.ClientID, undoErr)
	}
	return err
}

// recordBootstrapIssued records a minted registration code.
//
// Subject is the anchor's EXPIRY and not an id, because a bootstrap code has
// no id: only its SHA-256 is stored, and the hash is a verifier for a live
// secret that must never reach this file. The anchor is one at a time and each
// mint replaces its predecessor, so the expiry is the only non-secret fact
// that tells two of them apart — which is exactly what a reader needs to match
// a code against the registration that later consumed it.
func recordBootstrapIssued(a IssuanceAuditor, expires, via string) error {
	return recordIssuance(a, CredentialIssuance{
		Credential: auditCredentialBootstrap,
		Subject:    expires,
		Via:        via,
	})
}

// recordProjectTokenRotated records a rotation as an issuance. A rotation is
// both — the old token dies and a new one is born — and it is recorded as the
// issuance because that is the direction that widens: the new token is the one
// somebody will hold.
func recordProjectTokenRotated(a IssuanceAuditor, projectID, via, credID string) error {
	return recordIssuance(a, CredentialIssuance{
		Credential: auditCredentialProject,
		Subject:    projectID,
		Via:        via,
		CredID:     credID,
	})
}

// recordPasskeyRevoked records a removed passkey. The stored public key has no
// path into the record: passkeyView withholds X and Y from every operator
// surface for the same reason, and CredentialIssuance has no field for them.
func recordPasskeyRevoked(a IssuanceAuditor, p Passkey, via string) error {
	return recordIssuance(a, CredentialIssuance{
		Revoked:    true,
		Credential: auditCredentialPasskey,
		Subject:    p.ID,
		Name:       p.Name,
		Via:        via,
	})
}

// RecordIssuance writes one issuance or revocation and does not return until
// the bytes are on disk.
//
// Durable rather than queued, unlike RecordDecision: the fail-open queue
// exists so a slow sink can never delay a tool call, and delaying an issuance
// until its record is down is exactly the point here — the caller refuses the
// act when this returns an error (ADR-010 decision 5's ordering, applied to
// issuance in docs/audit-log.md).
func (r *AuditRecorder) RecordIssuance(iss CredentialIssuance) error {
	// A recorder with no sink behind it is the same state as no recorder at
	// all — there is no file an act could have gone unrecorded in — so it is
	// answered the way auditing being off is answered, not the way a failing
	// write is.
	if !r.Enabled() || !r.hasSink() {
		return nil
	}
	return r.RecordDurable(issuanceEvent(iss))
}

func issuanceEvent(iss CredentialIssuance) AuditEvent {
	event := AuditEventCredentialIssued
	if iss.Revoked {
		event = AuditEventCredentialRevoked
	}
	subject, subjectCut := capControlString(iss.Subject, auditMaxIssuanceFieldBytes)
	name, nameCut := capControlString(iss.Name, auditMaxIssuanceFieldBytes)
	grants, grantsCut := capIssuanceGrants(iss.Grants)
	return AuditEvent{
		ID:                newAuditID(),
		TS:                time.Now().UTC(),
		Event:             event,
		Outcome:           AuditOutcomeOK,
		Credential:        iss.Credential,
		Subject:           subject,
		SubjectName:       name,
		Grants:            grants,
		Via:               iss.Via,
		IssuanceTruncated: subjectCut || nameCut || grantsCut,
		Actor:             issuanceActor(iss),
	}
}

// classStrings widens a class set into the plain strings the record carries,
// keeping AuditEvent's on-disk shape independent of the authorization
// package's types the way Class and Transport already are.
func classStrings[T ~string](classes []T) []string {
	if len(classes) == 0 {
		return nil
	}
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = string(c)
	}
	return out
}

// capIssuanceGrants bounds both the number of entries and each entry, so a
// caller cannot spend the retention window through a grant list any more than
// through a path.
func capIssuanceGrants(grants []string) ([]string, bool) {
	if len(grants) == 0 {
		return nil, false
	}
	cut := false
	if len(grants) > auditMaxIssuanceGrants {
		grants = grants[:auditMaxIssuanceGrants]
		cut = true
	}
	out := make([]string, len(grants))
	for i, g := range grants {
		capped, entryCut := capControlString(g, auditMaxIssuanceFieldBytes)
		out[i] = capped
		cut = cut || entryCut
	}
	return out, cut
}

// issuanceActor attributes the act. An HTTP door has a resolved credential to
// name and is recorded as `control`, the same actor its control_decision row
// carries. Every other door authorizes by ownership of the config dir, so
// there is no credential — the pid, the process and above all the PARENT are
// the attribution, and the parent is the field that answers which agent ran
// `relay credential mint`.
func issuanceActor(iss CredentialIssuance) AuditActor {
	if iss.Via == auditViaHTTP {
		return AuditActor{Kind: AuditActorControl, Auth: AuditAuthToken, CredID: iss.CredID}
	}
	pid := os.Getpid()
	proc, parent := ProcessNames(pid)
	return AuditActor{Kind: AuditActorOperator, Auth: AuditAuthNone, PID: pid, Proc: proc, Parent: parent}
}

// openCLIIssuanceRecorder gives one CLI process a recorder of its own.
// Returns (nil, nil) when settings say auditing is off, and an error when a
// sink that should exist cannot be opened — the two are different answers and
// every caller here treats only the second as a reason to refuse.
//
// This is deliberate: the writer is a plain O_APPEND file and NOT the
// rotatingWriter the tray uses. Rotation renames the log out from under every
// other open descriptor, and the tray holds one for the life of the app; a CLI
// process that rotated would leave the tray writing into a file it had already
// moved, and further CLI rotations would then shift that file out of the
// generation window entirely. Appending cannot do that to anyone. Each record
// is one write(2) on a descriptor opened O_APPEND, which the kernel serialises
// against every other appender, so two processes interleave whole lines and
// never half of one.
//
// What it costs is a soft cap rather than a hard one. The tray's writer counts
// only the bytes it has itself written since it opened the file, so appends it
// did not make are invisible to its accounting and the log can exceed
// max_file_bytes by whatever CLI processes added. That is repaired two ways
// and neither loses a record: the tray rotates on its own accounting
// eventually, taking the whole file — CLI records included — into the next
// generation, and relay's next start re-stats the file and picks up its true
// size. Issuance is an operator act at human rate, so the overshoot is a few
// hundred bytes per invocation.
func openCLIIssuanceRecorder(store SettingsStore) (*AuditRecorder, error) {
	cfg := store.Get().Audit
	resolved := cfg.resolve()
	if !resolved.Enabled {
		return nil, nil
	}
	path, err := auditLogPath()
	if err != nil {
		return nil, fmt.Errorf("resolve audit log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	// The ring backs a live UI tail and this process has no UI; one slot keeps
	// the allocation from scaling with a setting that means nothing here.
	resolved.RingSize = 1
	return newAuditRecorderWith(resolved, path, f), nil
}

// cliIssuanceAuditor is what every issuing subcommand opens first. It exits
// rather than returning an error: a CLI process that cannot open the sink has
// nothing else to do, and proceeding would be the unrecorded issuance this
// whole path exists to prevent.
func cliIssuanceAuditor(store SettingsStore) (*AuditRecorder, func()) {
	rec, err := openCLIIssuanceRecorder(store)
	if err != nil {
		exitError("cannot open the audit log to record this (%v); nothing was issued or revoked. "+
			"`relay audit --path` names the file relay could not write", err)
	}
	return rec, rec.Close
}

// refuseUnrecordedIssuance is the CLI's half of the fail-closed rule. It is
// reached only after the act has committed, because a mint has no identifier
// to record before it runs — but before the PLAINTEXT has been printed, which
// is the point of no return: a credential whose secret was never disclosed
// grants nothing to anybody, so a refusal here is a real refusal and not the
// theatre ADR-010 decision 5 warns about.
//
// The inert record is deliberately left in settings.json rather than swept:
// the machine has just demonstrated it cannot be written to reliably, and a
// second write on that evidence is a worse answer than naming the one command
// that cleans up.
func refuseUnrecordedIssuance(err error, what, remedy string) {
	exitError("%s, but the audit log could not record it (%v). Its secret was NOT printed and cannot be recovered.\n"+
		"  nothing holds this credential; remove the inert record with: %s", what, err, remedy)
}

// warnUnrecordedRevocation is the other half, and it deliberately does NOT
// refuse.
//
// Revocation NARROWS a grant. Refusing to narrow one because the log is broken
// would make a failing disk the reason a compromised credential stays live,
// which is a worse failure than a gap in the record — the opposite balance to
// issuance, where the gap is the whole attack. So the act stands, and the
// non-zero exit plus this line are what keep it from being silent.
func warnUnrecordedRevocation(err error, what string) {
	exitError("%s, and that stands — but the audit log could not record it (%v). "+
		"Note it by hand: `relay audit` is no longer a complete record of this revocation", what, err)
}
