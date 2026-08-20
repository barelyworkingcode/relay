package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"relaygo/bridge"
)

// Enrolment lifecycle: creation (host-side operator act), grant validation,
// persistence in settings.json, bundle emission, and revocation. ADR-010
// decisions 2, 3 and 8. The listener that consumes all of this lives
// elsewhere; nothing here opens a socket.

// Conservative per-enrolment defaults. There is no meaningful global default —
// an agent that checks mail hourly and one that answers interactive questions
// have nothing in common — so these are a starting point to be tuned per
// enrolment, not a considered ceiling. The first values will be wrong; the
// audit log's `throttled` outcome is distinguishable precisely so tuning is
// driven by evidence rather than by guessing twice (ADR-010 decision 7).
const (
	defaultEnrolmentWindowSeconds  = 60
	defaultEnrolmentMaxCalls       = 60
	defaultEnrolmentMaxResultBytes = 8 << 20 // 8 MiB per window
)

// enrolmentBundleDir is the config-dir subdirectory holding emitted bundles,
// one directory per client id.
const enrolmentBundleDir = "enrolments"

// normalizeEnrolmentBudget fills unset fields with the conservative defaults.
// Zero never means "unlimited" — see the EnrolmentBudget doc comment.
func normalizeEnrolmentBudget(b EnrolmentBudget) EnrolmentBudget {
	if b.WindowSeconds <= 0 {
		b.WindowSeconds = defaultEnrolmentWindowSeconds
	}
	if b.MaxCalls <= 0 {
		b.MaxCalls = defaultEnrolmentMaxCalls
	}
	if b.MaxResultBytes <= 0 {
		b.MaxResultBytes = defaultEnrolmentMaxResultBytes
	}
	return b
}

// ---------------------------------------------------------------------------
// Enrolment CRUD — mirrors the Project CRUD in settings.go: plain mutators
// that do not save, called within store.With.
// ---------------------------------------------------------------------------

// AddEnrolment appends an enrolment. Validate with ValidateEnrolment first —
// this mutator applies unconditionally, like the Project mutators.
// Does not save; use within store.With.
func (s *Settings) AddEnrolment(e Enrolment) {
	e.Budget = normalizeEnrolmentBudget(e.Budget)
	s.Enrolments = append(s.Enrolments, e)
}

// RemoveEnrolment deletes the enrolment with the given client id and returns
// it, so the caller can hand its fingerprint to CloseRevokedEnrolment (the
// record is the only place that fingerprint is still written down). Returns
// false if no enrolment has that id.
//
// Deleting the enrolment is the whole of revocation on the persistence side:
// nothing about the certificate itself changes, and no project is touched.
// Does not save; use within store.With.
func (s *Settings) RemoveEnrolment(clientID string) (Enrolment, bool) {
	e, idx := s.findEnrolmentByClientID(clientID)
	if idx < 0 {
		return Enrolment{}, false
	}
	removed := *e
	s.Enrolments = slices.Delete(s.Enrolments, idx, idx+1)
	return removed, true
}

// UpdateEnrolmentGrants replaces an enrolment's grant list. Validate the
// resulting list with ValidateEnrolmentGrants first. Does not save; use within
// store.With.
func (s *Settings) UpdateEnrolmentGrants(clientID string, projectIDs []string) {
	e, idx := s.findEnrolmentByClientID(clientID)
	if idx < 0 {
		return
	}
	e.ProjectIDs = projectIDs
}

// FindEnrolmentByFingerprint resolves a presented client certificate to an
// enrolment. This is the listener's entry point: RemoteServer fingerprints the
// peer certificate (FingerprintCert) and calls this before reading a request,
// closing the connection when it returns nil so an unenrolled caller cannot
// probe for valid grants.
//
// Matching is on the full fingerprint string, exact — see FingerprintDER for
// why a prefix match would be the wrong shape here.
func (s *Settings) FindEnrolmentByFingerprint(fingerprint string) *Enrolment {
	if fingerprint == "" {
		return nil
	}
	for i := range s.Enrolments {
		if s.Enrolments[i].Fingerprint == fingerprint {
			return &s.Enrolments[i]
		}
	}
	return nil
}

// FindEnrolment returns the enrolment with the given client id, or nil.
func (s *Settings) FindEnrolment(clientID string) *Enrolment {
	e, _ := s.findEnrolmentByClientID(clientID)
	return e
}

// findEnrolmentByClientID returns the enrolment with the given client id and
// its index, or nil, -1. Mirrors findProjectByID.
func (s *Settings) findEnrolmentByClientID(clientID string) (*Enrolment, int) {
	for i := range s.Enrolments {
		if s.Enrolments[i].ClientID == clientID {
			return &s.Enrolments[i], i
		}
	}
	return nil, -1
}

// EnrolmentsGrantingProject returns the client ids of every enrolment holding
// a grant for projectID. Feeds the conversion refusal below, and gives the
// Settings UI the "which clients can reach this project" answer — a credential
// you cannot see is one you will not revoke (ADR-010 decision 8).
func (s *Settings) EnrolmentsGrantingProject(projectID string) []string {
	var ids []string
	for i := range s.Enrolments {
		if s.Enrolments[i].GrantsProject(projectID) {
			ids = append(ids, s.Enrolments[i].ClientID)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// Validation — ADR-010 decision 3, enforced at the two points this file owns.
// The third point (call time, in the router) belongs to the listener.
// ---------------------------------------------------------------------------

// ValidateEnrolment checks a candidate enrolment's shape and uniqueness
// against the current settings, then its grants. Call before AddEnrolment,
// inside the same store.With, so two concurrent creates cannot both pass.
func (s *Settings) ValidateEnrolment(e *Enrolment) error {
	if e.ClientID == "" {
		return fmt.Errorf("enrolment client id is required")
	}
	// The client id names the bundle directory under <config>/enrolments/ and
	// is the certificate's Common Name, so the same filename-safe restriction
	// service ids carry applies: a value with a path separator or ".." would
	// escape the bundle directory.
	if !isSafeID(e.ClientID) {
		return fmt.Errorf("enrolment client id %q is invalid: use only letters, digits, '.', '_', '-' (no path separators)", e.ClientID)
	}
	if existing, _ := s.findEnrolmentByClientID(e.ClientID); existing != nil {
		return fmt.Errorf("enrolment %q already exists: revoke it first, or choose another client id", e.ClientID)
	}
	if e.Fingerprint == "" {
		return fmt.Errorf("enrolment %q has no certificate fingerprint: the certificate IS the identity, so an enrolment without one could never resolve a connection", e.ClientID)
	}
	// A second enrolment on the same certificate would make identity
	// ambiguous at exactly the point relay resolves it, and the two rows
	// would diverge in grants — the resolved authority would then depend on
	// scan order. Refuse rather than pick.
	if other := s.FindEnrolmentByFingerprint(e.Fingerprint); other != nil {
		return fmt.Errorf("certificate %s is already enrolled as %q", e.Fingerprint, other.ClientID)
	}
	return s.ValidateEnrolmentGrants(e)
}

// ValidateEnrolmentGrants refuses an enrolment whose grants do not all name
// remote-kind projects (ADR-010 decision 3, point 1).
//
// Without this rule, every protection ADR-009 built — no filesystem scope, no
// wildcard grants, no sessions, no PTY — would be bypassed by POINTING AT THE
// WRONG PROJECT rather than by defeating any of them: a grant naming a local
// project would hand a remote client that project's full host-directory tool
// surface, `allowed_dirs` and all.
//
// Tests IsRemote(), never Kind == ProjectKindLocal: the zero value is local,
// so an equality check invites a future bug where an unset field reads as
// remote (see the Project.Kind comment). An unknown project id is refused too
// — a grant relay cannot resolve is not a grant to keep, and silently
// dropping it would let a later project reusing that id inherit the grant.
func (s *Settings) ValidateEnrolmentGrants(e *Enrolment) error {
	for _, id := range e.ProjectIDs {
		proj, _ := s.findProjectByID(id)
		if proj == nil {
			return fmt.Errorf("enrolment %q cannot grant unknown project %q", e.ClientID, id)
		}
		if !proj.IsRemote() {
			return fmt.Errorf("enrolment %q cannot grant project %q (%s): it is a local project, and a remote client granted one would inherit its host directory scope — only remote-kind projects may be enrolled", e.ClientID, id, proj.Name)
		}
	}
	return nil
}

// ValidateProjectEnrolments refuses converting a project remote→local while
// any enrolment still grants it (ADR-010 decision 3, point 2), naming the
// offending enrolments so the operator knows what to revoke or re-grant.
//
// This mirrors the constrained conversion ADR-009 already applies in the other
// direction (ValidateProjectGrants, refusing local→remote while a
// filesystem-scoped MCP is granted): the project model refuses an edit that
// would strand a credential rather than allowing it and cleaning up
// afterwards. Cleaning up afterwards would mean silently dropping grants from
// records the operator never touched — the same silent widening/narrowing of
// scope both ADRs refuse to do.
//
// A remote project is unaffected, and so is a local project no enrolment
// mentions, so the common case costs one loop over a list that is empty on
// every install that has never enrolled a client.
func (s *Settings) ValidateProjectEnrolments(proj *Project) error {
	if proj.IsRemote() {
		return nil
	}
	holders := s.EnrolmentsGrantingProject(proj.ID)
	if len(holders) == 0 {
		return nil
	}
	return fmt.Errorf("project %q cannot become a local project while enrolled clients grant it: %s — revoke those enrolments or drop the grant first", proj.ID, strings.Join(holders, ", "))
}

// ---------------------------------------------------------------------------
// Revocation hook — the seam RemoteServer fills in.
// ---------------------------------------------------------------------------

// EnrolmentRevocationHook is called after an enrolment is deleted, with the
// deleted client id and its full certificate fingerprint.
//
// Revocation must CLOSE LIVE CONNECTIONS, not merely take effect on the next
// one: the wire protocol holds persistent connections in a scanner loop, so a
// compromised agent that never reconnects would keep working indefinitely
// (ADR-010 decision 8). Deleting the record is all this package does; severing
// the socket is the listener's job, and this is where it registers to do it.
type EnrolmentRevocationHook func(clientID, fingerprint string)

var (
	revocationMu   sync.Mutex
	revocationHook EnrolmentRevocationHook
)

// SetEnrolmentRevocationHook installs the callback invoked by
// CloseRevokedEnrolment. RemoteServer calls this once at startup with a
// closure that closes every live connection presenting the given fingerprint.
// Passing nil clears it (which is also the state in the CLI process, where
// there is no listener to notify — see revokeEnrolment).
func SetEnrolmentRevocationHook(fn EnrolmentRevocationHook) {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	revocationHook = fn
}

// CloseRevokedEnrolment notifies the installed hook that an enrolment is gone.
// Safe to call with no hook installed: a revocation performed from the CLI
// while the tray is not running has no live connections to sever, and one
// performed while it IS running still needs the tray's own listener to be told
// — see the note in revokeEnrolment about that gap.
func CloseRevokedEnrolment(clientID, fingerprint string) {
	revocationMu.Lock()
	hook := revocationHook
	revocationMu.Unlock()
	if hook != nil {
		hook(clientID, fingerprint)
	}
}

// ---------------------------------------------------------------------------
// Creation — the host-side operator act.
// ---------------------------------------------------------------------------

// enrolmentRequest is the transport-agnostic body for creating an enrolment,
// in the shape of projectCreateFields: the CLI fills it today, a Settings-UI
// IPC handler could fill it tomorrow without duplicating the orchestration.
type enrolmentRequest struct {
	ClientID   string
	ProjectIDs []string
	Budget     EnrolmentBudget
}

// enrolmentBundle is what an operator copies to the client machine: the paths
// of the emitted files plus the persisted record.
type enrolmentBundle struct {
	Enrolment  Enrolment
	Dir        string
	KeyPath    string
	CertPath   string
	CACertPath string
}

// createEnrolment is the whole enrolment act: ensure the CA exists, sign a
// client certificate, persist the record, and emit the bundle.
//
// There is no self-service enrolment and no bootstrap token, deliberately. An
// enrolment endpoint reachable by presenting a secret would reintroduce
// precisely the replayable credential decision 2 removed, and would do it at
// the one point in the system where the result is a NEW IDENTITY rather than a
// single call. Creating an enrolment is a host-side act or it is nothing.
//
// Ordering matters: validation happens inside store.With, so two concurrent
// creates cannot both claim a client id, and a rejected request has written
// nothing anywhere — the signed key exists only in memory until the record is
// safely persisted. The bundle is written last, for the same reason: a key on
// disk that no enrolment references is a credential nobody knows to revoke.
func createEnrolment(store SettingsStore, req enrolmentRequest) (*enrolmentBundle, error) {
	ca, err := LoadOrCreateCA()
	if err != nil {
		return nil, err
	}
	keyPEM, certPEM, fingerprint, err := ca.IssueClientCert(req.ClientID)
	if err != nil {
		return nil, err
	}

	enrolment := Enrolment{
		ClientID:    req.ClientID,
		Fingerprint: fingerprint,
		ProjectIDs:  req.ProjectIDs,
		Budget:      normalizeEnrolmentBudget(req.Budget),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if enrolment.ProjectIDs == nil {
		enrolment.ProjectIDs = []string{}
	}

	var validationErr error
	if err := store.With(func(s *Settings) {
		if validationErr = s.ValidateEnrolment(&enrolment); validationErr != nil {
			return // no-op write; nothing is added
		}
		s.AddEnrolment(enrolment)
	}); err != nil {
		return nil, fmt.Errorf("failed to save settings: %w", err)
	}
	if validationErr != nil {
		return nil, validationErr
	}

	bundle, err := writeEnrolmentBundle(enrolment, keyPEM, certPEM, ca.CertPEM())
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

// writeEnrolmentBundle emits the three files a client needs — its key, its
// certificate, and the CA certificate it verifies the server with — into
// <config>/enrolments/<client-id>/, 0700 dir and 0600 files.
//
// The private key is generated on the host and travels to the client once.
// That is the weakest step in this design and it is deliberate: a CSR flow,
// where the client generates its key and only a signing request crosses the
// gap, never exposes the key at all, and is the obvious upgrade if these
// machines ever stop being the same person's (ADR-010 decision 8). Until then
// the mitigation is that the key sits in a 0600 file under a 0700 directory
// inside the config dir, and the operator is expected to move rather than copy
// it.
func writeEnrolmentBundle(e Enrolment, keyPEM, certPEM, caPEM []byte) (*enrolmentBundle, error) {
	dir := filepath.Join(bridge.ConfigDir(), enrolmentBundleDir, e.ClientID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create bundle dir: %w", err)
	}
	b := &enrolmentBundle{
		Enrolment:  e,
		Dir:        dir,
		KeyPath:    filepath.Join(dir, "client.key"),
		CertPath:   filepath.Join(dir, "client.crt"),
		CACertPath: filepath.Join(dir, "ca.crt"),
	}
	for path, data := range map[string][]byte{
		b.KeyPath:    keyPEM,
		b.CertPath:   certPEM,
		b.CACertPath: caPEM,
	} {
		if err := atomicWriteFile(path, data, 0600); err != nil {
			return nil, fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	return b, nil
}

// revokeEnrolment deletes an enrolment, notifies the revocation hook so the
// listener can sever live connections, and removes the emitted bundle.
// Returns the deleted record so the caller can report the fingerprint that no
// longer resolves.
//
// Deleting the record cuts the client without disturbing any project, and
// leaves the certificate itself untouched — there is nothing to un-sign.
// Revocation, not expiry, is the control (decision 8), so this is the only
// mechanism there is.
func revokeEnrolment(store SettingsStore, clientID string) (Enrolment, error) {
	// Resolution and removal happen inside one With() call, matching
	// resolveAndRemove's reason for doing the same: no TOCTOU window between
	// reading the record and deleting it.
	var removed Enrolment
	var found bool
	if err := store.With(func(s *Settings) {
		removed, found = s.RemoveEnrolment(clientID)
	}); err != nil {
		return Enrolment{}, fmt.Errorf("failed to save settings: %w", err)
	}
	if !found {
		return Enrolment{}, fmt.Errorf("no enrolment found with client id %q", clientID)
	}
	// Severing live connections is the half of revocation the record cannot
	// do: taking effect on the next connection is not enough, because the
	// wire protocol holds persistent connections in a scanner loop and a
	// compromised agent that never reconnects would keep working. In a CLI
	// process no hook is installed and this is a no-op — a CLI revocation
	// reaches a running tray only through its settings poll
	// (App.ReloadIfChanged), which changes what the NEXT connection resolves
	// to but does not by itself close an established one. Closing that gap
	// belongs to RemoteServer, which owns both the connection table and the
	// reload path; it is said plainly here rather than left to be discovered.
	CloseRevokedEnrolment(removed.ClientID, removed.Fingerprint)
	removeEnrolmentBundle(removed.ClientID)
	return removed, nil
}

// removeEnrolmentBundle deletes an emitted bundle directory. Best-effort: the
// operator may well have moved the bundle to the client and deleted it here,
// and a revocation must not fail because the copy on the host is already gone.
// Revocation's teeth are the deleted record and the closed connection, not
// this.
func removeEnrolmentBundle(clientID string) {
	if !isSafeID(clientID) {
		return // never join an unvalidated id into a path we then remove
	}
	_ = os.RemoveAll(filepath.Join(bridge.ConfigDir(), enrolmentBundleDir, clientID))
}
