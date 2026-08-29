package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"relaygo/bridge"
)

// Conservative per-enrolment defaults, tuned once from evidence for a
// single-user host rather than derived from anything — do not "optimise"
// them without new evidence. The byte cap scales with the window
// deliberately: a fixed cap would silently tighten if the window changed.
const (
	defaultEnrolmentWindowSeconds  = 3600
	defaultEnrolmentMaxCalls       = 120
	defaultEnrolmentMaxResultBytes = 64 << 20 // 64 MiB per window
)

const enrolmentBundleDir = "enrolments"

// normalizeEnrolmentBudget: zero never means "unlimited".
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

// AddEnrolment applies unconditionally; validate with ValidateEnrolment
// first. Does not save; use within store.With.
func (s *Settings) AddEnrolment(e Enrolment) {
	e.Budget = normalizeEnrolmentBudget(e.Budget)
	s.Enrolments = append(s.Enrolments, e)
}

// RemoveEnrolment returns the deleted enrolment so the caller can hand its
// fingerprint to CloseRevokedEnrolment. Does not save; use within store.With.
func (s *Settings) RemoveEnrolment(clientID string) (Enrolment, bool) {
	e, idx := s.findEnrolmentByClientID(clientID)
	if idx < 0 {
		return Enrolment{}, false
	}
	removed := *e
	s.Enrolments = slices.Delete(s.Enrolments, idx, idx+1)
	return removed, true
}

func (s *Settings) UpdateEnrolmentGrants(clientID string, projectIDs []string) {
	e, idx := s.findEnrolmentByClientID(clientID)
	if idx < 0 {
		return
	}
	e.ProjectIDs = projectIDs
}

// FindEnrolmentByFingerprint matches exact on the full fingerprint string —
// see FingerprintDER for why a prefix match would be the wrong shape here.
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

func (s *Settings) FindEnrolment(clientID string) *Enrolment {
	e, _ := s.findEnrolmentByClientID(clientID)
	return e
}

func (s *Settings) findEnrolmentByClientID(clientID string) (*Enrolment, int) {
	for i := range s.Enrolments {
		if s.Enrolments[i].ClientID == clientID {
			return &s.Enrolments[i], i
		}
	}
	return nil, -1
}

func (s *Settings) EnrolmentsGrantingProject(projectID string) []string {
	var ids []string
	for i := range s.Enrolments {
		if s.Enrolments[i].GrantsProject(projectID) {
			ids = append(ids, s.Enrolments[i].ClientID)
		}
	}
	return ids
}

// ValidateEnrolment: call before AddEnrolment, inside the same store.With,
// so two concurrent creates cannot both pass.
func (s *Settings) ValidateEnrolment(e *Enrolment) error {
	if e.ClientID == "" {
		return fmt.Errorf("enrolment client id is required")
	}
	// Client id names the bundle directory: a path separator or ".." would
	// escape it.
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
	// ambiguous at resolution time. Refuse rather than pick.
	if other := s.FindEnrolmentByFingerprint(e.Fingerprint); other != nil {
		return fmt.Errorf("certificate %s is already enrolled as %q", e.Fingerprint, other.ClientID)
	}
	return s.ValidateEnrolmentGrants(e)
}

// ValidateEnrolmentGrants refuses an enrolment whose grants do not all name
// remote-kind projects: without this, a grant naming a local project would
// hand a remote client that project's full host-directory tool surface,
// bypassing every scope restriction by pointing at the wrong project rather
// than defeating any of them.
//
// Tests IsRemote(), never Kind == ProjectKindLocal: the zero value is
// local, so an equality check invites a future bug where an unset field
// reads as remote.
func (s *Settings) ValidateEnrolmentGrants(e *Enrolment) error {
	for _, id := range e.ProjectIDs {
		proj, _ := s.findProjectByID(id)
		if proj == nil {
			return fmt.Errorf("enrolment %q cannot grant unknown access profile %q", e.ClientID, id)
		}
		if !proj.IsRemote() {
			return fmt.Errorf("enrolment %q cannot grant %q (%s): it is a local project, not an access profile, and a remote client granted one would inherit its host directory scope — only access profiles may be enrolled", e.ClientID, id, proj.Name)
		}
	}
	return nil
}

// ValidateProjectEnrolments refuses converting a project remote→local while
// any enrolment still grants it, rather than silently dropping grants from
// records the operator never touched.
func (s *Settings) ValidateProjectEnrolments(proj *Project) error {
	if proj.IsRemote() {
		return nil
	}
	holders := s.EnrolmentsGrantingProject(proj.ID)
	if len(holders) == 0 {
		return nil
	}
	return fmt.Errorf("access profile %q cannot become a local project while enrolled clients grant it: %s — revoke those enrolments or drop the grant first", proj.ID, strings.Join(holders, ", "))
}

// EnrolmentRevocationHook closes live connections holding a revoked
// certificate; deleting the settings record alone would let a compromised
// agent in a persistent scanner loop keep working indefinitely.
type EnrolmentRevocationHook func(clientID, fingerprint string)

var (
	revocationMu   sync.Mutex
	revocationHook EnrolmentRevocationHook
	// revocationOwner, compared by interface identity: the listener owning
	// the hook is rebuilt on every rebind, so teardown must ask "is this
	// still MINE?" before clearing — otherwise the ordinary rebind order
	// (bind new, then close old) has the old server silently uninstall the
	// LIVE server's hook.
	revocationOwner any
)

func SetEnrolmentRevocationHookFor(owner any, fn EnrolmentRevocationHook) {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	revocationHook = fn
	revocationOwner = owner
}

// ClearEnrolmentRevocationHookFor is the compare-and-clear a torn-down
// listener must use: during a rebind the replacement has already installed
// its own hook, and clearing unconditionally here would leave revocation
// unable to sever a live connection.
func ClearEnrolmentRevocationHookFor(owner any) bool {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	if revocationOwner != owner {
		return false
	}
	revocationHook = nil
	revocationOwner = nil
	return true
}

func SetEnrolmentRevocationHook(fn EnrolmentRevocationHook) {
	SetEnrolmentRevocationHookFor(nil, fn)
}

func enrolmentRevocationHookOwner() any {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	if revocationHook == nil {
		return nil
	}
	return revocationOwner
}

func CloseRevokedEnrolment(clientID, fingerprint string) {
	revocationMu.Lock()
	hook := revocationHook
	revocationMu.Unlock()
	if hook != nil {
		hook(clientID, fingerprint)
	}
}

type enrolmentRequest struct {
	ClientID   string
	ProjectIDs []string
	Budget     EnrolmentBudget
}

type enrolmentBundle struct {
	Enrolment  Enrolment
	Dir        string
	KeyPath    string
	CertPath   string
	CACertPath string
}

// createEnrolment has no self-service path and no bootstrap token,
// deliberately: an endpoint reachable by presenting a secret would
// reintroduce a replayable credential at the point where the result is a
// new identity, not a single call.
//
// Validation happens inside store.With so two concurrent creates cannot
// both claim a client id. The bundle is written last: a key on disk that no
// enrolment references is a credential nobody knows to revoke.
func createEnrolment(store SettingsStore, req enrolmentRequest) (*enrolmentBundle, error) {
	ca, err := LoadOrCreateCA(store.Sealer())
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
	if err := withDeclinable(store, func(s *Settings) error {
		if validationErr = s.ValidateEnrolment(&enrolment); validationErr != nil {
			return validationErr
		}
		s.AddEnrolment(enrolment)
		return nil
	}); err != nil {
		if validationErr != nil {
			return nil, invalidEnrolment(validationErr.Error())
		}
		return nil, fmt.Errorf("failed to save settings: %w", err)
	}

	bundle, err := writeEnrolmentBundle(enrolment, keyPEM, certPEM, ca.CertPEM())
	if err != nil {
		// The settings mutation above already committed: the enrolment
		// record exists and is a real, usable grant even though its bundle
		// failed to reach disk. Returning a bare error here would tell every
		// caller "nothing happened," which is false — errEnrolmentBundle is
		// what lets a caller hand back the record that landed instead of
		// silently orphaning it.
		return bundle, fmt.Errorf("%w: %v", errEnrolmentBundle, err)
	}
	return bundle, nil
}

// enrolmentBudgetUpdate: each field is a pointer because zero is meaningful
// on EnrolmentBudget itself ("use the default"), so a plain zero value
// could not distinguish "left alone" from "reset to default". nil means the
// former; a pointer to 0 means the latter.
type enrolmentBudgetUpdate struct {
	WindowSeconds  *int
	MaxCalls       *int
	MaxResultBytes *int64
}

// enrolmentUpdateRequest: ProjectIDs is a pointer to a slice for the same
// reason the budget fields are pointers — nil means "leave the grants
// alone", non-nil-but-empty means "replace them with nothing".
type enrolmentUpdateRequest struct {
	ClientID   string
	ProjectIDs *[]string
	Budget     enrolmentBudgetUpdate
}

// updateEnrolment changes budget and/or grants without touching the
// certificate: revoke+recreate reissues it, and rotating a credential and
// retuning a limit are different operations.
//
// Grants are only re-validated when req.ProjectIDs is non-nil: a
// budget-only update must succeed even when the existing grant already
// names a profile since deleted (docs/access-profiles.md's "dangling
// grant") — re-validating grants nobody asked to change would turn an
// unrelated budget edit into a refusal.
func updateEnrolment(store SettingsStore, req enrolmentUpdateRequest) (before, after Enrolment, err error) {
	var validationErr error
	saveErr := withDeclinable(store, func(s *Settings) error {
		e, idx := s.findEnrolmentByClientID(req.ClientID)
		if idx < 0 {
			validationErr = fmt.Errorf("no enrolment found with client id %q", req.ClientID)
			return validationErr
		}
		before = *e

		// Build the candidate on a copy first: validation must see the
		// post-update shape, and a rejected update must leave the stored
		// record byte-for-byte as it was.
		candidate := *e
		if req.Budget.WindowSeconds != nil {
			candidate.Budget.WindowSeconds = *req.Budget.WindowSeconds
		}
		if req.Budget.MaxCalls != nil {
			candidate.Budget.MaxCalls = *req.Budget.MaxCalls
		}
		if req.Budget.MaxResultBytes != nil {
			candidate.Budget.MaxResultBytes = *req.Budget.MaxResultBytes
		}
		candidate.Budget = normalizeEnrolmentBudget(candidate.Budget)

		if req.ProjectIDs != nil {
			candidate.ProjectIDs = *req.ProjectIDs
			if gerr := s.ValidateEnrolmentGrants(&candidate); gerr != nil {
				validationErr = gerr
				return gerr
			}
		}

		e.Budget = candidate.Budget
		if req.ProjectIDs != nil {
			e.ProjectIDs = candidate.ProjectIDs
		}
		after = *e
		return nil
	})
	if saveErr != nil {
		if validationErr != nil {
			return Enrolment{}, Enrolment{}, validationErr
		}
		return Enrolment{}, Enrolment{}, fmt.Errorf("failed to save settings: %w", saveErr)
	}
	return before, after, nil
}

// writeEnrolmentBundle emits the three files a client needs — its key, its
// certificate, and the CA certificate it verifies the server with — into
// <config>/enrolments/<client-id>/, 0700 dir and 0600 files.
//
// The private key is generated on the host and travels to the client once.
// That is the weakest step in this design and it is deliberate: a CSR flow,
// where the client generates its key and only a signing request crosses the
// gap, never exposes the key at all, and is the obvious upgrade if these
// machines ever stop being the same person's. Until then the mitigation is
// that the key sits in a 0600 file under a 0700 directory, and the operator
// is expected to move rather than copy it.
func writeEnrolmentBundle(e Enrolment, keyPEM, certPEM, caPEM []byte) (*enrolmentBundle, error) {
	dir := filepath.Join(bridge.ConfigDir(), enrolmentBundleDir, e.ClientID)
	// b is built and returned even on failure below: the settings record for
	// e already exists by the time this runs (createEnrolment writes it
	// first), so a caller handling a write failure still needs the record
	// and the directory it was trying to reach.
	b := &enrolmentBundle{
		Enrolment:  e,
		Dir:        dir,
		KeyPath:    filepath.Join(dir, "client.key"),
		CertPath:   filepath.Join(dir, "client.crt"),
		CACertPath: filepath.Join(dir, "ca.crt"),
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return b, fmt.Errorf("create bundle dir: %w", err)
	}
	for path, data := range map[string][]byte{
		b.KeyPath:    keyPEM,
		b.CertPath:   certPEM,
		b.CACertPath: caPEM,
	} {
		if err := atomicWriteFile(path, data, 0600); err != nil {
			return b, fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	return b, nil
}

// revokeEnrolment deletes an enrolment, notifies the revocation hook so the
// listener can sever live connections, and removes the emitted bundle.
// Deleting the record cuts the client without disturbing any project, and
// leaves the certificate itself untouched — there is nothing to un-sign.
func revokeEnrolment(store SettingsStore, clientID string) (Enrolment, error) {
	// Resolution and removal happen inside one With() call: no TOCTOU
	// window between reading the record and deleting it.
	var removed Enrolment
	if err := withDeclinable(store, func(s *Settings) error {
		var found bool
		if removed, found = s.RemoveEnrolment(clientID); !found {
			return fmt.Errorf("%w: %q", errEnrolmentNotFound, clientID)
		}
		return nil
	}); err != nil {
		if errors.Is(err, errEnrolmentNotFound) {
			return Enrolment{}, err
		}
		return Enrolment{}, fmt.Errorf("failed to save settings: %w", err)
	}
	// Severing live connections is the half of revocation the record cannot
	// do. In a CLI process no hook is installed and this is a no-op: what a
	// CLI-only revocation buys is narrower than it looks, since the running
	// tray's listener re-resolves the enrolment from the FILE on every
	// request, so the very next call on an already-open socket is refused —
	// but the socket itself stays open until that client tries something.
	// Closing it outright needs the process that owns the connection table,
	// which is why the hook exists.
	CloseRevokedEnrolment(removed.ClientID, removed.Fingerprint)
	removeEnrolmentBundle(removed.ClientID)
	return removed, nil
}

// removeEnrolmentBundle is best-effort: the operator may have already moved
// the bundle to the client and deleted it here, and a revocation must not
// fail because the host's copy is already gone. Revocation's teeth are the
// deleted record and the closed connection, not this.
func removeEnrolmentBundle(clientID string) {
	if !isSafeID(clientID) {
		return // never join an unvalidated id into a path we then remove
	}
	_ = os.RemoveAll(filepath.Join(bridge.ConfigDir(), enrolmentBundleDir, clientID))
}
