package main

import (
	"errors"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
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

	// The config_change vocabulary (§7.5): a gated act that mutates
	// settings without issuing anything a holder could authenticate with.
	auditCredentialExternalMcp  = "external_mcp"
	auditCredentialService      = "service"
	auditCredentialProjectGrant = "project_grant"
)

// How an act was initiated. A separate axis from the actor kind: `cli` and
// `ipc` are both the operator, and knowing which one told relay to do this is
// what tells a terminal session apart from a click in the Settings window.
const (
	auditViaCLI  = "cli"
	auditViaIPC  = "ipc"
	auditViaTray = "tray"
	auditViaHTTP = "http"
	// auditViaRemote is the enrolment-certificate door: the remote listener's
	// configuration plane (ADR-018 decision 4). Unlike every other `via`, the
	// caller here is attested by TLS rather than by ownership of the config
	// dir or a resolved control-plane credential, so internal/audit's
	// issuanceActor gives it its own branch rather than folding it into the
	// operator default.
	auditViaRemote = "remote"
)

// IssuanceAuditor is the issuance counterpart to control.ControlAuditor, and a
// separate interface rather than a method added to that one: the doors that
// issue are not the doors that route, and a control-plane door with no
// issuance to record should not have to implement this.
//
// It returns an error where RecordDecision does not, because its callers are
// fail-closed: "I could not record this" and "I recorded this" must not be
// indistinguishable at a door that is about to hand someone a credential.
type IssuanceAuditor interface {
	RecordIssuance(iss audit.CredentialIssuance) error
}

var _ IssuanceAuditor = (*audit.AuditRecorder)(nil)

// issuanceAuditorOrNil is audit.ControlAuditorOrNil's twin and exists for the same
// subtlety: a nil *audit.AuditRecorder boxed into this interface produces a
// non-nil IssuanceAuditor, and every nil check against this type is written
// on the interface.
func issuanceAuditorOrNil(rec *audit.AuditRecorder) IssuanceAuditor {
	if rec == nil {
		return nil
	}
	return rec
}

// errIssuanceAuditingRequired is what every gated core returns when there is
// no sink an act could be recorded in.
var errIssuanceAuditingRequired = errors.New(`refusing to issue — the tool-call audit log is disabled ("audit": {"enabled": false} in settings.json), and with sealed config active issuance auditing is a hard dependency, not a courtesy (ADR-017 Consequences; the same rule ADR-010 applies to the remote listener). Set "audit": {"enabled": true} and restart relay. ` + "`relay audit --path`" + ` names the file relay would write to.`)

// issuanceAuditorReadiness is implemented by *audit.AuditRecorder. A test
// fake that implements only IssuanceAuditor and not this is treated as ready
// by requireIssuanceAuditor: a fake wired to unconditionally accept a record
// IS a sink, by construction, so there is nothing for this check to add.
type issuanceAuditorReadiness interface{ Ready() bool }

// requireIssuanceAuditor is decision 3.4's hard dependency (§7.4): a gated
// core calls this BEFORE it asks for presence, so an operator is never made
// to type a password for an act that was going to refuse anyway.
//
// a == nil, or a recorder reporting it is not Ready (disabled, or enabled
// but never actually got a sink open), both refuse. This is deliberate:
// refusing at the operation rather than at startup, unlike ADR-010's
// remote-listener rule, because issuance is spread across six cores rather
// than being one optional subsystem — refusing every one of them to start
// would destroy the read half and the tray's own recovery UI over a single
// misconfigured field.
//
// This is deliberate: requireIssuanceAuditor stays an unqualified identifier
// in package main. cmd/relay/gate_ast_scan_test.go's
// TestGate_EveryIssuanceAuditorCallSiteHasACase matches call sites with
// call.Fun.(*ast.Ident), which a qualified audit.RequireIssuanceAuditor call
// would not satisfy — the scan would silently stop verifying that every
// gated core audits its issuance.
func requireIssuanceAuditor(a IssuanceAuditor) error {
	if a == nil {
		return errIssuanceAuditingRequired
	}
	if r, ok := a.(issuanceAuditorReadiness); ok && !r.Ready() {
		return errIssuanceAuditingRequired
	}
	return nil
}

// recordConfigChange records a gated act that mutates settings but issues
// nothing (§7.5): registering or unregistering an MCP or service, starting
// an MCP's OAuth flow, or widening a project's grant shape. It goes through
// the same durable, fail-closed RecordIssuance path recordIssuance does —
// there is no second sink for a config_change to go missing in.
func recordConfigChange(a IssuanceAuditor, credential, subject string, grants []string, via, credID, presenceID string) error {
	return recordIssuance(a, audit.CredentialIssuance{
		ConfigChange: true,
		Credential:   credential,
		Subject:      subject,
		Grants:       grants,
		Via:          via,
		CredID:       credID,
		PresenceID:   presenceID,
	})
}

// recordConfigChangeRemote is recordConfigChange's counterpart for an act
// reached over the remote listener: the acting identity is the enrolment's
// certificate, not a CLI process or an HTTP credential, so the record
// carries ClientID/Fingerprint instead of a CredID, and there is no presence
// grant to name — the caller (NarrowForEnrolment) is deliberately ungated.
func recordConfigChangeRemote(a IssuanceAuditor, credential, subject string, grants []string, caller bridge.RemoteCaller) error {
	return recordIssuance(a, audit.CredentialIssuance{
		ConfigChange: true,
		Credential:   credential,
		Subject:      subject,
		Grants:       grants,
		Via:          auditViaRemote,
		ClientID:     caller.ClientID,
		Fingerprint:  caller.Fingerprint,
	})
}

// recordIssuance is the front door every issuing site calls.
//
// This is deliberate: a nil auditor returns nil rather than an error. Auditing
// off is a state an operator reaches by writing `"enabled": false` into
// settings.json, and refusing to mint in it would make relay unusable in a
// configuration relay explicitly supports (docs/audit-log.md, "Turning it off,
// and what it costs"). A sink that exists and FAILS is the opposite case and
// is what the error return is for.
func recordIssuance(a IssuanceAuditor, iss audit.CredentialIssuance) error {
	if a == nil {
		return nil
	}
	return a.RecordIssuance(iss)
}

// recordEnrolmentIssued records a created enrolment and, when that record
// cannot be written, revokes what was just created.
//
// This is deliberate, and is the one issuing path that needs an undo: the
// artifact is a credential the client already holds — a key relay wrote, or
// a certificate over a key the client generated — so withholding the bundle
// path would not withhold the credential. enrolment.Revoke removes the
// record AND the emitted bundle, which is what makes the refusal real.
func recordEnrolmentIssued(a IssuanceAuditor, store config.SettingsStore, e config.Enrolment, via, credID, presenceID string) error {
	err := recordIssuance(a, audit.CredentialIssuance{
		Credential: auditCredentialEnrolment,
		Subject:    e.ClientID,
		Grants:     e.ProjectIDs,
		Via:        via,
		CredID:     credID,
		PresenceID: presenceID,
	})
	if err == nil {
		return nil
	}
	if _, undoErr := enrolment.Revoke(store, e.ClientID); undoErr != nil {
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
func recordBootstrapIssued(a IssuanceAuditor, expires, via, presenceID string) error {
	return recordIssuance(a, audit.CredentialIssuance{
		Credential: auditCredentialBootstrap,
		Subject:    expires,
		Via:        via,
		PresenceID: presenceID,
	})
}

// recordProjectTokenRotated records a rotation as an issuance. A rotation is
// both — the old token dies and a new one is born — and it is recorded as the
// issuance because that is the direction that widens: the new token is the one
// somebody will hold.
func recordProjectTokenRotated(a IssuanceAuditor, projectID, via, credID, presenceID string) error {
	return recordIssuance(a, audit.CredentialIssuance{
		Credential: auditCredentialProject,
		Subject:    projectID,
		Via:        via,
		CredID:     credID,
		PresenceID: presenceID,
	})
}

// recordPasskeyRevoked records a removed passkey. The stored public key has no
// path into the record: passkeyView withholds X and Y from every operator
// surface for the same reason, and audit.CredentialIssuance has no field for
// them.
func recordPasskeyRevoked(a IssuanceAuditor, p config.Passkey, via, presenceID string) error {
	return recordIssuance(a, audit.CredentialIssuance{
		Revoked:    true,
		Credential: auditCredentialPasskey,
		Subject:    p.ID,
		Name:       p.Name,
		Via:        via,
		PresenceID: presenceID,
	})
}

// openCLIIssuanceRecorder is a thin adapter over audit.OpenCLIIssuanceRecorder:
// it supplies the one thing that function needs and does not own, where
// relay's rotated logs live (serviceLogDir, log_rotate.go).
func openCLIIssuanceRecorder(store config.SettingsStore) (*audit.AuditRecorder, error) {
	dir, err := serviceLogDir()
	if err != nil {
		return nil, fmt.Errorf("resolve audit log dir: %w", err)
	}
	return audit.OpenCLIIssuanceRecorder(store, dir)
}

// cliIssuanceAuditor is what every issuing subcommand opens first. It exits
// rather than returning an error: a CLI process that cannot open the sink has
// nothing else to do, and proceeding would be the unrecorded issuance this
// whole path exists to prevent.
//
// This is dead code today (no caller outside its own test) and already
// flagged by the unused linter; it is not exported to make it reachable, and
// stays that way on purpose.
func cliIssuanceAuditor(store config.SettingsStore) (*audit.AuditRecorder, func()) {
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
