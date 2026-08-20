package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newEnrolmentSandbox returns a sandboxed store whose config dir also holds
// the CA and the emitted bundles, so nothing here can touch the real
// ~/Library/Application Support/relay.
func newEnrolmentSandbox(t *testing.T) (string, SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	return dir, store
}

// mkStoreProject creates a project of the given kind in the store and returns
// it. path must be empty for a remote project (validateProjectShape).
func mkStoreProject(t *testing.T, store SettingsStore, kind ProjectKind, name, path string) Project {
	t.Helper()
	var proj Project
	var createErr error
	assertNoErr(t, store.With(func(s *Settings) {
		proj, createErr = s.CreateProjectWithTokenKind(kind, name, path, []string{}, []string{}, nil, nil)
	}), "create %s project", kind)
	assertNoErr(t, createErr, "create %s project", kind)
	return proj
}

// A grant naming a local project is refused outright. Without this rule every
// protection ADR-009 built is bypassed by pointing at the wrong project rather
// than by defeating any of them.
func TestCreateEnrolment_RefusesLocalProjectGrant(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	local := mkStoreProject(t, store, ProjectKindLocal, "Workspace", dir)

	_, err := createEnrolment(store, enrolmentRequest{
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
	if _, err := os.Stat(filepath.Join(dir, enrolmentBundleDir, "hermes-mail")); !os.IsNotExist(err) {
		t.Fatalf("refused enrolment left a bundle behind: %v", err)
	}
}

// The happy path: a remote-kind grant enrols, persists, and emits the three
// files that get copied to the client machine.
func TestCreateEnrolment_RemoteProjectGrantSucceedsAndEmitsBundle(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	bundle, err := createEnrolment(store, enrolmentRequest{
		ClientID:   "hermes-mail",
		ProjectIDs: []string{mail.ID},
	})
	assertNoErr(t, err, "createEnrolment")

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
	if e.Budget.MaxCalls != defaultEnrolmentMaxCalls || e.Budget.MaxResultBytes != defaultEnrolmentMaxResultBytes || e.Budget.WindowSeconds != defaultEnrolmentWindowSeconds {
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
	caPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	assertNoErr(t, err, "read ca cert")
	bundledCA, err := os.ReadFile(bundle.CACertPath)
	assertNoErr(t, err, "read bundled ca cert")
	if string(caPEM) != string(bundledCA) {
		t.Fatal("bundle's ca.crt is not relay's CA certificate — the client could not verify the server with it")
	}

	// Resolution at connection time is by certificate, and it is the
	// certificate that carries the grant.
	s := store.Get()
	resolved := s.FindEnrolmentByFingerprint(e.Fingerprint)
	if resolved == nil || resolved.ClientID != "hermes-mail" {
		t.Fatalf("FindEnrolmentByFingerprint did not resolve the enrolment: %+v", resolved)
	}
	if s.FindEnrolmentByFingerprint("sha256:"+strings.Repeat("0", 64)) != nil {
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
	_, err := createEnrolment(store, enrolmentRequest{
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
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail"})
	assertNoErr(t, err, "first enrolment")

	if _, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail"}); err == nil {
		t.Fatal("a duplicate client id must be refused")
	}
	if _, err := createEnrolment(store, enrolmentRequest{ClientID: "../escape"}); err == nil {
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
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	cal := mkStoreProject(t, store, ProjectKindRemote, "Calendar", "")

	a, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}})
	assertNoErr(t, err, "enrol hermes-mail")
	b, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-cal", ProjectIDs: []string{cal.ID}})
	assertNoErr(t, err, "enrol hermes-cal")
	c, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-triage", ProjectIDs: []string{mail.ID, cal.ID}})
	assertNoErr(t, err, "enrol hermes-triage")

	if a.Enrolment.Fingerprint == b.Enrolment.Fingerprint || b.Enrolment.Fingerprint == c.Enrolment.Fingerprint {
		t.Fatal("co-located enrolments share a fingerprint — they must be distinct identities")
	}
	s := store.Get()
	if len(s.Enrolments) != 3 {
		t.Fatalf("want 3 co-existing enrolments, got %d", len(s.Enrolments))
	}
	holders := s.EnrolmentsGrantingProject(mail.ID)
	if len(holders) != 2 {
		t.Fatalf("want 2 enrolments granting Mail, got %v", holders)
	}

	// Revoking one leaves its neighbours untouched.
	_, err = revokeEnrolment(store, "hermes-mail")
	assertNoErr(t, err, "revoke hermes-mail")
	after := store.Get()
	if len(after.Enrolments) != 2 || after.FindEnrolment("hermes-cal") == nil || after.FindEnrolment("hermes-triage") == nil {
		t.Fatalf("revoking one enrolment disturbed the others: %+v", after.Enrolments)
	}
	if after.FindEnrolmentByFingerprint(c.Enrolment.Fingerprint) == nil {
		t.Fatal("a surviving enrolment stopped resolving by certificate")
	}
}

// The full fingerprint must survive the settings.json round trip: it is what
// keeps a revoked device's history legible, and a value truncated on write
// would be unrecoverable.
func TestEnrolmentFingerprint_RoundTripsThroughSettingsAtFullLength(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	bundle, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail"})
	assertNoErr(t, err, "createEnrolment")

	// Read settings.json off disk rather than through the cache, so a store
	// that only kept the value in memory cannot pass.
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	var onDisk Settings
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
func TestProjectConvertRemoteToLocal_RefusedWhileEnrolled(t *testing.T) {
	s := &Settings{}
	mail, err := s.CreateProjectWithTokenKind(ProjectKindRemote, "Mail", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create remote project")
	s.AddEnrolment(Enrolment{
		ClientID:    "hermes-mail",
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		ProjectIDs:  []string{mail.ID},
	})
	schemas := func() map[string]json.RawMessage { return nil }

	local := ProjectKindLocal
	path := t.TempDir()
	_, _, err = applyProjectUpdate(s, mail.ID, projectUpdateFields{Kind: &local, Path: &path}, schemas)
	if err == nil {
		t.Fatal("converting a remote project to local must be refused while an enrolment grants it")
	}
	if !strings.Contains(err.Error(), "hermes-mail") {
		t.Fatalf("refusal must name the offending enrolment, got: %v", err)
	}

	// The refusal must have changed nothing.
	after, _ := s.findProjectByID(mail.ID)
	if !after.IsRemote() || after.Path != "" {
		t.Fatalf("refused conversion mutated the project: kind=%q path=%q", after.Kind, after.Path)
	}

	// Revoking the enrolment makes the conversion legal — capability and
	// device revocation stay independent, and neither strands the other.
	if _, ok := s.RemoveEnrolment("hermes-mail"); !ok {
		t.Fatal("RemoveEnrolment did not find the enrolment it just stored")
	}
	if _, _, err := applyProjectUpdate(s, mail.ID, projectUpdateFields{Kind: &local, Path: &path}, schemas); err != nil {
		t.Fatalf("conversion should be legal once no enrolment grants the project: %v", err)
	}
	converted, _ := s.findProjectByID(mail.ID)
	if converted.IsRemote() {
		t.Fatal("project did not convert to local after the enrolment was revoked")
	}
}

// Belt-and-braces, in the shape UpdateProjectPath already uses: the exported
// mutator refuses the same conversion on its own, so a caller that skips
// applyProjectUpdate cannot produce the silent widening.
func TestUpdateProjectKind_RefusesRemoteToLocalWhileEnrolled(t *testing.T) {
	s := &Settings{}
	mail, err := s.CreateProjectWithTokenKind(ProjectKindRemote, "Mail", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create remote project")
	s.AddEnrolment(Enrolment{
		ClientID:    "hermes-mail",
		Fingerprint: "sha256:" + strings.Repeat("b", 64),
		ProjectIDs:  []string{mail.ID},
	})

	s.UpdateProjectKind(mail.ID, ProjectKindLocal)
	if proj, _ := s.findProjectByID(mail.ID); !proj.IsRemote() {
		t.Fatal("UpdateProjectKind converted an enrolled remote project to local")
	}

	// Unrelated projects, and remote→remote no-ops, stay unaffected.
	other, err := s.CreateProjectWithTokenKind(ProjectKindRemote, "Calendar", "", []string{}, []string{}, nil, nil)
	assertNoErr(t, err, "create second remote project")
	s.UpdateProjectKind(other.ID, ProjectKindLocal)
	if proj, _ := s.findProjectByID(other.ID); proj.IsRemote() {
		t.Fatal("an unenrolled remote project must still be convertible")
	}
}

// Revocation deletes the record, hands the fingerprint to the hook the
// listener installs (its cue to close live connections), and removes the
// host's copy of the bundle.
func TestRevokeEnrolment_RemovesRecordFiresHookAndDeletesBundle(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	bundle, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail"})
	assertNoErr(t, err, "createEnrolment")

	var gotClientID, gotFingerprint string
	SetEnrolmentRevocationHook(func(clientID, fingerprint string) {
		gotClientID, gotFingerprint = clientID, fingerprint
	})
	t.Cleanup(func() { SetEnrolmentRevocationHook(nil) })

	removed, err := revokeEnrolment(store, "hermes-mail")
	assertNoErr(t, err, "revokeEnrolment")

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
	if s.FindEnrolmentByFingerprint(bundle.Enrolment.Fingerprint) != nil {
		t.Fatal("a revoked certificate still resolves to an enrolment")
	}
	if _, err := os.Stat(bundle.Dir); !os.IsNotExist(err) {
		t.Fatalf("revocation left the bundle on disk: %v", err)
	}
	if _, err := revokeEnrolment(store, "hermes-mail"); err == nil {
		t.Fatal("revoking an unknown client id must report it, not succeed silently")
	}
}

// Revocation works with no hook installed — a CLI revocation with the tray
// stopped has no live connection to sever, and must not depend on one.
func TestRevokeEnrolment_WorksWithNoHookInstalled(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	SetEnrolmentRevocationHook(nil)
	_, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail"})
	assertNoErr(t, err, "createEnrolment")
	_, err = revokeEnrolment(store, "hermes-mail")
	assertNoErr(t, err, "revokeEnrolment without a hook")
}

// An install that never enrols a client keeps a settings.json with no
// enrolments key at all — the same round-trip guarantee Project.Kind has.
func TestSettings_OmitsEnrolmentsWhenNoneExist(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	if strings.Contains(string(raw), "enrolments") {
		t.Fatalf("settings.json grew an enrolments key with no enrolments:\n%s", raw)
	}
}
