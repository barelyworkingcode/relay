package main

// create, update and revoke are brokered (ADR-017 decision 2): `enrolUpdate`
// no longer calls updateEnrolment directly, so its flag-parsing is tested on
// its own, against parseEnrolUpdateFlags, with no store and no service dial
// involved at all. The end-to-end path (a real bridge server, a wired
// EnrolmentOps, the actual admin_op round trip) is covered separately below
// and in audit_issuance_test.go.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Creates an enrolment via the same path the tray's EnrolmentOps.Create
// uses, without going through flag parsing or a bridge round trip, so a
// test can set up a starting state concisely.
func enrolCreateForCLITest(t *testing.T, store SettingsStore, clientID string, projectIDs []string) {
	t.Helper()
	_, err := createEnrolment(store, enrolmentRequest{ClientID: clientID, ProjectIDs: projectIDs})
	assertNoErr(t, err, "createEnrolment")
}

// Flag detection is fs.Visit-based, not a zero check, so an unnamed flag must
// leave its field untouched (nil) rather than reset it (a pointer to zero) —
// this is the CLI-level check that the distinction survives flag parsing,
// independent of whatever door eventually applies the resulting request.
func TestParseEnrolUpdateFlags_OnlyTheNamedBudgetFlagIsSet(t *testing.T) {
	req := parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--max-calls", "999"})

	if req.ClientID != "hermes-mail" {
		t.Fatalf("ClientID = %q, want hermes-mail", req.ClientID)
	}
	if req.Budget.MaxCalls == nil || *req.Budget.MaxCalls != 999 {
		t.Fatalf("Budget.MaxCalls = %v, want a pointer to 999", req.Budget.MaxCalls)
	}
	if req.Budget.WindowSeconds != nil || req.Budget.MaxResultBytes != nil {
		t.Fatalf("an unnamed flag's field was set: %+v", req.Budget)
	}
	if req.ProjectIDs != nil {
		t.Fatalf("ProjectIDs = %v, want nil (neither --grant nor --clear-grants was passed)", req.ProjectIDs)
	}
}

func TestParseEnrolUpdateFlags_GrantAndClearGrants(t *testing.T) {
	req := parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--grant", "cal-project"})
	if req.ProjectIDs == nil || !slices.Equal(*req.ProjectIDs, []string{"cal-project"}) {
		t.Fatalf("--grant did not produce a replacement grant list: %v", req.ProjectIDs)
	}

	req = parseEnrolUpdateFlags([]string{"--client-id", "hermes-mail", "--clear-grants"})
	if req.ProjectIDs == nil || len(*req.ProjectIDs) != 0 {
		t.Fatalf("--clear-grants did not produce an empty (non-nil) grant list: %v", req.ProjectIDs)
	}
}

// TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker is the
// integration half: a real bridge server, a wired EnrolmentOps, and the
// actual admin_op round trip, proving the flags parsed above reach
// EnrolmentOps.Update rather than merely being well-formed on their own.
func TestEnrolUpdate_CLIDispatchesTheParsedRequestThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	enrolCreateForCLITest(t, store, "hermes-mail", []string{profile.ID})
	serveBroker(t, newBrokerRouter(t, store, nil))

	enrolUpdate(store, []string{"--client-id", "hermes-mail", "--max-calls", "999"})

	got := store.Get().FindEnrolment("hermes-mail").Budget
	if got.MaxCalls != 999 {
		t.Fatalf("MaxCalls = %d, want 999", got.MaxCalls)
	}
}

// writeTestCSRFile writes a valid P-256 CSR PEM to a file under t.TempDir()
// and returns its path, for tests exercising parseEnrolSignFlags' own
// --csr file I/O.
func writeTestCSRFile(t *testing.T, cn string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.csr")
	assertNoErr(t, os.WriteFile(path, genClientCSRPEM(t, cn), 0644), "write test CSR file")
	return path
}

// AC-5: parseEnrolSignFlags maps every flag to enrolmentSignFields,
// defaults the three budget fields to the default* constants, and returns
// --grant repeats in order — no store, no dial.
func TestParseEnrolSignFlags_MapsFlagsAndDefaultsBudget(t *testing.T) {
	csrPath := writeTestCSRFile(t, "hermes-mail")
	csrPEM, err := os.ReadFile(csrPath)
	assertNoErr(t, err, "read fixture CSR")

	got, err := parseEnrolSignFlags([]string{
		"--client-id", "hermes-mail",
		"--csr", csrPath,
		"--grant", "proj-a",
		"--grant", "proj-b",
	})
	assertNoErr(t, err, "parseEnrolSignFlags")

	if got.ClientID != "hermes-mail" {
		t.Fatalf("ClientID = %q, want hermes-mail", got.ClientID)
	}
	if !slices.Equal(got.ProjectIDs, []string{"proj-a", "proj-b"}) {
		t.Fatalf("ProjectIDs = %v, want [proj-a proj-b] in the order given", got.ProjectIDs)
	}
	want := EnrolmentBudget{WindowSeconds: defaultEnrolmentWindowSeconds, MaxCalls: defaultEnrolmentMaxCalls, MaxResultBytes: defaultEnrolmentMaxResultBytes}
	if got.Budget != want {
		t.Fatalf("Budget = %+v, want the defaults %+v", got.Budget, want)
	}
	if got.CSRPEM != string(csrPEM) {
		t.Fatal("CSRPEM does not match the file's contents")
	}
	if got.OutDir != "" {
		t.Fatalf("OutDir = %q, want empty when --out was not passed", got.OutDir)
	}
}

func TestParseEnrolSignFlags_BudgetFlagsAndOutDir(t *testing.T) {
	csrPath := writeTestCSRFile(t, "hermes-mail")
	got, err := parseEnrolSignFlags([]string{
		"--client-id", "hermes-mail",
		"--csr", csrPath,
		"--window-seconds", "1800",
		"--max-calls", "5",
		"--max-result-bytes", "1024",
		"--out", "/tmp/somewhere",
	})
	assertNoErr(t, err, "parseEnrolSignFlags")
	want := EnrolmentBudget{WindowSeconds: 1800, MaxCalls: 5, MaxResultBytes: 1024}
	if got.Budget != want {
		t.Fatalf("Budget = %+v, want %+v", got.Budget, want)
	}
	if got.OutDir != "/tmp/somewhere" {
		t.Fatalf("OutDir = %q, want /tmp/somewhere", got.OutDir)
	}
}

func TestParseEnrolSignFlags_ReadsCSRFromStdin(t *testing.T) {
	csrPath := writeTestCSRFile(t, "hermes-mail")
	csrPEM, err := os.ReadFile(csrPath)
	assertNoErr(t, err, "read fixture CSR")

	r, w, err := os.Pipe()
	assertNoErr(t, err, "os.Pipe")
	_, werr := w.Write(csrPEM)
	assertNoErr(t, werr, "write to pipe")
	assertNoErr(t, w.Close(), "close pipe writer")
	savedStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = savedStdin }()

	got, err := parseEnrolSignFlags([]string{"--client-id", "hermes-mail", "--csr", "-"})
	assertNoErr(t, err, "parseEnrolSignFlags with --csr -")
	if got.CSRPEM != string(csrPEM) {
		t.Fatal("CSRPEM read from stdin does not match the fixture")
	}
}

func TestParseEnrolSignFlags_RequiresClientIDAndCSR(t *testing.T) {
	csrPath := writeTestCSRFile(t, "hermes-mail")
	if _, err := parseEnrolSignFlags([]string{"--csr", csrPath}); err == nil {
		t.Fatal("missing --client-id must be refused")
	}
	// Asserts the guard's own message, not merely "some error": readCSRFile("")
	// also errors (os.Open("") fails), so a test that only checked err != nil
	// would keep passing even with the *csrPath == "" guard removed, and would
	// then be reporting a downstream file-open failure as if it were the
	// intended refusal.
	if _, err := parseEnrolSignFlags([]string{"--client-id", "hermes-mail"}); err == nil || !strings.Contains(err.Error(), "--csr is required") {
		t.Fatalf("missing --csr: err = %v, want the \"--csr is required\" refusal", err)
	}
}

// A CSR file over maxCSRBytes is refused locally, with the same message
// ParseClientCSR would give — before any store or dial.
func TestParseEnrolSignFlags_RefusesOverLengthCSRLocally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.csr")
	assertNoErr(t, os.WriteFile(path, make([]byte, maxCSRBytes+1), 0644), "write oversized CSR file")

	_, err := parseEnrolSignFlags([]string{"--client-id", "x", "--csr", path})
	if err == nil || !strings.Contains(err.Error(), "a few hundred bytes") {
		t.Fatalf("err = %v, want the over-length refusal", err)
	}
}

// AC-7: enrolSign dispatches through a real admin_op round trip and the
// enrolment lands in the store.
func TestEnrolSign_CLIDispatchesTheParsedRequestThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	csrPath := writeTestCSRFile(t, "hermes-mail")
	serveBroker(t, newBrokerRouter(t, store, nil))

	enrolSign(store, []string{"--client-id", "hermes-mail", "--csr", csrPath, "--grant", profile.ID})

	stored := store.Get().FindEnrolment("hermes-mail")
	if stored == nil {
		t.Fatal("enrolment did not land in the store")
	}
	if !stored.GrantsProject(profile.ID) {
		t.Fatalf("stored enrolment does not grant %s: %+v", profile.ID, stored)
	}
	if stored.SPKISHA256 == "" {
		t.Fatal("a CSR-signed enrolment must record its SPKI hash")
	}
}

// AC-8: --out DIR writes client.crt and ca.crt into DIR, byte-identical to
// the config-dir copy.
func TestEnrolSign_OutDirWritesByteIdenticalCopies(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	csrPath := writeTestCSRFile(t, "hermes-mail")
	serveBroker(t, newBrokerRouter(t, store, nil))

	outDir := t.TempDir()
	enrolSign(store, []string{"--client-id", "hermes-mail", "--csr", csrPath, "--grant", profile.ID, "--out", outDir})

	stored := store.Get().FindEnrolment("hermes-mail")
	if stored == nil {
		t.Fatal("enrolment did not land in the store")
	}
	bundleDir := filepath.Join(dir, enrolmentBundleDir, "hermes-mail")

	for _, name := range []string{"client.crt", "ca.crt"} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		assertNoErr(t, err, "read --out copy of %s", name)
		want, err := os.ReadFile(filepath.Join(bundleDir, name))
		assertNoErr(t, err, "read config-dir copy of %s", name)
		if string(got) != string(want) {
			t.Fatalf("%s: --out copy differs from the config-dir copy", name)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "client.key")); err == nil {
		t.Fatal("--out must never write a client.key")
	}
}

// Regression: on the bundle-error path, CertPEM/CAPEM come back empty, so
// --out must not write anything, and nothing printed before that point may
// claim a certificate path — a stale client.crt in --out DIR from an
// earlier, successful sign must survive a later, failed one untouched.
func TestEnrolSign_BundleErrorLeavesOutDirUntouchedAndReportsFailureFirst(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	csrPath := writeTestCSRFile(t, "hermes-mail")
	serveBroker(t, newBrokerRouter(t, store, nil))

	// Force signEnrolment down the bundle-error path: writeSignedCertBundle
	// refuses when client.key already exists in the config-dir bundle
	// location, so seed one there before signing.
	bundleDir := filepath.Join(dir, enrolmentBundleDir, "hermes-mail")
	assertNoErr(t, os.MkdirAll(bundleDir, 0700), "mkdir bundle dir")
	assertNoErr(t, os.WriteFile(filepath.Join(bundleDir, "client.key"), []byte("stale key"), 0600), "seed stale client.key")

	// outDir already holds a working client.crt/ca.crt from an earlier,
	// successful sign.
	outDir := t.TempDir()
	existingCert := []byte("earlier working client.crt")
	existingCA := []byte("earlier working ca.crt")
	assertNoErr(t, os.WriteFile(filepath.Join(outDir, "client.crt"), existingCert, 0644), "seed existing client.crt")
	assertNoErr(t, os.WriteFile(filepath.Join(outDir, "ca.crt"), existingCA, 0644), "seed existing ca.crt")

	out := captureStdout(t, func() {
		enrolSign(store, []string{"--client-id", "hermes-mail", "--csr", csrPath, "--grant", profile.ID, "--out", outDir})
	})

	gotCert, err := os.ReadFile(filepath.Join(outDir, "client.crt"))
	assertNoErr(t, err, "read client.crt after bundle-error sign")
	if string(gotCert) != string(existingCert) {
		t.Fatalf("client.crt in --out DIR was overwritten on the bundle-error path: got %q, want unchanged %q", gotCert, existingCert)
	}
	gotCA, err := os.ReadFile(filepath.Join(outDir, "ca.crt"))
	assertNoErr(t, err, "read ca.crt after bundle-error sign")
	if string(gotCA) != string(existingCA) {
		t.Fatalf("ca.crt in --out DIR was overwritten on the bundle-error path: got %q, want unchanged %q", gotCA, existingCA)
	}

	if strings.Contains(out, "copies also written to") {
		t.Fatalf("output claims a write happened on the bundle-error path: %s", out)
	}
	if strings.Contains(out, "certificate:") {
		t.Fatalf("output names a certificate path for a directory with no certificate: %s", out)
	}
	if !strings.Contains(out, "bundle to disk failed") {
		t.Fatalf("output does not name the bundle failure: %s", out)
	}
}
