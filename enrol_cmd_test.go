package main

// create, update and revoke are brokered (ADR-017 decision 2): `enrolUpdate`
// no longer calls updateEnrolment directly, so its flag-parsing is tested on
// its own, against parseEnrolUpdateFlags, with no store and no service dial
// involved at all. The end-to-end path (a real bridge server, a wired
// EnrolmentOps, the actual admin_op round trip) is covered separately below
// and in audit_issuance_test.go.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"relaygo/presence"
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

// AC-3 (spec §3), CLI half: requests, approve, refuse.

func TestParseEnrolApproveFlags_RequiresIDAndClientID(t *testing.T) {
	got := parseEnrolApproveFlags([]string{"--id", "req_123", "--client-id", "hermes-mail", "--grant", "proj-a"})
	if got.RequestID != "req_123" || got.ClientID != "hermes-mail" {
		t.Fatalf("got = %+v", got)
	}
	if !slices.Equal(got.ProjectIDs, []string{"proj-a"}) {
		t.Fatalf("ProjectIDs = %v, want [proj-a]", got.ProjectIDs)
	}
}

// §11.5: over SSH, approve must name the tray as the working door, not the
// generic "there is no queue and no pending-approval list" line — that
// sentence is false for this one verb, since a request DOES sit in a queue.
func TestEnrolApproveErrorText_NamesTheTrayNotTheGenericQueueMessage(t *testing.T) {
	wireErr := errors.New("bridge error (code -32603): " + presence.ErrNoSession.Error())
	got := enrolApproveErrorText(wireErr)
	if !strings.Contains(got, "Pending requests") {
		t.Fatalf("enrolApproveErrorText did not name the tray's pending-requests panel: %q", got)
	}
	if strings.Contains(got, "no pending-approval list") {
		t.Fatalf("enrolApproveErrorText kept the generic (now-false for this verb) line: %q", got)
	}
}

func TestEnrolApproveErrorText_PassesThroughEverythingElse(t *testing.T) {
	wireErr := errors.New(`bridge error (code -32602): request id is required`)
	got := enrolApproveErrorText(wireErr)
	if got != wireErr.Error() {
		t.Fatalf("got = %q, want unchanged %q", got, wireErr.Error())
	}
}

// End-to-end: requests, approve and refuse each dispatch through a real
// admin_op round trip into EnrolmentOps.
func TestEnrolRequestsApproveRefuse_CLIDispatchThroughTheBroker(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	table := newEnrolmentRequestTable()
	csrPEM := genClientCSRPEM(t, "hermes-mail")
	l, err := table.Lodge(csrPEM, "vm-a", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge")

	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.enrolmentOps.Requests = table
	}))

	out := captureStdout(t, func() {
		enrolRequests([]string{})
	})
	if !strings.Contains(out, l.RequestID) || !strings.Contains(out, "vm-a") {
		t.Fatalf("enrol requests output = %q, want it to name the pending request and its label", out)
	}

	out = captureStdout(t, func() {
		enrolApprove([]string{"--id", l.RequestID, "--client-id", "hermes-mail", "--grant", profile.ID})
	})
	if !strings.Contains(out, "hermes-mail") {
		t.Fatalf("enrol approve output = %q, want it to name the client id", out)
	}
	stored := store.Get().FindEnrolment("hermes-mail")
	if stored == nil {
		t.Fatal("approval did not land in the store")
	}
	if !stored.GrantsProject(profile.ID) {
		t.Fatalf("approved enrolment does not grant %s: %+v", profile.ID, stored)
	}

	poll, perr := table.Poll(l.RequestID)
	assertNoErr(t, perr, "Poll")
	if poll.Status != "approved" {
		t.Fatalf("poll status = %q, want approved", poll.Status)
	}

	// A second, distinct request is refused instead of approved.
	l2, err := table.Lodge(genClientCSRPEM(t, "hermes-refuse"), "", "10.0.0.6:1")
	assertNoErr(t, err, "Lodge second")
	out = captureStdout(t, func() {
		enrolRefuse([]string{"--id", l2.RequestID})
	})
	if !strings.Contains(out, l2.RequestID) {
		t.Fatalf("enrol refuse output = %q, want it to name the request id", out)
	}
	if _, ok := table.Get(l2.RequestID); ok {
		t.Fatal("refuse must remove the pending record")
	}
}

// Finding 5, the CLI-visible half: if the pending row is swept while the
// presence prompt is open (a request lodged ~14 minutes ago, answered two
// minutes later, crossing the 15-minute TTL), the enrolment still commits
// -- but `relay enrol approve`'s own output must say so plainly, naming the
// expiry and the recovery paths, and must NOT claim the client will collect
// the certificate on its next poll: there is no row left to poll.
func TestEnrolApprove_RowSweptDuringPresencePromptSaysSoAndDoesNotClaimDelivery(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	table := newEnrolmentRequestTable()
	now := time.Now()
	table.setClock(func() time.Time { return now })
	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "vm-a", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge")

	// 14 minutes pass before the operator runs `relay enrol approve`.
	now = now.Add(14 * time.Minute)
	sink := &sweptDuringApprovalSink{enrolmentRequestTable: table, now: &now}

	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.enrolmentOps.Requests = sink
	}))

	out := captureStdout(t, func() {
		enrolApprove([]string{"--id", l.RequestID, "--client-id", "hermes-mail", "--grant", profile.ID})
	})

	if !strings.Contains(out, "hermes-mail") {
		t.Fatalf("enrol approve output = %q, want it to name the client id", out)
	}
	if strings.Contains(out, "delivered to the client on its next poll") {
		t.Fatalf("enrol approve output = %q, must NOT claim the client will collect it -- the row is gone", out)
	}
	if !strings.Contains(out, "expired") {
		t.Fatalf("enrol approve output = %q, want it to name the expiry plainly", out)
	}
	if !strings.Contains(out, "relay enrol list") {
		t.Fatalf("enrol approve output = %q, want it to point at `relay enrol list` for the real, recorded enrolment", out)
	}

	stored := store.Get().FindEnrolment("hermes-mail")
	if stored == nil {
		t.Fatal("the enrolment must be real and recorded even though the pending row expired")
	}
	if !stored.GrantsProject(profile.ID) {
		t.Fatalf("approved enrolment does not grant %s: %+v", profile.ID, stored)
	}
}

// Issue #93, the CLI-visible half: if the pending row is instead found
// REFUSED when MarkApproved runs -- the operator declined this exact
// request from the pending list while THIS approval's own presence prompt
// was still open -- the enrolment still commits, but `relay enrol
// approve`'s own output must name the refusal plainly (never call it an
// expiry) and point at revoke by the client id that was just signed.
func TestEnrolApprove_RowRefusedDuringPresencePromptNamesTheRefusalNotAnExpiry(t *testing.T) {
	store := newCLISandboxStore(t)
	profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	table := newEnrolmentRequestTable()
	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "vm-a", "10.0.0.5:41233")
	assertNoErr(t, err, "Lodge")

	sink := &refusedDuringApprovalSink{enrolmentRequestTable: table, requestID: l.RequestID}

	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.enrolmentOps.Requests = sink
	}))

	out := captureStdout(t, func() {
		enrolApprove([]string{"--id", l.RequestID, "--client-id", "hermes-mail", "--grant", profile.ID})
	})

	if !strings.Contains(out, "hermes-mail") {
		t.Fatalf("enrol approve output = %q, want it to name the client id", out)
	}
	if strings.Contains(out, "expired") || strings.Contains(out, "TTL") {
		t.Fatalf("enrol approve output = %q, must NOT call this an expiry -- the row was refused, not swept", out)
	}
	if !strings.Contains(out, "you refused this exact request") {
		t.Fatalf("enrol approve output = %q, want it to name the operator's own refusal plainly", out)
	}
	if !strings.Contains(out, "relay enrol revoke --client-id hermes-mail") {
		t.Fatalf("enrol approve output = %q, want it to point at revoke by client id", out)
	}

	stored := store.Get().FindEnrolment("hermes-mail")
	if stored == nil {
		t.Fatal("the enrolment must be real and recorded even though the pending row was refused mid-approval")
	}
	if !stored.GrantsProject(profile.ID) {
		t.Fatalf("approved enrolment does not grant %s: %+v", profile.ID, stored)
	}

	poll, perr := table.Poll(l.RequestID)
	assertNoErr(t, perr, "Poll")
	if poll.Status != "refused" {
		t.Fatalf("poll status = %q, want refused -- the row still exists and was decided, not expired", poll.Status)
	}
}

func TestEnrolRequests_JSONFlagPrintsMachineReadableOutput(t *testing.T) {
	store := newCLISandboxStore(t)
	table := newEnrolmentRequestTable()
	l, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "10.0.0.5:1")
	assertNoErr(t, err, "Lodge")
	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.enrolmentOps.Requests = table
	}))

	out := captureStdout(t, func() {
		enrolRequests([]string{"--json"})
	})
	var items []enrolmentRequestListItem
	assertNoErr(t, json.Unmarshal([]byte(out), &items), "parse --json output")
	if len(items) != 1 || items[0].RequestID != l.RequestID {
		t.Fatalf("items = %+v, want exactly the one lodged request", items)
	}
}

// AC-30's CLI half: `relay enrol ca-fingerprint` needs no broker at all.
func TestEnrolCAFingerprint_ReadsDirectlyWithNoBrokerNeeded(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	_, err := LoadOrCreateCA(testSealer())
	assertNoErr(t, err, "LoadOrCreateCA")

	out := captureStdout(t, func() {
		enrolCAFingerprint()
	})
	if !strings.HasPrefix(strings.TrimSpace(out), "sha256:") {
		t.Fatalf("enrol ca-fingerprint output = %q, want a sha256: fingerprint", out)
	}
}
