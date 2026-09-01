package main

// The comparison-code half of the enrolment-request channel (spec §2, §3):
// the commitment lodged, relay's nonce minted after it, the open verified
// exactly once, and the host-side refusal that makes the comparison
// mandatory regardless of what the client claims it did.
//
// A row lodged WITHOUT a commitment is a `relayremote request` and must go
// on behaving exactly as it did before any of this existed — that is the
// property half these tests are here to hold down.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// seedCAInto gives a table the CA material remote_reconcile.go's tick would
// push into it. The sandbox must already exist.
func seedCAInto(t *testing.T, table *enrolmentRequestTable) (certPEM, spki []byte) {
	t.Helper()
	if _, err := LoadOrCreateCA(testSealer()); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	certPEM, spki, err := caMaterialFromDisk()
	assertNoErr(t, err, "caMaterialFromDisk")
	table.setCACert(certPEM, spki)
	return certPEM, spki
}

// sasCATable is a sandboxed table holding a real CA, for tests that need no
// settings store.
func sasCATable(t *testing.T) (*enrolmentRequestTable, []byte, []byte) {
	t.Helper()
	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()
	certPEM, spki := seedCAInto(t, table)
	return table, certPEM, spki
}

// sasClient is what `relayremote register` holds before it lodges: a key, a
// CSR over it, its own nonce, and the commitment binding the two.
type sasClient struct {
	csrPEM []byte
	spki   []byte
	rc     []byte
	commit string
}

func newSASClient(t *testing.T, cn string) *sasClient {
	t.Helper()
	csrPEM := genClientCSRPEM(t, cn)
	csr := parseCSRForTest(t, csrPEM)
	rc := make([]byte, sasNonceBytes)
	if _, err := rand.Read(rc); err != nil {
		t.Fatalf("read client nonce: %v", err)
	}
	return &sasClient{
		csrPEM: csrPEM,
		spki:   csr.RawSubjectPublicKeyInfo,
		rc:     rc,
		commit: sasCommitment(sha256.Sum256(csr.RawSubjectPublicKeyInfo), rc),
	}
}

func (c *sasClient) open() string { return hex.EncodeToString(c.rc) }

// lodgeRegister lodges c as a `register` would and returns the ack.
func (c *sasClient) lodgeRegister(t *testing.T, table *enrolmentRequestTable, remoteAddr string) lodged {
	t.Helper()
	l, err := table.Lodge(c.csrPEM, "", "", c.commit, remoteAddr)
	assertNoErr(t, err, "Lodge with a commitment")
	return l
}

// viewFor finds one row in the table's projection.
func viewFor(t *testing.T, table *enrolmentRequestTable, requestID string) enrolmentRequestView {
	t.Helper()
	for _, v := range table.List() {
		if v.RequestID == requestID {
			return v
		}
	}
	t.Fatalf("request %s is not in the table's projection", requestID)
	return enrolmentRequestView{}
}

// otherNonce returns a valid 32-hex opening that is not c's.
func otherNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, sasNonceBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("read nonce: %v", err)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// AC-8: the nonce is relay's, minted inside Lodge, and written exactly once
// ---------------------------------------------------------------------------

func TestSAS_AC8_RelayNonceIsMintedInLodgeAndAssignedOnce(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this file's location")
	}
	path := filepath.Join(filepath.Dir(file), "enrolment_requests.go")

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	assertNoErr(t, err, "parse enrolment_requests.go")

	var literalKeys, selectorAssignments int
	ast.Inspect(parsed, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := node.Key.(*ast.Ident); ok && id.Name == "sasNonce" {
				literalKeys++
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "sasNonce" {
					selectorAssignments++
				}
			}
		}
		return true
	})
	if literalKeys != 1 {
		t.Fatalf("sasNonce is set in %d composite literals, want exactly 1 (the record Lodge builds)", literalKeys)
	}
	if selectorAssignments != 0 {
		t.Fatalf("sasNonce is assigned through %d selector(s) — the nonce must not be settable after the row exists, "+
			"or an attacker that learns the client's opening could have relay re-choose the value it displays", selectorAssignments)
	}

	table, _, _ := sasCATable(t)
	a := newSASClient(t, "ac8-a").lodgeRegister(t, table, "10.0.0.1:1")
	b := newSASClient(t, "ac8-b").lodgeRegister(t, table, "10.0.0.2:1")
	if !validSASHex(a.SASNonce, sasNonceBytes) || !validSASHex(b.SASNonce, sasNonceBytes) {
		t.Fatalf("nonces are not 32 lowercase hex: %q, %q", a.SASNonce, b.SASNonce)
	}
	if a.SASNonce == b.SASNonce {
		t.Fatalf("two lodges share the nonce %q", a.SASNonce)
	}
	if a.CAPEM == "" || b.CAPEM == "" {
		t.Fatal("a commitment-bearing lodge was answered without the CA certificate")
	}
}

// A lodge that carries a commitment when relay has no CA is refused, never
// answered with an empty ca_pem — and a legacy lodge is unaffected.
func TestSAS_LodgeWithCommitmentRefusedWhenThereIsNoCA(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	table := newEnrolmentRequestTable()

	c := newSASClient(t, "no-ca")
	if _, err := table.Lodge(c.csrPEM, "", "", c.commit, "10.0.0.1:1"); !errors.Is(err, errEnrolmentNoCA) {
		t.Fatalf("err = %v, want errEnrolmentNoCA", err)
	}
	if !strings.Contains(errEnrolmentNoCA.Error(), "relay enrol create") {
		t.Fatalf("the refusal does not name the one-time fix: %v", errEnrolmentNoCA)
	}
	if n := len(table.List()); n != 0 {
		t.Fatalf("a refused lodge left %d row(s) behind", n)
	}

	// The carried-pin path needs no CA at lodge time and still works.
	if _, err := table.Lodge(genClientCSRPEM(t, "legacy-no-ca"), "", "", "", "10.0.0.2:1"); err != nil {
		t.Fatalf("a lodge without a commitment was refused for a missing CA: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC-9: opened once, correctly, and never twice differently
// ---------------------------------------------------------------------------

func TestSAS_AC9_CommitmentOpensOnceCorrectlyAndNeverTwiceDifferently(t *testing.T) {
	t.Run("correct open, then the identical one again", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "ac9-good")
		l := c.lodgeRegister(t, table, "10.0.0.1:1")

		if v := viewFor(t, table, l.RequestID); v.SASReady || v.SAS != "" {
			t.Fatalf("row is ready before any open: %+v", v)
		}

		res, err := table.Poll(l.RequestID, c.open())
		assertNoErr(t, err, "poll with the correct open")
		if res.Status != "pending" {
			t.Fatalf("status = %q, want pending", res.Status)
		}
		v := viewFor(t, table, l.RequestID)
		if !v.SASReady || len(v.SAS) != 6 {
			t.Fatalf("row is not ready after a correct open: %+v", v)
		}

		// A redialled poll resends the same opening and must not fail.
		if _, err := table.Poll(l.RequestID, c.open()); err != nil {
			t.Fatalf("an identical re-open was refused: %v", err)
		}
		if got := viewFor(t, table, l.RequestID); got.SAS != v.SAS {
			t.Fatalf("the code moved on a redialled poll: %q then %q", v.SAS, got.SAS)
		}
	})

	t.Run("a different open is refused, not applied", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "ac9-second")
		l := c.lodgeRegister(t, table, "10.0.0.1:1")
		_, err := table.Poll(l.RequestID, c.open())
		assertNoErr(t, err, "poll with the correct open")
		before := viewFor(t, table, l.RequestID)

		if _, err := table.Poll(l.RequestID, otherNonce(t)); !errors.Is(err, errEnrolmentSASRefused) {
			t.Fatalf("err = %v, want errEnrolmentSASRefused", err)
		}
		after := viewFor(t, table, l.RequestID)
		if after.SAS != before.SAS || after.SASFailed || !after.SASReady {
			t.Fatalf("a refused second opening changed the row: %+v then %+v", before, after)
		}
	})

	t.Run("an incorrect open fails the row permanently", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "ac9-bad")
		l := c.lodgeRegister(t, table, "10.0.0.1:1")

		if _, err := table.Poll(l.RequestID, otherNonce(t)); !errors.Is(err, errEnrolmentSASRefused) {
			t.Fatalf("err = %v, want errEnrolmentSASRefused", err)
		}
		v := viewFor(t, table, l.RequestID)
		if !v.SASFailed || v.SASReady || v.SAS != "" {
			t.Fatalf("a failed open did not mark the row: %+v", v)
		}

		// One row buys one blind guess. The correct opening, offered
		// second, must not rescue it.
		if _, err := table.Poll(l.RequestID, c.open()); !errors.Is(err, errEnrolmentSASRefused) {
			t.Fatalf("a failed row accepted a later opening: err = %v", err)
		}
		if v := viewFor(t, table, l.RequestID); !v.SASFailed || v.SASReady {
			t.Fatalf("a failed row recovered: %+v", v)
		}
	})

	t.Run("the commitment binds the key", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		// A commitment captured off the wire, replayed under a different
		// CSR: the row it is lodged against holds another key, so the
		// opening cannot verify.
		captured := newSASClient(t, "ac9-victim")
		attacker := newSASClient(t, "ac9-attacker")
		l, err := table.Lodge(attacker.csrPEM, "", "", captured.commit, "10.0.0.1:1")
		assertNoErr(t, err, "lodge the replayed commitment")

		if _, err := table.Poll(l.RequestID, captured.open()); !errors.Is(err, errEnrolmentSASRefused) {
			t.Fatalf("a commitment over another key opened: err = %v", err)
		}
	})

	t.Run("an open against a legacy row is refused", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		l, err := table.Lodge(genClientCSRPEM(t, "ac9-legacy"), "", "", "", "10.0.0.1:1")
		assertNoErr(t, err, "legacy lodge")
		_, err = table.Poll(l.RequestID, otherNonce(t))
		if !errors.Is(err, errEnrolmentSASRefused) || !strings.Contains(err.Error(), "not lodged with a comparison commitment") {
			t.Fatalf("err = %v, want a refusal naming the missing commitment", err)
		}
	})
}

// The two screens show the same six characters on the honest path, and the
// host derives its own from the stored CSR and the CA on disk.
func TestSAS_HostAndClientDeriveTheSameCode(t *testing.T) {
	table, certPEM, caSPKI := sasCATable(t)
	c := newSASClient(t, "agree")
	l := c.lodgeRegister(t, table, "10.0.0.1:1")
	if l.CAPEM != string(certPEM) {
		t.Fatal("the ack did not carry the CA certificate verbatim")
	}
	_, err := table.Poll(l.RequestID, c.open())
	assertNoErr(t, err, "poll with the open")

	rr, err := hex.DecodeString(l.SASNonce)
	assertNoErr(t, err, "decode the relay nonce")
	client := computeSAS(caSPKI, c.spki, c.rc, rr)

	if host := viewFor(t, table, l.RequestID).SAS; host != client {
		t.Fatalf("host shows %q, client computes %q", host, client)
	}
}

// ---------------------------------------------------------------------------
// §2.6: the idempotent re-lodge rules
// ---------------------------------------------------------------------------

func TestSAS_IdempotentRelodgeRules(t *testing.T) {
	t.Run("same commitment resumes with the same code", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "relodge-same")
		first := c.lodgeRegister(t, table, "10.0.0.1:1")
		second := c.lodgeRegister(t, table, "10.0.0.1:1")

		if second.RequestID != first.RequestID {
			t.Fatalf("resume took a second slot: %s then %s", first.RequestID, second.RequestID)
		}
		if second.SASNonce != first.SASNonce || second.CAPEM != first.CAPEM {
			t.Fatal("resume answered with different comparison material, which would print a second code")
		}
		if n := len(table.List()); n != 1 {
			t.Fatalf("resume created %d rows", n)
		}
	})

	t.Run("a second commitment on one key is refused", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "relodge-diff")
		c.lodgeRegister(t, table, "10.0.0.1:1")

		other := newSASClient(t, "relodge-diff-other")
		_, err := table.Lodge(c.csrPEM, "", "", other.commit, "10.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "different comparison commitment") {
			t.Fatalf("err = %v, want a refusal naming the different commitment", err)
		}
	})

	t.Run("a legacy row is never upgraded in place", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		csrPEM := genClientCSRPEM(t, "relodge-legacy")
		_, err := table.Lodge(csrPEM, "", "", "", "10.0.0.1:1")
		assertNoErr(t, err, "legacy lodge")

		csr := parseCSRForTest(t, csrPEM)
		rc := make([]byte, sasNonceBytes)
		if _, err := rand.Read(rc); err != nil {
			t.Fatalf("read nonce: %v", err)
		}
		commit := sasCommitment(sha256.Sum256(csr.RawSubjectPublicKeyInfo), rc)
		_, err = table.Lodge(csrPEM, "", "", commit, "10.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "without a comparison commitment") {
			t.Fatalf("err = %v, want a refusal naming the mismatch", err)
		}
	})

	t.Run("a commitment row is not downgraded either", func(t *testing.T) {
		table, _, _ := sasCATable(t)
		c := newSASClient(t, "relodge-down")
		c.lodgeRegister(t, table, "10.0.0.1:1")
		_, err := table.Lodge(c.csrPEM, "", "", "", "10.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "with a comparison commitment") {
			t.Fatalf("err = %v, want a refusal naming the mismatch", err)
		}
	})
}

// The generation counter is the tray's whole pull mechanism: only an actual
// insert may move it.
func TestSAS_LodgeGenerationMovesOnlyOnAnInsert(t *testing.T) {
	table, _, _ := sasCATable(t)
	if got := table.LodgeGeneration(); got != 0 {
		t.Fatalf("a fresh table starts at generation %d", got)
	}

	c := newSASClient(t, "gen")
	l := c.lodgeRegister(t, table, "10.0.0.1:1")
	afterInsert := table.LodgeGeneration()
	if afterInsert != 1 {
		t.Fatalf("generation = %d after one insert, want 1", afterInsert)
	}

	c.lodgeRegister(t, table, "10.0.0.1:1") // idempotent re-lodge
	if _, err := table.Poll(l.RequestID, c.open()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if _, err := table.Lodge([]byte("not a csr"), "", "", "", "10.0.0.1:1"); err == nil {
		t.Fatal("a malformed CSR was accepted")
	}
	if got := table.LodgeGeneration(); got != afterInsert {
		t.Fatalf("generation moved to %d on a re-lodge, a poll or a refusal", got)
	}
}

// ---------------------------------------------------------------------------
// AC-10 / AC-11: the host enforces the comparison, from every door
// ---------------------------------------------------------------------------

// sasApproveCase drives one door with a row whose comparison is incomplete.
type sasApproveCase struct {
	name string
	// prepare lodges a row and leaves it in the unapprovable state.
	prepare func(t *testing.T, table *enrolmentRequestTable) string
}

func sasIncompleteCases() []sasApproveCase {
	return []sasApproveCase{
		{
			name: "never opened",
			prepare: func(t *testing.T, table *enrolmentRequestTable) string {
				t.Helper()
				return newSASClient(t, "unopened").lodgeRegister(t, table, "10.0.0.5:1").RequestID
			},
		},
		{
			name: "opened wrongly",
			prepare: func(t *testing.T, table *enrolmentRequestTable) string {
				t.Helper()
				c := newSASClient(t, "failed")
				l := c.lodgeRegister(t, table, "10.0.0.5:1")
				if _, err := table.Poll(l.RequestID, otherNonce(t)); err == nil {
					t.Fatal("a wrong opening was accepted")
				}
				return l.RequestID
			},
		},
	}
}

// AC-10 and AC-11, core door: refused with errEnrolmentRequestSASIncomplete
// and BEFORE the presence gate — an operator is never asked for Touch ID
// for an act that is going to be refused.
func TestSAS_AC10_AC11_IncompleteComparisonRefusedBeforeTheGate(t *testing.T) {
	for _, tc := range sasIncompleteCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, store := newEnrolmentSandbox(t)
			profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
			table := newEnrolmentRequestTable()
			seedCAInto(t, table)
			requestID := tc.prepare(t, table)

			recording := presencetest.NewRecording(nil)
			gate, err := presence.NewGate(recording)
			assertNoErr(t, err, "NewGate")

			ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t), Requests: table}
			_, err = ops.Approve(context.Background(), approveFields{
				RequestID: requestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
			}, auditViaCLI, "")
			if !errors.Is(err, errEnrolmentRequestSASIncomplete) {
				t.Fatalf("err = %v, want errEnrolmentRequestSASIncomplete", err)
			}
			if n := recording.Calls(); n != 0 {
				t.Fatalf("presence provider called %d time(s); the refusal must come before the gate", n)
			}
			if len(store.Get().Enrolments) != 0 {
				t.Fatalf("an unapprovable request produced an enrolment: %+v", store.Get().Enrolments)
			}
		})
	}
}

// The same refusal through the Settings-window door.
func TestSAS_AC10_AC11_IncompleteComparisonRefusedOverIPC(t *testing.T) {
	for _, tc := range sasIncompleteCases() {
		t.Run(tc.name, func(t *testing.T) {
			ipc, store, ui, table := newEnrolmentRequestsIPC(t)
			seedCAInto(t, table)
			mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
			requestID := tc.prepare(t, table)

			ipcApproveEnrolmentRequest(ipc, mustRaw(t, map[string]interface{}{
				"request_id":  requestID,
				"client_id":   "hermes-mail",
				"project_ids": []string{mail.ID},
			}))

			if _, ok := findEvent(ui, "onEnrolmentCreated"); ok {
				t.Fatal("the Settings door approved a request with no completed comparison")
			}
			if _, ok := findEvent(ui, "onEnrolmentError"); !ok {
				t.Fatalf("expected onEnrolmentError; got %+v", ui.events)
			}
			if len(store.Get().Enrolments) != 0 {
				t.Fatalf("an unapprovable request produced an enrolment: %+v", store.Get().Enrolments)
			}
		})
	}
}

// And through the CLI door: `relay enrol approve` reaches the same core
// over the admin-op broker, and the refusal is what comes back over it.
// The handler is driven directly rather than through enrolApprove, whose
// refusal path is exitError's os.Exit.
func TestSAS_AC10_AC11_IncompleteComparisonRefusedFromTheCLIDoor(t *testing.T) {
	for _, tc := range sasIncompleteCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := newCLISandboxStore(t)
			profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
			table := newEnrolmentRequestTable()
			seedCAInto(t, table)
			requestID := tc.prepare(t, table)

			recording := presencetest.NewRecording(nil)
			gate, err := presence.NewGate(recording)
			assertNoErr(t, err, "NewGate")

			router := newBrokerRouter(t, store, func(r *appRouter) {
				r.enrolmentOps.Requests = table
				r.enrolmentOps.Gate = gate
			})
			args, err := json.Marshal(approveFields{
				RequestID: requestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
			})
			assertNoErr(t, err, "marshal approveFields")

			_, err = adminEnrolmentRequestApprove(context.Background(), router, args)
			if !errors.Is(err, errEnrolmentRequestSASIncomplete) {
				t.Fatalf("err = %v, want errEnrolmentRequestSASIncomplete", err)
			}
			if !strings.Contains(err.Error(), "comparison handshake") {
				t.Fatalf("the operator-facing message does not name what happened: %v", err)
			}
			if n := recording.Calls(); n != 0 {
				t.Fatalf("presence provider called %d time(s) from the CLI door", n)
			}
			if len(store.Get().Enrolments) != 0 {
				t.Fatalf("an unapprovable request produced an enrolment: %+v", store.Get().Enrolments)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC-12: a legacy `relayremote request` row is still approvable
// ---------------------------------------------------------------------------

func TestSAS_AC12_LegacyRequestRowIsStillApprovable(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)

	l, err := table.Lodge(genClientCSRPEM(t, "hermes-carried"), "hermes-carried", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "legacy lodge")
	if l.CAPEM != "" || l.SASNonce != "" {
		t.Fatalf("a legacy lodge was answered with comparison material: %+v", l)
	}

	v := viewFor(t, table, l.RequestID)
	if !v.IsLegacyRequest || v.SASReady || v.SASFailed || v.SAS != "" {
		t.Fatalf("legacy row projects comparison state: %+v", v)
	}

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	created, err := ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-carried", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	assertNoErr(t, err, "Approve a legacy row")
	if created.Enrolment.ClientID != "hermes-carried" || !created.Enrolment.GrantsProject(profile.ID) {
		t.Fatalf("approved enrolment is wrong: %+v", created.Enrolment)
	}
}

// ---------------------------------------------------------------------------
// AC-14: relay never echoes the code it displays
// ---------------------------------------------------------------------------

func TestSAS_AC14_NoResultTypeCarriesTheComparisonCode(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(enrolmentRequestLodgeResult{}),
		reflect.TypeOf(enrolmentRequestPollResult{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			name := strings.ToLower(f.Name)
			if !strings.Contains(name, "sas") && !strings.Contains(tag, "sas") {
				continue
			}
			if f.Name != "SASNonce" || tag != "sas_nonce" {
				t.Fatalf("%s.%s (json %q) carries comparison state to the client; only sas_nonce may, and it is the "+
					"nonce, not the code", typ.Name(), f.Name, tag)
			}
		}
	}

	// And behaviourally: a fully populated pair of results, marshalled,
	// contains the row's six characters nowhere.
	_, store := newEnrolmentSandbox(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	table := newEnrolmentRequestTable()
	seedCAInto(t, table)

	c := newSASClient(t, "ac14")
	l := c.lodgeRegister(t, table, "10.0.0.5:1")
	_, err := table.Poll(l.RequestID, c.open())
	assertNoErr(t, err, "poll with the open")

	ops := &EnrolmentOps{Store: store, Gate: allowGate(t), Issuance: pgwWithIssuance(t), Requests: table}
	_, err = ops.Approve(context.Background(), approveFields{
		RequestID: l.RequestID, ClientID: "hermes-mail", ProjectIDs: []string{profile.ID},
	}, auditViaCLI, "")
	assertNoErr(t, err, "Approve")

	code := viewFor(t, table, l.RequestID).SAS
	if len(code) != 6 {
		t.Fatalf("the row has no code to look for: %q", code)
	}

	lodgeJSONBytes, err := json.Marshal(enrolmentRequestLodgeResult{
		RequestID: l.RequestID, SPKISHA256: l.SPKISHA256,
		PollAfterSeconds: l.PollAfterSeconds, ExpiresInSeconds: l.ExpiresInSeconds,
		CAPEM: l.CAPEM, SASNonce: l.SASNonce,
	})
	assertNoErr(t, err, "marshal the lodge result")

	res, err := table.Poll(l.RequestID, "")
	assertNoErr(t, err, "poll after approval")
	pollJSONBytes, err := json.Marshal(enrolmentRequestPollResult{
		Status: res.Status, PollAfterSeconds: res.PollAfterSeconds, ExpiresInSeconds: res.ExpiresInSeconds,
		ClientID: res.ClientID, ProjectIDs: res.ProjectIDs, RelayAddr: res.RelayAddr,
		CertPEM: res.CertPEM, CAPEM: res.CAPEM,
	})
	assertNoErr(t, err, "marshal the poll result")

	for name, data := range map[string][]byte{"lodge": lodgeJSONBytes, "poll": pollJSONBytes} {
		if strings.Contains(string(data), code) {
			t.Fatalf("the %s result carries the comparison code %q; a man-in-the-middle would forward relay's own "+
				"value and the comparison would compare one number with itself", name, code)
		}
	}
}

// ---------------------------------------------------------------------------
// AC-27: an old client against a new relay
// ---------------------------------------------------------------------------

func TestSAS_AC27_OldClientLodgeAndPollUnchangedOverTheWire(t *testing.T) {
	f := newEnrolFixture(t, enrolFixtureOpts{})
	seedCAIntoSandboxless(t, f.table)

	csrPEM := genClientCSRPEM(t, "old-client")
	c := f.dialClient()

	resp := c.roundTrip(lodgeJSON(csrPEM, "old-client"))
	if resp.Type != bridge.RespResult {
		t.Fatalf("lodge = %s %q", resp.Type, resp.Message)
	}
	var first map[string]any
	assertNoErr(t, json.Unmarshal(resp.Result, &first), "decode the lodge result")
	for _, absent := range []string{"ca_pem", "sas_nonce"} {
		if _, ok := first[absent]; ok {
			t.Fatalf("a lodge with no commitment was answered with %q: %v", absent, first)
		}
	}
	for _, present := range []string{"request_id", "spki_sha256", "poll_after_seconds", "expires_in_seconds"} {
		if _, ok := first[present]; !ok {
			t.Fatalf("the lodge result lost %q: %v", present, first)
		}
	}

	// Idempotent re-lodge, unchanged.
	resp = c.roundTrip(lodgeJSON(csrPEM, "old-client"))
	var second map[string]any
	assertNoErr(t, json.Unmarshal(resp.Result, &second), "decode the re-lodge result")
	if second["request_id"] != first["request_id"] {
		t.Fatalf("re-lodge changed the request id: %v then %v", first["request_id"], second["request_id"])
	}

	// Poll with no sas_open, unchanged.
	resp = c.roundTrip(pollJSON(first["request_id"].(string)))
	var polled map[string]any
	assertNoErr(t, json.Unmarshal(resp.Result, &polled), "decode the poll result")
	if polled["status"] != "pending" {
		t.Fatalf("poll status = %v, want pending", polled["status"])
	}

	// An unknown id still reads as "unknown", not as a comparison refusal.
	resp = c.roundTrip(pollJSON("req_nope"))
	assertNoErr(t, json.Unmarshal(resp.Result, &polled), "decode the unknown poll result")
	if polled["status"] != "unknown" {
		t.Fatalf("unknown poll status = %v", polled["status"])
	}
}

// seedCAIntoSandboxless seeds a CA for a fixture that built its own table
// without a settings sandbox around it.
func seedCAIntoSandboxless(t *testing.T, table *enrolmentRequestTable) {
	t.Helper()
	mkEmptySandboxRelayHome(t)
	seedCAInto(t, table)
}

// ---------------------------------------------------------------------------
// §2.1, §2.3: the door refuses malformed comparison fields before the table
// ---------------------------------------------------------------------------

func TestSAS_DecodeRefusesMalformedComparisonFields(t *testing.T) {
	csrPEM := string(genClientCSRPEM(t, "decode"))

	badCommits := []string{
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("A", 64), // uppercase hex is one spelling too many
		strings.Repeat("z", 64),
	}
	for _, commit := range badCommits {
		line, err := json.Marshal(bridge.EnrolmentRequestWire{
			Type: bridge.ReqEnrolmentRequest, CSRPEM: csrPEM, SASCommit: commit,
		})
		assertNoErr(t, err, "marshal")
		if _, err := bridge.DecodeEnrolmentRequest(line); err == nil ||
			!strings.Contains(err.Error(), "64 lowercase hex") {
			t.Fatalf("sas_commit %q: err = %v", commit, err)
		}
	}

	for _, profile := range []string{"has spaces", "newline\ninjection", strings.Repeat("p", 65), "../escape"} {
		line, err := json.Marshal(bridge.EnrolmentRequestWire{
			Type: bridge.ReqEnrolmentRequest, CSRPEM: csrPEM, RequestedProfile: profile,
		})
		assertNoErr(t, err, "marshal")
		if _, err := bridge.DecodeEnrolmentRequest(line); err == nil ||
			!strings.Contains(err.Error(), "requested_profile") {
			t.Fatalf("requested_profile %q: err = %v", profile, err)
		}
	}

	for _, open := range []string{strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32)} {
		line, err := json.Marshal(bridge.EnrolmentRequestPollWire{
			Type: bridge.ReqEnrolmentRequestPoll, RequestID: "req_x", SASOpen: open,
		})
		assertNoErr(t, err, "marshal")
		if _, err := bridge.DecodeEnrolmentRequestPoll(line); err == nil ||
			!strings.Contains(err.Error(), "32 lowercase hex") {
			t.Fatalf("sas_open %q: err = %v", open, err)
		}
	}

	// Strict decoding is untouched: an unknown field is still loud.
	if _, err := bridge.DecodeEnrolmentRequest([]byte(`{"type":"EnrolmentRequest","csr_pem":"x","nope":1}`)); err == nil {
		t.Fatal("DisallowUnknownFields no longer refuses an unknown field")
	}
}

// The requested profile is hostile input the table refuses on the same terms
// as the label, and it never reaches a grant.
func TestSAS_RequestedProfileIsBoundedAndOnlyEverDisplayed(t *testing.T) {
	table, _, _ := sasCATable(t)
	if _, err := table.Lodge(genClientCSRPEM(t, "rp"), "", strings.Repeat("p", maxEnrolmentLabelBytes+1), "", "10.0.0.1:1"); err == nil ||
		!strings.Contains(err.Error(), "requested_profile must be") {
		t.Fatalf("err = %v, want a refusal naming requested_profile", err)
	}

	l, err := table.Lodge(genClientCSRPEM(t, "rp2"), "", "proj_mail", "", "10.0.0.2:1")
	assertNoErr(t, err, "lodge with a requested profile")
	if got := viewFor(t, table, l.RequestID).RequestedProfile; got != "proj_mail" {
		t.Fatalf("RequestedProfile = %q, want proj_mail", got)
	}
}

// ---------------------------------------------------------------------------
// suggestClientID is advisory
// ---------------------------------------------------------------------------

func TestSuggestClientID_SuffixesOnCollisionAndRefusesUnusableLabels(t *testing.T) {
	s := &config.Settings{}
	if got := suggestClientID(s, "hermes-mail"); got != "hermes-mail" {
		t.Fatalf("free label suggested %q", got)
	}

	s.Enrolments = []config.Enrolment{{ClientID: "hermes-mail"}, {ClientID: "hermes-mail-2"}}
	if got := suggestClientID(s, "hermes-mail"); got != "hermes-mail-3" {
		t.Fatalf("collision suggested %q, want hermes-mail-3", got)
	}

	for _, bad := range []string{"", "   ", "has spaces", "../escape", strings.Repeat("x", maxEnrolmentLabelBytes+1)} {
		if got := suggestClientID(s, bad); got != "" {
			t.Fatalf("label %q suggested %q, want no suggestion", bad, got)
		}
	}

	// Every suffix taken: no suggestion rather than a wrong one.
	full := &config.Settings{Enrolments: []config.Enrolment{{ClientID: "taken"}}}
	for n := 2; n <= 99; n++ {
		full.Enrolments = append(full.Enrolments, config.Enrolment{ClientID: "taken-" + itoaForTest(n)})
	}
	if got := suggestClientID(full, "taken"); got != "" {
		t.Fatalf("suggested %q when every candidate is taken", got)
	}
}

func itoaForTest(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
