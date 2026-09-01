package enrolment

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/google/uuid"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newEnrolmentSandbox returns a sandboxed store whose config dir also holds
// the CA and the emitted bundles, so nothing here can touch the real
// ~/Library/Application Support/relay.
func newEnrolmentSandbox(t *testing.T) (string, config.SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	return dir, store
}

// mkStoreProject stores a project of the given kind and returns it. Built
// here rather than through internal/project's CreateWithTokenKind: that
// one also mints a project token and derives permissions, none of which any
// rule in this package reads — grant validation reaches a stored project by
// id and asks IsRemote(), and nothing else. path must be empty for a remote
// project.
func mkStoreProject(t *testing.T, store config.SettingsStore, kind config.ProjectKind, name, path string) config.Project {
	t.Helper()
	proj := config.Project{
		ID:            uuid.New().String(),
		Kind:          config.NormalizeProjectKind(kind),
		Name:          name,
		Path:          path,
		AllowedMcpIDs: []string{},
		AllowedModels: []string{},
	}
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.AddProject(proj)
	}), "create %s project", kind)
	return proj
}

// A grant naming a local project is refused outright. Without this rule every
// protection ADR-009 built is bypassed by pointing at the wrong project rather
// than by defeating any of them.
func TestCreateEnrolment_RefusesLocalProjectGrant(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	local := mkStoreProject(t, store, config.ProjectKindLocal, "Workspace", dir)

	_, err := Create(store, Request{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{local.ID},
	})
	if err == nil {
		t.Fatal("enrolling a grant that names a local project must be refused")
	}
	if !strings.Contains(err.Error(), local.ID) || !strings.Contains(err.Error(), "Workspace") {
		t.Fatalf("refusal must name the offending project, got: %v", err)
	}

	// A refused enrolment persists nothing and emits no credential — a key on
	// disk that no enrolment references is one nobody knows to revoke.
	if got := store.Get().Enrolments; len(got) != 0 {
		t.Fatalf("refused enrolment was persisted anyway: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, BundleDir, "hermes-mail")); !os.IsNotExist(err) {
		t.Fatalf("refused enrolment left a bundle behind: %v", err)
	}
}

// The happy path: a remote-kind grant enrols, persists, and emits the three
// files that get copied to the client machine.
func TestCreateEnrolment_RemoteProjectGrantSucceedsAndEmitsBundle(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")

	bundle, err := Create(store, Request{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{mail.ID},
	})
	assertNoErr(t, err, "Create")

	stored := store.Get().Enrolments
	if len(stored) != 1 {
		t.Fatalf("want 1 enrolment persisted, got %d", len(stored))
	}
	e := stored[0]
	if e.ClientID != "hermes-mail" || !e.GrantsProject(mail.ID) {
		t.Fatalf("persisted enrolment does not match the request: %+v", e)
	}
	if e.CreatedAt == "" {
		t.Fatal("enrolment has no created-at")
	}
	// Budgets are stored even though enforcement lives elsewhere, and an
	// unset budget must never read as "unlimited".
	if e.Budget.MaxCalls != DefaultMaxCalls || e.Budget.MaxResultBytes != DefaultMaxResultBytes || e.Budget.WindowSeconds != DefaultWindowSeconds {
		t.Fatalf("unset budget did not take the conservative defaults: %+v", e.Budget)
	}

	// The bundle is key + cert + CA cert, tight permissions, and the
	// fingerprint on record is the fingerprint of the cert that shipped.
	for _, path := range []string{bundle.KeyPath, bundle.CertPath, bundle.CACertPath} {
		info, err := os.Stat(path)
		assertNoErr(t, err, "stat %s", path)
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("%s mode = %#o, want 0600", path, perm)
		}
	}
	if info, err := os.Stat(bundle.Dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("bundle dir %s: err=%v mode=%v, want 0700", bundle.Dir, err, info.Mode().Perm())
	}
	certPEM, err := os.ReadFile(bundle.CertPath)
	assertNoErr(t, err, "read client cert")
	if got := FingerprintCert(parseCertPEM(t, certPEM)); got != e.Fingerprint {
		t.Fatalf("recorded fingerprint %q != emitted certificate's %q", e.Fingerprint, got)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	assertNoErr(t, err, "read ca cert")
	bundledCA, err := os.ReadFile(bundle.CACertPath)
	assertNoErr(t, err, "read bundled ca cert")
	if string(caPEM) != string(bundledCA) {
		t.Fatal("bundle's ca.crt is not relay's CA certificate — the client could not verify the server with it")
	}

	// Resolution at connection time is by certificate, and it is the
	// certificate that carries the grant.
	s := store.Get()
	resolved := FindByFingerprint(s, e.Fingerprint)
	if resolved == nil || resolved.ClientID != "hermes-mail" {
		t.Fatalf("FindEnrolmentByFingerprint did not resolve the enrolment: %+v", resolved)
	}
	if FindByFingerprint(s, "sha256:"+strings.Repeat("0", 64)) != nil {
		t.Fatal("an unknown fingerprint must resolve to nothing")
	}
	if resolved.GrantsProject("proj-nobody-granted") {
		t.Fatal("enrolment claims a grant it does not hold")
	}
}

// A grant relay cannot resolve is not a grant to keep: silently dropping it
// would let a later project reusing that id inherit it.
func TestCreateEnrolment_RefusesUnknownProjectGrant(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	_, err := Create(store, Request{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{"proj_does_not_exist"},
	})
	if err == nil || !strings.Contains(err.Error(), "proj_does_not_exist") {
		t.Fatalf("want a refusal naming the unknown project, got: %v", err)
	}
}

// Client ids are unique and filesystem-safe: the id names the bundle
// directory, and a duplicate would make two certificates answer to one name.
func TestCreateEnrolment_RefusesDuplicateAndUnsafeClientIDs(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	_, err := Create(store, Request{ClientID: "hermes-mail"})
	assertNoErr(t, err, "first enrolment")

	if _, err := Create(store, Request{ClientID: "hermes-mail"}); err == nil {
		t.Fatal("a duplicate client id must be refused")
	}
	if _, err := Create(store, Request{ClientID: "../escape"}); err == nil {
		t.Fatal("a client id with path separators must be refused")
	}
	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("want 1 enrolment after two refusals, got %d", got)
	}
}

// An enrolment is keyed by certificate, NOT by machine: several agents on one
// host each hold their own enrolment, granted and revoked independently.
// Nothing may assume one-per-machine.
func TestEnrolments_AreKeyedByCertificateNotByMachine(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	cal := mkStoreProject(t, store, config.ProjectKindRemote, "Calendar", "")

	a, err := Create(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "enrol hermes-mail")
	b, err := Create(store, Request{ClientID: "hermes-cal", ProjectIDs: []string{cal.ID}})
	assertNoErr(t, err, "enrol hermes-cal")
	c, err := Create(store, Request{ClientID: "hermes-triage", ProjectIDs: []string{mail.ID, cal.ID}})
	assertNoErr(t, err, "enrol hermes-triage")

	if a.Enrolment.Fingerprint == b.Enrolment.Fingerprint || b.Enrolment.Fingerprint == c.Enrolment.Fingerprint {
		t.Fatal("co-located enrolments share a fingerprint — they must be distinct identities")
	}
	s := store.Get()
	if len(s.Enrolments) != 3 {
		t.Fatalf("want 3 co-existing enrolments, got %d", len(s.Enrolments))
	}
	holders := GrantingProject(s, mail.ID)
	if len(holders) != 2 {
		t.Fatalf("want 2 enrolments granting Mail, got %v", holders)
	}

	// Revoking one leaves its neighbours untouched.
	_, err = Revoke(store, "hermes-mail")
	assertNoErr(t, err, "revoke hermes-mail")
	after := store.Get()
	if len(after.Enrolments) != 2 || Find(after, "hermes-cal") == nil || Find(after, "hermes-triage") == nil {
		t.Fatalf("revoking one enrolment disturbed the others: %+v", after.Enrolments)
	}
	if FindByFingerprint(after, c.Enrolment.Fingerprint) == nil {
		t.Fatal("a surviving enrolment stopped resolving by certificate")
	}
}

// The full fingerprint must survive the settings.json round trip: it is what
// keeps a revoked device's history legible, and a value truncated on write
// would be unrecoverable.
func TestEnrolmentFingerprint_RoundTripsThroughSettingsAtFullLength(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	bundle, err := Create(store, Request{ClientID: "hermes-mail"})
	assertNoErr(t, err, "Create")

	// Read settings.json off disk rather than through the cache, so a store
	// that only kept the value in memory cannot pass.
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	var onDisk config.Settings
	assertNoErr(t, json.Unmarshal(raw, &onDisk), "parse settings.json")

	if len(onDisk.Enrolments) != 1 {
		t.Fatalf("want 1 enrolment on disk, got %d", len(onDisk.Enrolments))
	}
	got := onDisk.Enrolments[0].Fingerprint
	if got != bundle.Enrolment.Fingerprint {
		t.Fatalf("fingerprint changed on the way to disk: %q != %q", got, bundle.Enrolment.Fingerprint)
	}
	if len(strings.TrimPrefix(got, "sha256:")) != 64 {
		t.Fatalf("persisted fingerprint is not full length: %q", got)
	}

	// There is no bearer token anywhere on this path — a stolen settings.json
	// must grant no remote access. Guard the serialized form, since that is
	// the artifact an attacker would read.
	for _, forbidden := range []string{"\"token\"", "\"secret\"", "\"bearer\""} {
		if strings.Contains(strings.ToLower(string(mustMarshal(t, onDisk.Enrolments[0]))), forbidden) {
			t.Fatalf("enrolment serialized a %s field — decision 2 leaves no bearer credential on the remote path", forbidden)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	assertNoErr(t, err, "marshal")
	return data
}

// Converting a project remote→local is refused while any enrolment grants it,
// and the refusal names the offending enrolments. Mirrors ADR-009's
// constrained local→remote conversion: refuse the edit that would strand a
// credential rather than allowing it and cleaning up afterwards.
// Revocation deletes the record, hands the fingerprint to the hook the
// listener installs (its cue to close live connections), and removes the
// host's copy of the bundle.
func TestRevokeEnrolment_RemovesRecordFiresHookAndDeletesBundle(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	bundle, err := Create(store, Request{ClientID: "hermes-mail"})
	assertNoErr(t, err, "Create")

	var gotClientID, gotFingerprint string
	SetRevocationHook(func(clientID, fingerprint string) {
		gotClientID, gotFingerprint = clientID, fingerprint
	})
	t.Cleanup(func() { SetRevocationHook(nil) })

	removed, err := Revoke(store, "hermes-mail")
	assertNoErr(t, err, "Revoke")

	if removed.Fingerprint != bundle.Enrolment.Fingerprint {
		t.Fatalf("revoked record %q is not the one created %q", removed.Fingerprint, bundle.Enrolment.Fingerprint)
	}
	if gotClientID != "hermes-mail" || gotFingerprint != bundle.Enrolment.Fingerprint {
		t.Fatalf("revocation hook got (%q, %q), want the revoked client id and its full fingerprint", gotClientID, gotFingerprint)
	}
	s := store.Get()
	if len(s.Enrolments) != 0 {
		t.Fatalf("revocation left the record behind: %+v", s.Enrolments)
	}
	if FindByFingerprint(s, bundle.Enrolment.Fingerprint) != nil {
		t.Fatal("a revoked certificate still resolves to an enrolment")
	}
	if _, err := os.Stat(bundle.Dir); !os.IsNotExist(err) {
		t.Fatalf("revocation left the bundle on disk: %v", err)
	}
	if _, err := Revoke(store, "hermes-mail"); err == nil {
		t.Fatal("revoking an unknown client id must report it, not succeed silently")
	}
}

// Revocation works with no hook installed — a CLI revocation with the tray
// stopped has no live connection to sever, and must not depend on one.
func TestRevokeEnrolment_WorksWithNoHookInstalled(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	SetRevocationHook(nil)
	_, err := Create(store, Request{ClientID: "hermes-mail"})
	assertNoErr(t, err, "Create")
	_, err = Revoke(store, "hermes-mail")
	assertNoErr(t, err, "Revoke without a hook")
}

// The retuned single-user-host defaults (window_seconds 3600, max_calls 120,
// max_result_bytes 64 MiB) apply to a budget with every field left zero —
// zero must still never mean "unlimited" after the retune, exactly as before
// it.
func TestNormalizeEnrolmentBudget_ZeroFieldsTakeTheRetunedDefaults(t *testing.T) {
	got := NormalizeBudget(config.EnrolmentBudget{})
	want := config.EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20}
	if got != want {
		t.Fatalf("NormalizeBudget(zero) = %+v, want %+v", got, want)
	}
	// Pinned against the named constants too, so a future edit to one without
	// the other cannot pass silently.
	if DefaultWindowSeconds != 3600 || DefaultMaxCalls != 120 || DefaultMaxResultBytes != 64<<20 {
		t.Fatalf("default constants drifted from the documented retune: window=%d calls=%d bytes=%d",
			DefaultWindowSeconds, DefaultMaxCalls, DefaultMaxResultBytes)
	}
}

// ---------------------------------------------------------------------------
// Update — retuning a budget or regranting profiles WITHOUT
// reissuing the certificate.
// ---------------------------------------------------------------------------

// A budget-only update changes exactly the field named and leaves every other
// field, every grant, and the identity of the enrolment (client id,
// fingerprint, created-at) untouched — and so does the certificate on disk.
func TestUpdateEnrolment_ChangesOnlyNamedField(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	bundle, err := Create(store, Request{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{mail.ID},
	})
	assertNoErr(t, err, "Create")
	origCertPEM, err := os.ReadFile(bundle.CertPath)
	assertNoErr(t, err, "read cert before update")

	newMaxCalls := 500
	before, after, err := Update(store, UpdateRequest{
		ClientID: "hermes-mail",
		Budget:   BudgetUpdate{MaxCalls: &newMaxCalls},
	})
	assertNoErr(t, err, "Update")

	if after.Budget.MaxCalls != newMaxCalls {
		t.Fatalf("the named field did not change: got %d, want %d", after.Budget.MaxCalls, newMaxCalls)
	}
	if after.Budget.WindowSeconds != before.Budget.WindowSeconds || after.Budget.MaxResultBytes != before.Budget.MaxResultBytes {
		t.Fatalf("a field that was not named changed anyway: before=%+v after=%+v", before.Budget, after.Budget)
	}
	if !slices.Equal(after.ProjectIDs, before.ProjectIDs) {
		t.Fatalf("grants changed when --grant was never passed: before=%v after=%v", before.ProjectIDs, after.ProjectIDs)
	}
	if after.ClientID != before.ClientID || after.Fingerprint != before.Fingerprint || after.CreatedAt != before.CreatedAt {
		t.Fatalf("an identity field moved on a budget update: before=%+v after=%+v", before, after)
	}

	// The certificate itself — the thing revoke+recreate would have reissued
	// — is byte-for-byte unchanged. This is the load-bearing assertion:
	// silently reissuing it would be the worst possible bug in this feature.
	gotCertPEM, err := os.ReadFile(bundle.CertPath)
	assertNoErr(t, err, "read cert after update")
	if string(gotCertPEM) != string(origCertPEM) {
		t.Fatal("the certificate on disk changed after a budget-only update")
	}
	stored := Find(store.Get(), "hermes-mail")
	if stored == nil || stored.Fingerprint != before.Fingerprint {
		t.Fatalf("the persisted fingerprint moved: %+v", stored)
	}
}

// An unset budget field must preserve whatever was already stored, not reset
// it to the conservative default — those are different actions and only an
// explicit 0 asks for the second one (the next test).
func TestUpdateEnrolment_UnsetFieldsPreserveStoredValueNotDefault(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	_, err := Create(store, Request{
		ClientID: "hermes-mail",
		Budget:   config.EnrolmentBudget{WindowSeconds: 7200, MaxCalls: 999, MaxResultBytes: 999 << 20},
	})
	assertNoErr(t, err, "Create")

	newWindow := 1800
	_, after, err := Update(store, UpdateRequest{
		ClientID: "hermes-mail",
		Budget:   BudgetUpdate{WindowSeconds: &newWindow},
	})
	assertNoErr(t, err, "Update")

	if after.Budget.WindowSeconds != 1800 {
		t.Fatalf("the named field did not change: %+v", after.Budget)
	}
	if after.Budget.MaxCalls != 999 || after.Budget.MaxResultBytes != 999<<20 {
		t.Fatalf("unset fields were reset to the default instead of preserved: %+v", after.Budget)
	}
}

// An explicit 0 is not "leave alone" — it is "use the default", the same
// meaning NormalizeBudget gives it everywhere else. This is what
// makes the pointer (not a zero check) the right representation for "unset".
func TestUpdateEnrolment_ExplicitZeroResetsToDefault(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	_, err := Create(store, Request{
		ClientID: "hermes-mail",
		Budget:   config.EnrolmentBudget{WindowSeconds: 7200, MaxCalls: 999, MaxResultBytes: 999 << 20},
	})
	assertNoErr(t, err, "Create")

	zero := 0
	_, after, err := Update(store, UpdateRequest{
		ClientID: "hermes-mail",
		Budget:   BudgetUpdate{MaxCalls: &zero},
	})
	assertNoErr(t, err, "Update")
	if after.Budget.MaxCalls != DefaultMaxCalls {
		t.Fatalf("an explicit 0 must reset to the default (%d), got %d", DefaultMaxCalls, after.Budget.MaxCalls)
	}
	// The fields not named are still preserved.
	if after.Budget.WindowSeconds != 7200 || after.Budget.MaxResultBytes != 999<<20 {
		t.Fatalf("an explicit 0 on one field disturbed the others: %+v", after.Budget)
	}
}

// An unknown client id is refused, not silently ignored.
func TestUpdateEnrolment_RefusesUnknownClientID(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	v := 10
	_, _, err := Update(store, UpdateRequest{
		ClientID: "does-not-exist",
		Budget:   BudgetUpdate{MaxCalls: &v},
	})
	if err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("want a refusal naming the unknown client id, got: %v", err)
	}
}

// --grant on update runs the same ValidateGrants check create does:
// a grant naming a local project is refused, exactly as it would be at
// creation, and a refused update leaves the stored grants untouched. A
// legitimate replacement (including replacing down to zero grants) works.
func TestUpdateEnrolment_GrantsValidateAndReplaceWholeList(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	cal := mkStoreProject(t, store, config.ProjectKindRemote, "Calendar", "")
	local := mkStoreProject(t, store, config.ProjectKindLocal, "Workspace", dir)

	_, err := Create(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "Create")

	// A legitimate replacement swaps the whole list, not just appends.
	newGrants := []string{cal.ID}
	before, after, err := Update(store, UpdateRequest{ClientID: "hermes-mail", ProjectIDs: &newGrants})
	assertNoErr(t, err, "Update: replace grants")
	if !slices.Equal(before.ProjectIDs, []string{mail.ID}) {
		t.Fatalf("before-snapshot is wrong: %v", before.ProjectIDs)
	}
	if !slices.Equal(after.ProjectIDs, []string{cal.ID}) {
		t.Fatalf("grants did not replace as requested: %v", after.ProjectIDs)
	}

	// A grant naming a local project is refused, and names the offender —
	// same rule as create, same message shape.
	badGrants := []string{local.ID}
	_, _, err = Update(store, UpdateRequest{ClientID: "hermes-mail", ProjectIDs: &badGrants})
	if err == nil || !strings.Contains(err.Error(), local.ID) {
		t.Fatalf("want a refusal naming the local project, got: %v", err)
	}
	// The refusal changed nothing.
	stored := Find(store.Get(), "hermes-mail")
	if stored == nil || !slices.Equal(stored.ProjectIDs, []string{cal.ID}) {
		t.Fatalf("a refused grant update mutated the stored record: %+v", stored)
	}

	// Replacing down to zero grants (--clear-grants) is a legitimate action,
	// distinct from leaving grants alone: it withdraws every profile without
	// touching the certificate.
	empty := []string{}
	_, after, err = Update(store, UpdateRequest{ClientID: "hermes-mail", ProjectIDs: &empty})
	assertNoErr(t, err, "Update: clear grants")
	if len(after.ProjectIDs) != 0 {
		t.Fatalf("grants were not cleared: %v", after.ProjectIDs)
	}
}

// A budget-only update must succeed even when the enrolment's existing grant
// names a profile that has since been deleted (the #23 dangling-grant case
// docs/access-profiles.md describes) — re-validating grants nobody asked to
// change would turn an unrelated budget edit into a refusal.
func TestUpdateEnrolment_BudgetOnlyUpdateSurvivesDanglingGrant(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	_, err := Create(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "Create")

	assertNoErr(t, store.With(func(s *config.Settings) { s.RemoveProject(mail.ID) }), "delete the granted profile out from under the enrolment")

	newMaxCalls := 42
	_, after, err := Update(store, UpdateRequest{
		ClientID: "hermes-mail",
		Budget:   BudgetUpdate{MaxCalls: &newMaxCalls},
	})
	assertNoErr(t, err, "a budget-only update must not re-validate untouched grants")
	if after.Budget.MaxCalls != 42 {
		t.Fatalf("the named field did not change: %+v", after.Budget)
	}
	if !slices.Equal(after.ProjectIDs, []string{mail.ID}) {
		t.Fatalf("grants changed on a budget-only update: %v", after.ProjectIDs)
	}
}

// An install that never enrols a client keeps a settings.json with no
// enrolments key at all — the same round-trip guarantee Project.Kind has.
func TestSettings_OmitsEnrolmentsWhenNoneExist(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	if strings.Contains(string(raw), "enrolments") {
		t.Fatalf("settings.json grew an enrolments key with no enrolments:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// Sign — the CSR path: relay never generates or sees a client
// private key, and the bundle it writes has no key at all.
// ---------------------------------------------------------------------------

func parseCSRForTest(t *testing.T, csrPEM []byte) *x509.CertificateRequest {
	t.Helper()
	csr, err := ParseClientCSR(csrPEM)
	assertNoErr(t, err, "ParseClientCSR")
	return csr
}

// The happy path: Sign persists the record, and the bundle it
// writes to disk is client.crt and ca.crt only — no client.key, and the
// returned bundle's KeyPath is empty.
func TestSignEnrolment_HappyPathEmitsCertOnlyBundle(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	csr := parseCSRForTest(t, genClientCSRPEM(t, "hermes-mail"))

	bundle, err := Sign(store, Request{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{mail.ID},
	}, csr)
	assertNoErr(t, err, "Sign")

	if bundle.KeyPath != "" {
		t.Fatalf("KeyPath = %q, want empty — the CSR path never writes a key", bundle.KeyPath)
	}
	entries, err := os.ReadDir(bundle.Dir)
	assertNoErr(t, err, "read bundle dir")
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"ca.crt", "client.crt"}) {
		t.Fatalf("bundle dir contains %v, want exactly [ca.crt client.crt]", names)
	}
	for _, path := range []string{bundle.CertPath, bundle.CACertPath} {
		info, err := os.Stat(path)
		assertNoErr(t, err, "stat %s", path)
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("%s mode = %#o, want 0600", path, perm)
		}
	}
	if info, err := os.Stat(bundle.Dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("bundle dir %s: err=%v mode=%v, want 0700", bundle.Dir, err, info.Mode().Perm())
	}

	stored := Find(store.Get(), "hermes-mail")
	if stored == nil {
		t.Fatal("enrolment was not persisted")
	}
	if stored.SPKISHA256 == "" {
		t.Fatal("SPKISHA256 was not recorded for a CSR-signed enrolment")
	}
	if stored.SPKISHA256 != SPKISHA256Hex(csr.RawSubjectPublicKeyInfo) {
		t.Fatalf("SPKISHA256 = %q, want the CSR's own SPKI hash", stored.SPKISHA256)
	}
	if stored.Fingerprint != bundle.Enrolment.Fingerprint {
		t.Fatalf("stored fingerprint %q != bundle's %q", stored.Fingerprint, bundle.Enrolment.Fingerprint)
	}
	certPEM, err := os.ReadFile(bundle.CertPath)
	assertNoErr(t, err, "read client cert")
	if got := FingerprintCert(parseCertPEM(t, certPEM)); got != stored.Fingerprint {
		t.Fatalf("recorded fingerprint %q != emitted certificate's %q", stored.Fingerprint, got)
	}
	if resolved := FindByFingerprint(store.Get(), stored.Fingerprint); resolved == nil || resolved.ClientID != "hermes-mail" {
		t.Fatalf("FindEnrolmentByFingerprint did not resolve the signed enrolment: %+v", resolved)
	}
}

// AC-20: --client-id naming an existing enrolment is refused — and this is
// independent of the CSR's own CN. A CSR whose CN collides with an
// existing enrolment but whose --client-id does not succeeds, and the
// issued certificate's CN is --client-id, never the CSR's.
func TestSignEnrolment_ClientIDCollisionRefusedButCSR_CNCollisionAloneSucceeds(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	seeded, err := Create(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "seed existing enrolment")
	certBefore, err := os.ReadFile(seeded.CertPath)
	assertNoErr(t, err, "read seeded client.crt")

	// --client-id collides: refused, store and bundle both untouched.
	csrSameClientID := parseCSRForTest(t, genClientCSRPEM(t, "some-other-cn"))
	_, err = Sign(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}}, csrSameClientID)
	if err == nil || !strings.Contains(err.Error(), "hermes-mail") {
		t.Fatalf("a --client-id collision must be refused naming it, got: %v", err)
	}
	if len(store.Get().Enrolments) != 1 {
		t.Fatalf("a refused sign must not add a second enrolment: %+v", store.Get().Enrolments)
	}
	certAfter, err := os.ReadFile(seeded.CertPath)
	assertNoErr(t, err, "read client.crt after refused collision")
	if string(certAfter) != string(certBefore) {
		t.Fatal("a refused --client-id collision must not write a new client.crt over the existing one")
	}

	// The CSR's own CN collides with the existing enrolment's client id,
	// but --client-id names something else entirely: this succeeds, and
	// the issued certificate's CN is --client-id, not the CSR's CN.
	csrCNCollides := parseCSRForTest(t, genClientCSRPEM(t, "hermes-mail"))
	bundle, err := Sign(store, Request{ClientID: "hermes-mail-2", ProjectIDs: []string{mail.ID}}, csrCNCollides)
	assertNoErr(t, err, "a CSR CN colliding with another enrolment's client id must not block a distinct --client-id")

	certPEM, err := os.ReadFile(bundle.CertPath)
	assertNoErr(t, err, "read issued cert")
	cert := parseCertPEM(t, certPEM)
	if cert.Subject.CommonName != "hermes-mail-2" {
		t.Fatalf("issued certificate CN = %q, want the --client-id %q, not the CSR's own CN", cert.Subject.CommonName, "hermes-mail-2")
	}
}

// AC-21: signing two different CSRs generated from the SAME private key
// under two different client ids — the second is refused, naming the
// first client id.
func TestSignEnrolment_DuplicateSPKIRefusedNamingFirstClientID(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assertNoErr(t, err, "generate shared key")
	csrA := parseCSRForTest(t, genCSRPEMFromKey(t, key, 0, "hermes-a"))
	csrB := parseCSRForTest(t, genCSRPEMFromKey(t, key, 0, "hermes-b"))

	_, err = Sign(store, Request{ClientID: "hermes-a", ProjectIDs: []string{mail.ID}}, csrA)
	assertNoErr(t, err, "sign the first client over the shared key")

	_, err = Sign(store, Request{ClientID: "hermes-b", ProjectIDs: []string{mail.ID}}, csrB)
	if err == nil {
		t.Fatal("signing a second CSR over the SAME private key under a different client id must be refused")
	}
	if !strings.Contains(err.Error(), "hermes-a") {
		t.Fatalf("refusal must name the first client id (hermes-a), got: %v", err)
	}
	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("want 1 enrolment after the refused duplicate-key sign, got %d", got)
	}
}

// writeSignedCertBundle refuses outright when client.key already exists in
// the target directory: a stale key would make a CSR-issued bundle
// indistinguishable from a relay-generated one.
func TestWriteSignedCertBundle_RefusesWhenClientKeyAlreadyExists(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	// Seed a stale client.key the way Create would have left one.
	_, err := Create(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "seed a host-generated enrolment with a client.key")
	// Revoke the record but leave the key file behind, simulating an
	// operator who deleted only the settings entry.
	assertNoErr(t, store.With(func(s *config.Settings) { s.Enrolments = nil }), "clear the record, leaving the bundle dir")

	keyPath := filepath.Join(dir, BundleDir, "hermes-mail", "client.key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("test setup: stale client.key not present: %v", err)
	}

	e := config.Enrolment{ClientID: "hermes-mail"}
	_, err = writeSignedCertBundle(e, []byte("cert"), []byte("ca"))
	if err == nil || !strings.Contains(err.Error(), "client.key") {
		t.Fatalf("want a refusal naming client.key, got: %v", err)
	}
}

// AC-2: after a successful sign, the sandbox config dir contains no file
// whose CONTENT carries a "PRIVATE KEY" PEM header, other than
// ca.key.sealed (an opaque, encrypted envelope). Asserted by content, not
// by filename.
func TestSignEnrolment_LeavesNoPrivateKeyMaterialInTheSandbox(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	csr := parseCSRForTest(t, genClientCSRPEM(t, "hermes-mail"))
	_, err := Sign(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}}, csr)
	assertNoErr(t, err, "Sign")

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) == CAKeySealedFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "PRIVATE KEY") {
			t.Errorf("%s contains a PRIVATE KEY PEM header, and it is not ca.key.sealed", path)
		}
		return nil
	})
	assertNoErr(t, err, "walk sandbox dir")
}

// AC-22: a grant naming a local (non-remote) project is refused by
// ValidateGrants on the CSR path too — unchanged behaviour,
// reused rather than reimplemented.
func TestSignEnrolment_RefusesLocalProjectGrant(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	local := mkStoreProject(t, store, config.ProjectKindLocal, "Workspace", dir)
	csr := parseCSRForTest(t, genClientCSRPEM(t, "hermes-mail"))

	_, err := Sign(store, Request{ClientID: "hermes-mail", ProjectIDs: []string{local.ID}}, csr)
	if err == nil || !strings.Contains(err.Error(), local.ID) {
		t.Fatalf("a grant naming a local project must be refused naming it, got: %v", err)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Fatal("a refused sign must not persist an enrolment")
	}
	if _, statErr := os.Stat(filepath.Join(dir, BundleDir, "hermes-mail")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused sign left a bundle behind: %v", statErr)
	}
}
