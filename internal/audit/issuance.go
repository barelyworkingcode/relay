package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// via mirrors two of cmd/relay's auditVia* constants (auditViaHTTP,
// auditViaRemote) by value: CredentialIssuance.Via is a plain string, and
// this package cannot import cmd/relay to share the originals without an
// import cycle. These are part of the on-disk log's own vocabulary, not an
// identity that could drift silently like a sentinel error — a typo here
// would show up immediately as issuanceActor never recognising a real
// record's Via.
const (
	viaHTTP   = "http"
	viaRemote = "remote"
)

// Subject, Name and Grants can all be caller-chosen — an enrolment's client id
// arrives in the body of POST /api/enrolments — so they are bounded here for
// the reason control.go bounds Path and Method: what matters is that the
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
	// that widens and therefore the one that is fail-closed below. Ignored
	// when ConfigChange is set.
	Revoked bool

	// ConfigChange marks this record as a config_change event rather than
	// credential_issued/credential_revoked: the gated act mutated settings
	// (registering an MCP or service, starting OAuth, widening a project's
	// grant shape) but issued nothing a holder could authenticate with.
	// Calling that credential_issued would be a lie, and leaving it
	// unrecorded would break the ADR's detection argument (§7.5).
	ConfigChange bool

	// Credential is one of cmd/relay's auditCredential* values; Subject is
	// the identifier of the thing issued, revoked or changed.
	Credential string
	Subject    string

	// Name is the human-readable label where the kind has one distinct from
	// its identifier — a credential's --name, a passkey's display name.
	Name string

	// Grants is the class set for an api_credential, the granted
	// access-profile ids for an enrolment, or the changed field names for a
	// config_change project grant. Nil for a kind that has none of these.
	// For an enrolment's config_change on cli_admin the entry carries the
	// resulting state (e.g. "cli_admin=on") rather than the bare field
	// name: for a boolean the direction IS the content, and a record
	// saying only "cli_admin changed" cannot answer the question it
	// exists for.
	Grants []string

	// Via is one of cmd/relay's auditVia* values.
	Via string

	// CredID names the control-plane credential that asked for this, and is
	// set only for Via == "http": the other doors authorize by ownership of
	// the config dir, where there is no credential to name.
	CredID string

	// ClientID and Fingerprint attribute an act to the enrolment certificate
	// that made it, set only for Via == "remote". A remote act has no
	// control-plane credential and no config-dir ownership to name — the
	// certificate IS the identity, resolved by TLS before the request was
	// ever read.
	ClientID    string
	Fingerprint string

	// PresenceID is the nonce id (presence.Grant.ID()) that authorised this
	// act, when it was gated (ADR-017 implementation spec §7.5). Empty for
	// an ungated issuance — nothing here changes for those. Always empty
	// for Via == "remote": NarrowForEnrolment is deliberately ungated.
	PresenceID string
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
	return r.RecordDurable(IssuanceEvent(iss))
}

func IssuanceEvent(iss CredentialIssuance) AuditEvent {
	event := AuditEventCredentialIssued
	switch {
	case iss.ConfigChange:
		event = AuditEventConfigChange
	case iss.Revoked:
		event = AuditEventCredentialRevoked
	}
	subject, subjectCut := CapControlString(iss.Subject, auditMaxIssuanceFieldBytes)
	name, nameCut := CapControlString(iss.Name, auditMaxIssuanceFieldBytes)
	grants, grantsCut := capIssuanceGrants(iss.Grants)
	return AuditEvent{
		ID:                NewAuditID(),
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
		PresenceID:        iss.PresenceID,
	}
}

// ClassStrings widens a class set into the plain strings the record carries,
// keeping AuditEvent's on-disk shape independent of the authorization
// package's types the way Class and control.Transport already are.
func ClassStrings[T ~string](classes []T) []string {
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
		capped, entryCut := CapControlString(g, auditMaxIssuanceFieldBytes)
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
	if iss.Via == viaHTTP {
		return AuditActor{Kind: AuditActorControl, Auth: AuditAuthToken, CredID: iss.CredID}
	}
	if iss.Via == viaRemote {
		return AuditActor{Kind: AuditActorRemote, Auth: AuditAuthMTLS, ClientID: iss.ClientID, Fingerprint: iss.Fingerprint}
	}
	pid := os.Getpid()
	proc, parent := ProcessNames(pid)
	return AuditActor{Kind: AuditActorOperator, Auth: AuditAuthNone, PID: pid, Proc: proc, Parent: parent}
}

// OpenCLIIssuanceRecorder gives one CLI process a recorder of its own.
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
func OpenCLIIssuanceRecorder(store config.SettingsStore, logDir string) (*AuditRecorder, error) {
	cfg := config.FreshSettings(store).Audit
	resolved := ResolveAuditConfig(cfg)
	if !resolved.Enabled {
		return nil, nil
	}
	path := LogPath(logDir)
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
	return NewAuditRecorderWith(resolved, path, f), nil
}
