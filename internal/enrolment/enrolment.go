package enrolment

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// Conservative per-enrolment defaults, tuned once from evidence for a
// single-user host rather than derived from anything — do not "optimise"
// them without new evidence. The byte cap scales with the window
// deliberately: a fixed cap would silently tighten if the window changed.
const (
	DefaultWindowSeconds  = 3600
	DefaultMaxCalls       = 120
	DefaultMaxResultBytes = 64 << 20 // 64 MiB per window

	// Mount-plane defaults, on the same rolling window as the tool-plane
	// series above but counted separately (EnrolmentBudget's own doc
	// comment). Tuned as a starting point, not derived; see the design doc's
	// "Budget magnitudes" decision.
	DefaultMountMaxOps        = 500_000
	DefaultMountMaxReadBytes  = 512 << 20 // 512 MiB per window
	DefaultMountMaxWriteBytes = 512 << 20 // 512 MiB per window
)

const BundleDir = "enrolments"

// NormalizeBudget: zero never means "unlimited".
func NormalizeBudget(b config.EnrolmentBudget) config.EnrolmentBudget {
	if b.WindowSeconds <= 0 {
		b.WindowSeconds = DefaultWindowSeconds
	}
	if b.MaxCalls <= 0 {
		b.MaxCalls = DefaultMaxCalls
	}
	if b.MaxResultBytes <= 0 {
		b.MaxResultBytes = DefaultMaxResultBytes
	}
	if b.MountMaxOps <= 0 {
		b.MountMaxOps = DefaultMountMaxOps
	}
	if b.MountMaxReadBytes <= 0 {
		b.MountMaxReadBytes = DefaultMountMaxReadBytes
	}
	if b.MountMaxWriteBytes <= 0 {
		b.MountMaxWriteBytes = DefaultMountMaxWriteBytes
	}
	return b
}

// SafeID guards against path traversal: a client id is joined into the
// bundle directory path <config>/enrolments/<client-id>, so a value with a
// path separator or "." / ".." would escape it. The pending-request table's
// optional label is held to the same charset for the same reason.
func SafeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// addEnrolment applies unconditionally; validate with validate
// first. Does not save; use within store.With.
func addEnrolment(s *config.Settings, e config.Enrolment) {
	e.Budget = NormalizeBudget(e.Budget)
	s.Enrolments = append(s.Enrolments, e)
}

// removeEnrolment returns the deleted enrolment so the caller can hand its
// fingerprint to CloseRevoked. Does not save; use within store.With.
func removeEnrolment(s *config.Settings, clientID string) (config.Enrolment, bool) {
	e, idx := findByClientID(s, clientID)
	if idx < 0 {
		return config.Enrolment{}, false
	}
	removed := *e
	s.Enrolments = slices.Delete(s.Enrolments, idx, idx+1)
	return removed, true
}

// FindByFingerprint matches exact on the full fingerprint string —
// see FingerprintDER for why a prefix match would be the wrong shape here.
func FindByFingerprint(s *config.Settings, fingerprint string) *config.Enrolment {
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

func Find(s *config.Settings, clientID string) *config.Enrolment {
	e, _ := findByClientID(s, clientID)
	return e
}

func findByClientID(s *config.Settings, clientID string) (*config.Enrolment, int) {
	for i := range s.Enrolments {
		if s.Enrolments[i].ClientID == clientID {
			return &s.Enrolments[i], i
		}
	}
	return nil, -1
}

func GrantingProject(s *config.Settings, projectID string) []string {
	var ids []string
	for i := range s.Enrolments {
		if s.Enrolments[i].GrantsProject(projectID) {
			ids = append(ids, s.Enrolments[i].ClientID)
		}
	}
	return ids
}

// validate: call before addEnrolment, inside the same store.With,
// so two concurrent creates cannot both pass.
func validate(s *config.Settings, e *config.Enrolment) error {
	if e.ClientID == "" {
		return fmt.Errorf("enrolment client id is required")
	}
	// Client id names the bundle directory: a path separator or ".." would
	// escape it.
	if !SafeID(e.ClientID) {
		return fmt.Errorf("enrolment client id %q is invalid: use only letters, digits, '.', '_', '-' (no path separators)", e.ClientID)
	}
	if existing, _ := findByClientID(s, e.ClientID); existing != nil {
		return fmt.Errorf("enrolment %q already exists: revoke it first, or choose another client id", e.ClientID)
	}
	if e.Fingerprint == "" {
		return fmt.Errorf("enrolment %q has no certificate fingerprint: the certificate IS the identity, so an enrolment without one could never resolve a connection", e.ClientID)
	}
	// A second enrolment on the same certificate would make identity
	// ambiguous at resolution time. Refuse rather than pick.
	if other := FindByFingerprint(s, e.Fingerprint); other != nil {
		return fmt.Errorf("certificate %s is already enrolled as %q", e.Fingerprint, other.ClientID)
	}
	// A duplicate SPKI (the CSR path only) means the SAME private key was
	// enrolled twice under two client ids: FindByFingerprint alone
	// cannot catch this, since the fingerprint hashes the whole certificate
	// (serial included), and two certificates over one key get two
	// different fingerprints — two live identities, two budgets, and
	// revoking one would leave the other working.
	if e.SPKISHA256 != "" {
		for i := range s.Enrolments {
			if s.Enrolments[i].SPKISHA256 == e.SPKISHA256 {
				return fmt.Errorf("this certificate's public key is already enrolled as %q: revoke it first, or sign against that client id instead", s.Enrolments[i].ClientID)
			}
		}
	}
	return ValidateGrants(s, e)
}

// ValidateGrants refuses an enrolment whose grants do not all name
// remote-kind projects: without this, a grant naming a local project would
// hand a remote client that project's full host-directory tool surface,
// bypassing every scope restriction by pointing at the wrong project rather
// than defeating any of them.
//
// Tests IsRemote(), never Kind == ProjectKindLocal: the zero value is
// local, so an equality check invites a future bug where an unset field
// reads as remote.
func ValidateGrants(s *config.Settings, e *config.Enrolment) error {
	for _, id := range e.ProjectIDs {
		proj, _ := config.FindProjectByID(s, id)
		if proj == nil {
			return fmt.Errorf("enrolment %q cannot grant unknown access profile %q", e.ClientID, id)
		}
		if !proj.IsRemote() {
			return fmt.Errorf("enrolment %q cannot grant %q (%s): it is a local project, not an access profile, and a remote client granted one would inherit its host directory scope — only access profiles may be enrolled", e.ClientID, id, proj.Name)
		}
	}
	return nil
}

// ValidateProjectConversion refuses converting a project remote→local while
// any enrolment still grants it, rather than silently dropping grants from
// records the operator never touched.
func ValidateProjectConversion(s *config.Settings, proj *config.Project) error {
	if proj.IsRemote() {
		return nil
	}
	holders := GrantingProject(s, proj.ID)
	if len(holders) == 0 {
		return nil
	}
	return fmt.Errorf("access profile %q cannot become a local project while enrolled clients grant it: %s — revoke those enrolments or drop the grant first", proj.ID, strings.Join(holders, ", "))
}

// RevocationHook closes live connections holding a revoked
// certificate; deleting the settings record alone would let a compromised
// agent in a persistent scanner loop keep working indefinitely.
type RevocationHook func(clientID, fingerprint string)

var (
	revocationMu   sync.Mutex
	revocationHook RevocationHook
	// revocationOwner, compared by interface identity: the listener owning
	// the hook is rebuilt on every rebind, so teardown must ask "is this
	// still MINE?" before clearing — otherwise the ordinary rebind order
	// (bind new, then close old) has the old server silently uninstall the
	// LIVE server's hook.
	revocationOwner any
)

func SetRevocationHookFor(owner any, fn RevocationHook) {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	revocationHook = fn
	revocationOwner = owner
}

// ClearRevocationHookFor is the compare-and-clear a torn-down
// listener must use: during a rebind the replacement has already installed
// its own hook, and clearing unconditionally here would leave revocation
// unable to sever a live connection.
func ClearRevocationHookFor(owner any) bool {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	if revocationOwner != owner {
		return false
	}
	revocationHook = nil
	revocationOwner = nil
	return true
}

func SetRevocationHook(fn RevocationHook) {
	SetRevocationHookFor(nil, fn)
}

func RevocationHookOwner() any {
	revocationMu.Lock()
	defer revocationMu.Unlock()
	if revocationHook == nil {
		return nil
	}
	return revocationOwner
}

func CloseRevoked(clientID, fingerprint string) {
	revocationMu.Lock()
	hook := revocationHook
	revocationMu.Unlock()
	if hook != nil {
		hook(clientID, fingerprint)
	}
}

type Request struct {
	ClientID   string
	ProjectIDs []string
	Budget     config.EnrolmentBudget
}

type Bundle struct {
	Enrolment  config.Enrolment
	Dir        string
	KeyPath    string
	CertPath   string
	CACertPath string
	// CertPEM and CAPEM are the certificate bytes Sign already
	// holds in memory once ca.SignClientCSR returns, kept here so a caller
	// can hand them back even when the write below (writeSignedCertBundle)
	// fails: the certificate exists — the record already committed — and
	// only the on-host copy is missing (spec §11.7's "the poll response
	// must still deliver the certificate"). Create leaves both
	// empty; a host-generated bundle's caller reads the key and cert off
	// Dir instead, the same as it always has.
	CertPEM []byte
	CAPEM   []byte
}

// Commit builds the Enrolment record and persists it inside
// store.With, after validate passes — the shared middle of the
// host-generated (Create) and CSR (Sign) paths. spki is
// "" for the former; the CSR's SPKI hash for the latter.
func Commit(store config.SettingsStore, req Request, fingerprint, spki string) (config.Enrolment, error) {
	enrolment := config.Enrolment{
		ClientID:    req.ClientID,
		Fingerprint: fingerprint,
		ProjectIDs:  req.ProjectIDs,
		Budget:      NormalizeBudget(req.Budget),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		SPKISHA256:  spki,
	}
	if enrolment.ProjectIDs == nil {
		enrolment.ProjectIDs = []string{}
	}

	var validationErr error
	if err := config.WithDeclinable(store, func(s *config.Settings) error {
		if validationErr = validate(s, &enrolment); validationErr != nil {
			return validationErr
		}
		addEnrolment(s, enrolment)
		return nil
	}); err != nil {
		if validationErr != nil {
			return config.Enrolment{}, Invalid(validationErr.Error())
		}
		return config.Enrolment{}, fmt.Errorf("failed to save settings: %w", err)
	}
	return enrolment, nil
}

// Create has no self-service path and no bootstrap token,
// deliberately: an endpoint reachable by presenting a secret would
// reintroduce a replayable credential at the point where the result is a
// new identity, not a single call.
//
// Validation happens inside store.With (via Commit) so two
// concurrent creates cannot both claim a client id. The bundle is written
// last: a key on disk that no enrolment references is a credential nobody
// knows to revoke.
func Create(store config.SettingsStore, req Request) (*Bundle, error) {
	ca, err := LoadOrCreateCA(store.Sealer())
	if err != nil {
		return nil, err
	}
	keyPEM, certPEM, fingerprint, err := ca.IssueClientCert(req.ClientID)
	if err != nil {
		return nil, err
	}

	enrolment, err := Commit(store, req, fingerprint, "")
	if err != nil {
		return nil, err
	}

	bundle, err := writeLegacyBundle(enrolment, keyPEM, certPEM, ca.CertPEM())
	if err != nil {
		// The settings mutation above already committed: the enrolment
		// record exists and is a real, usable grant even though its bundle
		// failed to reach disk. Returning a bare error here would tell every
		// caller "nothing happened," which is false — ErrBundle is
		// what lets a caller hand back the record that landed instead of
		// silently orphaning it.
		return bundle, fmt.Errorf("%w: %v", ErrBundle, err)
	}
	return bundle, nil
}

// Sign is Create's CSR counterpart: the CSR (already
// validated by ParseClientCSR, before the caller ever reached the gate)
// supplies the public key, relay's CA signs it, and no client private key
// ever exists in this process's address space. Same ErrBundle
// semantics as create: a failed bundle write does not unwind a committed
// record.
func Sign(store config.SettingsStore, req Request, csr *x509.CertificateRequest) (*Bundle, error) {
	ca, err := LoadOrCreateCA(store.Sealer())
	if err != nil {
		return nil, err
	}
	certPEM, fingerprint, err := ca.SignClientCSR(csr, req.ClientID)
	if err != nil {
		return nil, err
	}

	spki := SPKISHA256Hex(csr.RawSubjectPublicKeyInfo)
	enrolment, err := Commit(store, req, fingerprint, spki)
	if err != nil {
		return nil, err
	}

	caPEM := ca.CertPEM()
	bundle, err := writeSignedCertBundle(enrolment, certPEM, caPEM)
	// The certificate exists in memory whether or not the write below
	// succeeded — see Bundle's own doc comment on CertPEM/CAPEM.
	bundle.CertPEM = certPEM
	bundle.CAPEM = caPEM
	if err != nil {
		return bundle, fmt.Errorf("%w: %v", ErrBundle, err)
	}
	return bundle, nil
}

// BudgetUpdate: each field is a pointer because zero is meaningful
// on EnrolmentBudget itself ("use the default"), so a plain zero value
// could not distinguish "left alone" from "reset to default". nil means the
// former; a pointer to 0 means the latter.
type BudgetUpdate struct {
	WindowSeconds      *int   `json:"window_seconds,omitempty"`
	MaxCalls           *int   `json:"max_calls,omitempty"`
	MaxResultBytes     *int64 `json:"max_result_bytes,omitempty"`
	MountMaxOps        *int   `json:"mount_max_ops,omitempty"`
	MountMaxReadBytes  *int64 `json:"mount_max_read_bytes,omitempty"`
	MountMaxWriteBytes *int64 `json:"mount_max_write_bytes,omitempty"`
}

// UpdateRequest: ProjectIDs is a pointer to a slice for the same
// reason the budget fields are pointers — nil means "leave the grants
// alone", non-nil-but-empty means "replace them with nothing". JSON tags
// are what let this travel as an admin_op payload (ADR-017 implementation
// spec S6): `relay enrol update` is its only caller and builds one directly
// from flags, so there is no separate wire type to keep in sync.
type UpdateRequest struct {
	ClientID   string       `json:"client_id"`
	ProjectIDs *[]string    `json:"project_ids,omitempty"`
	Budget     BudgetUpdate `json:"budget"`
	// CLIAdmin is a pointer for the same nil-means-no-change reason as
	// ProjectIDs and every budget field.
	CLIAdmin *bool `json:"cli_admin,omitempty"`
}

// Update changes budget and/or grants without touching the
// certificate: revoke+recreate reissues it, and rotating a credential and
// retuning a limit are different operations.
//
// Grants are only re-validated when req.ProjectIDs is non-nil: a
// budget-only update must succeed even when the existing grant already
// names a profile since deleted (docs/access-profiles.md's "dangling
// grant") — re-validating grants nobody asked to change would turn an
// unrelated budget edit into a refusal.
func Update(store config.SettingsStore, req UpdateRequest) (before, after config.Enrolment, err error) {
	var validationErr error
	saveErr := config.WithDeclinable(store, func(s *config.Settings) error {
		e, idx := findByClientID(s, req.ClientID)
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
		if req.Budget.MountMaxOps != nil {
			candidate.Budget.MountMaxOps = *req.Budget.MountMaxOps
		}
		if req.Budget.MountMaxReadBytes != nil {
			candidate.Budget.MountMaxReadBytes = *req.Budget.MountMaxReadBytes
		}
		if req.Budget.MountMaxWriteBytes != nil {
			candidate.Budget.MountMaxWriteBytes = *req.Budget.MountMaxWriteBytes
		}
		candidate.Budget = NormalizeBudget(candidate.Budget)

		if req.ProjectIDs != nil {
			candidate.ProjectIDs = *req.ProjectIDs
			if gerr := ValidateGrants(s, &candidate); gerr != nil {
				validationErr = gerr
				return gerr
			}
		}

		e.Budget = candidate.Budget
		if req.ProjectIDs != nil {
			e.ProjectIDs = candidate.ProjectIDs
		}
		// Applied outside the grants branch above: a cli-admin-only toggle
		// must not be refused because an unrelated grant names a
		// since-deleted profile (the dangling-grant case this function's
		// own doc comment protects).
		if req.CLIAdmin != nil {
			e.CLIAdmin = *req.CLIAdmin
		}
		after = *e
		return nil
	})
	if saveErr != nil {
		if validationErr != nil {
			return config.Enrolment{}, config.Enrolment{}, validationErr
		}
		return config.Enrolment{}, config.Enrolment{}, fmt.Errorf("failed to save settings: %w", saveErr)
	}
	return before, after, nil
}

// writeBundleFiles is the write step writeLegacyBundle and
// writeSignedCertBundle share: create dir 0700, then write each file 0600
// via atomicWriteFile.
func writeBundleFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}
	for path, data := range files {
		if err := config.AtomicWriteFile(path, data, 0600); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

// writeLegacyBundle emits the three files a client needs — its key, its
// certificate, and the CA certificate it verifies the server with — into
// <config>/enrolments/<client-id>/, 0700 dir and 0600 files.
//
// This is the legacy host-generated path: the private key is generated on
// the host and travels to the client once, which is the liability ADR-018
// decision 6 step 1 retires. writeSignedCertBundle — fed by a CSR the
// client generated itself, so the key never leaves that machine — is the
// one to use going forward; this stays only because create's three doors
// (CLI, HTTP, the Remote Clients tab) are not all CSR-ready yet. Until
// every caller moves, the mitigation is that the key sits in a 0600 file
// under a 0700 directory, and the operator is expected to move rather than
// copy it.
func writeLegacyBundle(e config.Enrolment, keyPEM, certPEM, caPEM []byte) (*Bundle, error) {
	dir := filepath.Join(bridge.ConfigDir(), BundleDir, e.ClientID)
	// b is built and returned even on failure below: the settings record for
	// e already exists by the time this runs (Create writes it
	// first), so a caller handling a write failure still needs the record
	// and the directory it was trying to reach.
	b := &Bundle{
		Enrolment:  e,
		Dir:        dir,
		KeyPath:    filepath.Join(dir, "client.key"),
		CertPath:   filepath.Join(dir, "client.crt"),
		CACertPath: filepath.Join(dir, "ca.crt"),
	}
	if err := writeBundleFiles(dir, map[string][]byte{
		b.KeyPath:    keyPEM,
		b.CertPath:   certPEM,
		b.CACertPath: caPEM,
	}); err != nil {
		return b, err
	}
	return b, nil
}

// writeSignedCertBundle is Sign's write step: certificate and CA
// certificate only — client.crt and ca.crt, 0700 dir / 0600 files, sharing
// writeBundleFiles with the legacy path above. The returned bundle's
// KeyPath is left empty rather than given a nillable key parameter: a
// function whose key argument is sometimes nil is one `if` away from
// writing an empty client.key, and a reader at the call site could not
// tell which mode it was in.
//
// This refuses outright if client.key already exists in the target
// directory: a stale key would make a CSR-issued bundle indistinguishable
// from a relay-generated one, and the caller carrying a CSR onto this host
// is asserting the opposite — that the key never left the client machine.
func writeSignedCertBundle(e config.Enrolment, certPEM, caPEM []byte) (*Bundle, error) {
	dir := filepath.Join(bridge.ConfigDir(), BundleDir, e.ClientID)
	b := &Bundle{
		Enrolment:  e,
		Dir:        dir,
		CertPath:   filepath.Join(dir, "client.crt"),
		CACertPath: filepath.Join(dir, "ca.crt"),
	}
	keyPath := filepath.Join(dir, "client.key")
	if _, err := os.Stat(keyPath); err == nil {
		return b, fmt.Errorf("%s already exists: a stale key here would make this CSR-issued bundle indistinguishable from a relay-generated one — remove it first if you mean to re-issue over it", keyPath)
	} else if !os.IsNotExist(err) {
		return b, fmt.Errorf("stat %s: %w", keyPath, err)
	}
	if err := writeBundleFiles(dir, map[string][]byte{
		b.CertPath:   certPEM,
		b.CACertPath: caPEM,
	}); err != nil {
		return b, err
	}
	return b, nil
}

// Revoke deletes an enrolment, notifies the revocation hook so the
// listener can sever live connections, and removes the emitted bundle.
// Deleting the record cuts the client without disturbing any project, and
// leaves the certificate itself untouched — there is nothing to un-sign.
func Revoke(store config.SettingsStore, clientID string) (config.Enrolment, error) {
	// Resolution and removal happen inside one With() call: no TOCTOU
	// window between reading the record and deleting it.
	var removed config.Enrolment
	if err := config.WithDeclinable(store, func(s *config.Settings) error {
		var found bool
		if removed, found = removeEnrolment(s, clientID); !found {
			return fmt.Errorf("%w: %q", ErrNotFound, clientID)
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrNotFound) {
			return config.Enrolment{}, err
		}
		return config.Enrolment{}, fmt.Errorf("failed to save settings: %w", err)
	}
	// Severing live connections is the half of revocation the record cannot
	// do. In a CLI process no hook is installed and this is a no-op: what a
	// CLI-only revocation buys is narrower than it looks, since the running
	// tray's listener re-resolves the enrolment from the FILE on every
	// request, so the very next call on an already-open socket is refused —
	// but the socket itself stays open until that client tries something.
	// Closing it outright needs the process that owns the connection table,
	// which is why the hook exists.
	CloseRevoked(removed.ClientID, removed.Fingerprint)
	removeBundle(removed.ClientID)
	return removed, nil
}

// removeBundle is best-effort: the operator may have already moved
// the bundle to the client and deleted it here, and a revocation must not
// fail because the host's copy is already gone. Revocation's teeth are the
// deleted record and the closed connection, not this.
func removeBundle(clientID string) {
	if !SafeID(clientID) {
		return // never join an unvalidated id into a path we then remove
	}
	_ = os.RemoveAll(filepath.Join(bridge.ConfigDir(), BundleDir, clientID))
}
