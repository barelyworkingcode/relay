package main

// Coverage for the presence gate's wiring into the six operation cores
// (ADR-017 implementation spec §6, §7.4). The nonce properties themselves —
// single-use, operation-bound, argument-bound, 120s, uniform refusal — are
// proven once, thoroughly, in package presence (presence_test.go,
// digest_test.go): Require mints and redeems in the same call, so there is
// no way to exercise "reuse a grant across two calls" from outside that
// package. What belongs here instead is proof that each core actually
// reaches the gate rather than merely being reviewed to: a nil Gate refuses
// (AC-16b), issuance auditing being off refuses before the provider is ever
// called (§7.4, AC-26/AC-26b), and the digest genuinely binds the arguments
// §6.4 lists for each operation — a project update touching only its name
// must not prompt, and one that turns on allow_cwd_auth or widens
// allowed_tools must (AC-16c).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relaygo/presence"
	"relaygo/presence/presencetest"
)

func pgwSandbox(t *testing.T) (string, SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return dir, store
}

// pgwCase is one gated operation's minimal legal call. seed prepares
// whatever the operation needs to already exist (a credential to revoke, an
// enrolment to update) and runs BEFORE the untouched-file snapshot every
// caller takes; run is the gated call itself and nothing else, so a test
// comparing settings.json before and after run sees only what the gated
// operation itself did.
type pgwCase struct {
	op   string
	seed func(t *testing.T, store SettingsStore)
	run  func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error
}

// pgwCases covers every gated operation exercisable through this table's
// generic context/store/gate/issuance run() shape. sealed.reset is
// deliberately absent: it is a free function (resetSealedStore) reachable
// from no door but the tray's own "Reset Sealed Store…" menu item, not a
// method on an ops core any of the other rows' shape could construct — see
// the comment on TestGate_EveryImplementedOpRefusesWithoutGate.
func pgwCases(t *testing.T) []pgwCase {
	noSeed := func(*testing.T, SettingsStore) {}
	return []pgwCase{
		{"credential.mint", noSeed, func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
			ops := &CredentialOps{Store: store, Gate: gate, Issuance: issuance}
			_, _, err := ops.Mint(context.Background(), credentialMintRequest{Name: "eve-view", Classes: []string{"read"}}, auditViaCLI, "")
			return err
		}},
		{"credential.revoke",
			func(t *testing.T, store SettingsStore) {
				_, _, err := mintAPICredential(store, credentialMintRequest{Name: "to-revoke", Classes: []string{"read"}})
				assertNoErr(t, err, "seed credential")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				cred := store.Get().APICredentials[len(store.Get().APICredentials)-1]
				ops := &CredentialOps{Store: store, Gate: gate, Issuance: issuance}
				_, err := ops.Revoke(context.Background(), cred.ID, auditViaCLI, "")
				return err
			}},
		{"enrolment.create",
			func(t *testing.T, store SettingsStore) {
				mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				profile := store.Get().Projects[0]
				ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: issuance}
				_, err := ops.Create(context.Background(), enrolmentFields{ClientID: "pgw-client", ProjectIDs: []string{profile.ID}}, auditViaCLI, "")
				return err
			}},
		{"enrolment.update",
			func(t *testing.T, store SettingsStore) {
				profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
				if _, err := createEnrolment(store, enrolmentRequest{ClientID: "pgw-update", ProjectIDs: []string{profile.ID}}); err != nil {
					t.Fatalf("seed enrolment: %v", err)
				}
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: issuance}
				calls := 5
				_, _, err := ops.Update(context.Background(), enrolmentUpdateRequest{ClientID: "pgw-update", Budget: enrolmentBudgetUpdate{MaxCalls: &calls}}, auditViaCLI, "")
				return err
			}},
		{"enrolment.revoke",
			func(t *testing.T, store SettingsStore) {
				profile := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
				if _, err := createEnrolment(store, enrolmentRequest{ClientID: "pgw-revoke", ProjectIDs: []string{profile.ID}}); err != nil {
					t.Fatalf("seed enrolment: %v", err)
				}
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: issuance}
				_, err := ops.Revoke(context.Background(), "pgw-revoke", auditViaCLI, "")
				return err
			}},
		{"enrolment.sign",
			func(t *testing.T, store SettingsStore) {
				mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				profile := store.Get().Projects[0]
				ops := &EnrolmentOps{Store: store, Gate: gate, Issuance: issuance}
				csrPEM := genClientCSRPEM(t, "pgw-sign-client")
				_, err := ops.Sign(context.Background(), enrolmentSignFields{ClientID: "pgw-sign-client", ProjectIDs: []string{profile.ID}, CSRPEM: string(csrPEM)}, auditViaCLI, "")
				return err
			}},
		{"login.bootstrap.mint", noSeed, func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
			ops := &LoginOps{Store: store, Gate: gate, Audit: pgwAuditRecorderFor(t, issuance)}
			_, err := ops.MintBootstrap(context.Background(), auditViaCLI)
			return err
		}},
		{"login.passkey.revoke",
			func(t *testing.T, store SettingsStore) {
				assertNoErr(t, store.With(func(s *Settings) {
					s.Passkeys = append(s.Passkeys, Passkey{ID: "pgw-passkey", Name: "pgw"})
				}), "seed passkey")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &LoginOps{Store: store, Gate: gate, Audit: pgwAuditRecorderFor(t, issuance)}
				_, err := ops.RevokePasskey(context.Background(), "pgw-passkey")
				return err
			}},
		{"mcp.register", noSeed, func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
			// stdio against the real cmd/testmcp peer, never a network
			// address: the hermetic suite must not depend on DNS or a real
			// socket (ADR-001/ADR-002), and http transport would need both.
			ops := &McpOps{Store: store, Ctx: context.Background(), Gate: gate, Issuance: issuance}
			_, err := ops.Add(context.Background(), mcpFields{DisplayName: "pgw-mcp", Command: buildTestMcpBinary(t)}, auditViaCLI, "")
			return err
		}},
		{"mcp.oauth.start",
			func(t *testing.T, store SettingsStore) {
				assertNoErr(t, store.With(func(s *Settings) {
					s.UpsertExternalMcp(ExternalMcp{ID: "pgw-oauth", DisplayName: "pgw", Transport: "http", URL: "https://mcp.example.test/"})
				}), "seed mcp")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &McpOps{Store: store, Ctx: context.Background(), Gate: gate, Issuance: issuance,
					StartFlow: func(string, func(string)) (*oauthResult, error) { return &oauthResult{AccessToken: "granted"}, nil },
				}
				_, err := ops.StartOAuth(context.Background(), "pgw-oauth", func(string) {}, auditViaCLI, "")
				return err
			}},
		{"service.register", noSeed, func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
			ops := &ServiceOps{Store: store, Registry: noopServiceManager{}, Gate: gate, Issuance: issuance}
			_, err := ops.Create(context.Background(), serviceFields{DisplayName: "pgw-svc", Command: "/bin/true"}, auditViaCLI, "")
			return err
		}},
		{"project.rotate_token",
			func(t *testing.T, store SettingsStore) {
				mkStoreProject(t, store, ProjectKindLocal, "pgw-project", t.TempDir())
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				proj := store.Get().Projects[0]
				ops := &ProjectOps{Store: store, Gate: gate, Issuance: issuance}
				_, _, err := ops.RotateToken(context.Background(), proj.ID, auditViaCLI, "")
				return err
			}},
		{"project.grant", noSeed, func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
			ops := &ProjectOps{Store: store, Gate: gate, Issuance: issuance}
			_, err := ops.Create(context.Background(), projectCreateFields{Name: "pgw-grant", Path: t.TempDir()}, nil, auditViaCLI, "")
			return err
		}},
	}
}

// pgwUngatedCase is one core method that calls requireIssuanceAuditor but
// never requireGate — the shape a retired op takes on (§4.2). method names
// the method the way gate_ast_scan_test.go's scan reports it
// ("Receiver.Method"), which is what lets
// TestGate_EveryIssuanceAuditorCallSiteHasACase compare this table against
// source instead of against itself.
type pgwUngatedCase struct {
	method string
	seed   func(t *testing.T, store SettingsStore)
	run    func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error
}

// pgwUngatedCases covers every core method retired from presence.GatedOps
// by ADR-018 step 3: removal only narrows what a caller already reaches
// (re-registering under the same id still hits the register op's gate), so
// it never needed the gate — but requireIssuanceAuditor and
// recordConfigChange still run, with an empty presence_id (§5.1, §5.2).
//
// ProjectOps.NarrowForEnrolment is NOT a row here even though it also
// calls requireIssuanceAuditor and never requireGate: it predates this
// step (§3.6, untouched), its signature (a project id and a
// bridge.RemoteCaller, not this table's context/store/gate/issuance shape)
// does not fit either table, and its issuance-off behaviour already has a
// dedicated regression —
// TestNarrowForEnrolment_IssuanceAuditingOffRefusesBeforeTouchingStore in
// remote_config_plane_test.go. TestGate_EveryIssuanceAuditorCallSiteHasACase
// accounts for it by name instead of by a row here.
func pgwUngatedCases(t *testing.T) []pgwUngatedCase {
	return []pgwUngatedCase{
		{"McpOps.Remove",
			func(t *testing.T, store SettingsStore) {
				assertNoErr(t, store.With(func(s *Settings) {
					s.UpsertExternalMcp(ExternalMcp{ID: "pgw-ungated-mcp", DisplayName: "pgw", Transport: "http", URL: "https://mcp.example.test/"})
				}), "seed mcp")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &McpOps{Store: store, Ctx: context.Background(), Gate: gate, Issuance: issuance}
				return ops.Remove(context.Background(), "pgw-ungated-mcp", auditViaCLI, "")
			}},
		{"ServiceOps.Remove",
			func(t *testing.T, store SettingsStore) {
				assertNoErr(t, store.With(func(s *Settings) {
					s.UpsertService(ServiceConfig{ID: "pgw-ungated-svc", DisplayName: "pgw", Command: "/bin/true"})
				}), "seed service")
			},
			func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error {
				ops := &ServiceOps{Store: store, Registry: noopServiceManager{}, Gate: gate, Issuance: issuance}
				return ops.Remove(context.Background(), "pgw-ungated-svc", auditViaCLI, "")
			}},
	}
}

// pgwAuditRecorderFor lets a case built for the generic IssuanceAuditor
// interface (every core but LoginOps) share its "issuance off" (nil) and
// "issuance on" (a real recorder) states with LoginOps, whose Audit field is
// the concrete *AuditRecorder type. issuance is either nil or the exact
// value pgwWithIssuance below hands every other case.
func pgwAuditRecorderFor(t *testing.T, issuance IssuanceAuditor) *AuditRecorder {
	t.Helper()
	if issuance == nil {
		return nil
	}
	rec, ok := issuance.(*AuditRecorder)
	if !ok {
		t.Fatalf("pgwAuditRecorderFor: issuance is a %T, not *AuditRecorder", issuance)
	}
	return rec
}

// TestGate_EveryImplementedOpRefusesWithoutGate is AC-16b over every
// gated operation that has a core as of S5. sealed.reset is deliberately
// absent: it is a free function (resetSealedStore) reachable from no door
// but the tray's own "Reset Sealed Store…" menu item, not a method on an
// ops core this table's context/store/gate/issuance run() shape could
// construct.
func TestGate_EveryImplementedOpRefusesWithoutGate(t *testing.T) {
	for _, tc := range pgwCases(t) {
		t.Run(tc.op, func(t *testing.T) {
			dir, store := pgwSandbox(t)
			tc.seed(t, store)
			before := odwSnap(t, dir)

			issuance := pgwWithIssuance(t)
			err := tc.run(t, store, nil, issuance)
			if !errors.Is(err, errPresenceGateNotWired) {
				t.Fatalf("%s with a nil Gate: err = %v, want errPresenceGateNotWired", tc.op, err)
			}
			before.assertUntouched(t, dir, tc.op+" (nil gate)")
		})
	}
}

// TestGate_EveryImplementedOpSucceedsWithAnAllowingGate is the positive
// control for the table above: every case must be a genuinely legal call
// that succeeds once a gate is wired, so a refusal in the test above is
// proof the gate fired and not an artifact of a broken fixture.
func TestGate_EveryImplementedOpSucceedsWithAnAllowingGate(t *testing.T) {
	for _, tc := range pgwCases(t) {
		t.Run(tc.op, func(t *testing.T) {
			_, store := pgwSandbox(t)
			tc.seed(t, store)
			gate, err := presence.NewGate(presencetest.Allow())
			assertNoErr(t, err, "NewGate")
			if err := tc.run(t, store, gate, pgwWithIssuance(t)); err != nil {
				t.Fatalf("%s with an allowing gate and issuance auditing on: %v", tc.op, err)
			}
		})
	}
}

// pgwAuditingCase is the shape TestGate_IssuanceAuditingOffRefusesBeforeThePrompt
// needs from either table: a label for the subtest name, the seed, and the
// run closure. pgwCase and pgwUngatedCase both convert into this — the
// point of §4.2's rewrite is that this test's coverage must not shrink when
// an op leaves pgwCases, so it iterates the union of both tables rather
// than pgwCases alone.
type pgwAuditingCase struct {
	label string
	seed  func(t *testing.T, store SettingsStore)
	run   func(t *testing.T, store SettingsStore, gate *presence.Gate, issuance IssuanceAuditor) error
}

func pgwAllAuditingCases(t *testing.T) []pgwAuditingCase {
	var out []pgwAuditingCase
	for _, tc := range pgwCases(t) {
		out = append(out, pgwAuditingCase{label: tc.op, seed: tc.seed, run: tc.run})
	}
	for _, tc := range pgwUngatedCases(t) {
		out = append(out, pgwAuditingCase{label: tc.method, seed: tc.seed, run: tc.run})
	}
	return out
}

// TestGate_IssuanceAuditingOffRefusesBeforeThePrompt is AC-26 and AC-26b,
// widened by §4.2 to pgwCases ∪ pgwUngatedCases: with no sink to record
// into, EVERY op that reaches requireIssuanceAuditor refuses before it ever
// asks for presence — gated or not, since a retired op still calls
// requireIssuanceAuditor (§3.4) — and a Recording provider proves zero
// calls reached it.
func TestGate_IssuanceAuditingOffRefusesBeforeThePrompt(t *testing.T) {
	for _, tc := range pgwAllAuditingCases(t) {
		t.Run(tc.label, func(t *testing.T) {
			dir, store := pgwSandbox(t)
			tc.seed(t, store)
			before := odwSnap(t, dir)

			recording := presencetest.NewRecording(nil)
			gate, err := presence.NewGate(recording)
			assertNoErr(t, err, "NewGate")

			err = tc.run(t, store, gate, nil)
			if !errors.Is(err, errIssuanceAuditingRequired) {
				t.Fatalf("%s with auditing off: err = %v, want errIssuanceAuditingRequired", tc.label, err)
			}
			if recording.Calls() != 0 {
				t.Errorf("%s: presence provider called %d time(s) with auditing off; must refuse before prompting", tc.label, recording.Calls())
			}
			before.assertUntouched(t, dir, tc.label+" (auditing off)")
		})
	}
}

// TestGate_RetiredOpsNeverPrompt is AC-4: a retired op behind a REAL Gate
// wrapping a Recording provider must both succeed and never touch the
// provider — proof of removal, not merely the absence of an assertion (the
// same argument TestProjectOps_UpdateTouchingOnlyNameDoesNotPrompt already
// makes for the configure subset, applied here to mcp.unregister and
// service.unregister).
func TestGate_RetiredOpsNeverPrompt(t *testing.T) {
	for _, tc := range pgwUngatedCases(t) {
		t.Run(tc.method, func(t *testing.T) {
			_, store := pgwSandbox(t)
			tc.seed(t, store)

			recording := presencetest.NewRecording(nil)
			gate, err := presence.NewGate(recording)
			assertNoErr(t, err, "NewGate")

			if err := tc.run(t, store, gate, pgwWithIssuance(t)); err != nil {
				t.Fatalf("%s: %v", tc.method, err)
			}
			if n := recording.Calls(); n != 0 {
				t.Errorf("%s: presence provider called %d time(s); a retired op must never prompt", tc.method, n)
			}
		})
	}
}

// pgwUngatedCaseConfigChangeCredential names the auditCredential* value
// each pgwUngatedCases row's config_change record carries, so
// TestGate_RetiredOpsStillWriteConfigChange can assert it without
// re-deriving it from the op's own core.
var pgwUngatedCaseConfigChangeCredential = map[string]string{
	"McpOps.Remove":     auditCredentialExternalMcp,
	"ServiceOps.Remove": auditCredentialService,
}

// TestGate_RetiredOpsStillWriteConfigChange is AC-12: removing the gate
// must not also remove the record. Each retired op must still write the
// same config_change it did while gated -- event, credential, subject,
// via and outcome unchanged -- with only presence_id now empty (§5.1,
// §5.2, §3.4).
func TestGate_RetiredOpsStillWriteConfigChange(t *testing.T) {
	for _, tc := range pgwUngatedCases(t) {
		t.Run(tc.method, func(t *testing.T) {
			_, store := pgwSandbox(t)
			tc.seed(t, store)

			logPath := filepath.Join(t.TempDir(), "audit.jsonl")
			rec, err := NewAuditRecorder(nil, logPath)
			assertNoErr(t, err, "NewAuditRecorder")
			t.Cleanup(rec.Close)

			if err := tc.run(t, store, allowGate(t), issuanceAuditorOrNil(rec)); err != nil {
				t.Fatalf("%s: %v", tc.method, err)
			}
			rec.Close() // flush and wait for the writer goroutine before reading

			data, err := os.ReadFile(logPath)
			assertNoErr(t, err, "read audit log")
			events := aiParse(t, string(data))

			var found *AuditEvent
			for i := range events {
				if events[i].Event == AuditEventConfigChange {
					found = &events[i]
				}
			}
			if found == nil {
				t.Fatalf("%s: no config_change record written; events = %+v", tc.method, events)
			}
			if wantCred := pgwUngatedCaseConfigChangeCredential[tc.method]; found.Credential != wantCred {
				t.Errorf("%s: credential = %q, want %q", tc.method, found.Credential, wantCred)
			}
			if found.Via != auditViaCLI {
				t.Errorf("%s: via = %q, want %q", tc.method, found.Via, auditViaCLI)
			}
			if found.Outcome != "ok" {
				t.Errorf("%s: outcome = %q, want %q", tc.method, found.Outcome, "ok")
			}
			if found.PresenceID != "" {
				t.Errorf("%s: presence_id = %q, want empty (removal is not gated)", tc.method, found.PresenceID)
			}
		})
	}
}

// TestGate_EveryIssuanceAuditorCallSiteHasACase is §4.2's fifth guard: an
// AST scan of every requireIssuanceAuditor call site must equal the union
// of pgwCases' and pgwUngatedCases' methods (plus the two documented
// exemptions below), so a future op that reaches requireIssuanceAuditor
// without landing in either table goes unnoticed by neither
// TestGate_IssuanceAuditingOffRefusesBeforeThePrompt nor this one.
//
// Two exemptions, both named rather than silently absorbed:
//
//   - resetSealedStore (sealed.reset) calls requireGate but never
//     requireIssuanceAuditor — it is the tray-only break-glass recovery for a
//     degraded sealed store (ADR-017 §5.6 clause 5), and the record its own
//     act would need to write may itself be part of what is degraded. It is
//     excluded from the "want" side entirely.
//   - ProjectOps.NarrowForEnrolment calls requireIssuanceAuditor and never
//     requireGate but has no row in either table — see pgwUngatedCases' doc
//     comment for why. It is added to "want" by name.
func TestGate_EveryIssuanceAuditorCallSiteHasACase(t *testing.T) {
	root := gsModuleRoot(t)

	got := map[string]bool{}
	for _, m := range scanRequireIssuanceAuditorCallSites(t, root) {
		got[m] = true
	}

	want := map[string]bool{}
	for op, methods := range wantGateCallSites {
		if op == "sealed.reset" {
			continue
		}
		for _, m := range methods {
			want[m] = true
		}
	}
	for _, uc := range pgwUngatedCases(t) {
		want[uc.method] = true
	}
	want["ProjectOps.NarrowForEnrolment"] = true

	for m := range want {
		if !got[m] {
			t.Errorf("%s is expected (by pgwCases, pgwUngatedCases, or a documented exemption) to call requireIssuanceAuditor, but no call site was found", m)
		}
	}
	for m := range got {
		if !want[m] {
			t.Errorf("%s calls requireIssuanceAuditor but is accounted for by neither pgwCases nor pgwUngatedCases nor a documented exemption", m)
		}
	}
}

// pgwWithIssuance returns a real, Ready() *AuditRecorder boxed as an
// IssuanceAuditor -- the "auditing on" state every op-succeeds case needs.
func pgwWithIssuance(t *testing.T) IssuanceAuditor {
	t.Helper()
	return issuanceAuditorOrNil(enabledIssuanceRecorder(t))
}

// TestGate_TableCoversGatedOps requires pgwCases' key set to equal
// presence.GatedOps minus sealed.reset exactly (which has no core to test
// through this table's shape — see the comment on
// TestGate_EveryImplementedOpRefusesWithoutGate: it is a free function with
// a tray-only door, not an op with no core yet). Adding a gated op without
// adding a case here fails this test by name. Strengthened (§4.3) with the
// wantGatedOps literal comparison and a disjointness check between
// pgwCases and pgwUngatedCases at the method level, so neither table can
// silently claim a method the other one already covers.
func TestGate_TableCoversGatedOps(t *testing.T) {
	have := map[string]bool{}
	for _, tc := range pgwCases(t) {
		if have[tc.op] {
			t.Fatalf("duplicate case for op %q", tc.op)
		}
		have[tc.op] = true
	}
	for _, op := range presence.GatedOps {
		if op == "sealed.reset" {
			continue
		}
		if !have[op] {
			t.Errorf("presence.GatedOps has %q with no case in pgwCases", op)
		}
		delete(have, op)
	}
	for op := range have {
		t.Errorf("pgwCases has %q, which is not in presence.GatedOps", op)
	}

	// wantGatedOps literal comparison: pgwCases' key set must equal the
	// pinned list minus sealed.reset, not merely equal presence.GatedOps
	// (which gate_structural_test.go's own TestGate_GatedOpsMatchesPinnedList
	// pins independently) — two routes to the same fact rather than one
	// indirect one.
	haveFromWant := map[string]bool{}
	for _, tc := range pgwCases(t) {
		haveFromWant[tc.op] = true
	}
	for _, op := range wantGatedOps {
		if op == "sealed.reset" {
			continue
		}
		if !haveFromWant[op] {
			t.Errorf("wantGatedOps has %q with no case in pgwCases", op)
		}
		delete(haveFromWant, op)
	}
	for op := range haveFromWant {
		t.Errorf("pgwCases has %q, which is not in wantGatedOps", op)
	}

	// Disjointness: no core method may be claimed by both tables.
	gatedMethods := map[string]bool{}
	for _, methods := range wantGateCallSites {
		for _, m := range methods {
			gatedMethods[m] = true
		}
	}
	for _, uc := range pgwUngatedCases(t) {
		if gatedMethods[uc.method] {
			t.Errorf("%s is claimed by both the gated call-site map and pgwUngatedCases", uc.method)
		}
	}
}

// TestProjectOps_UpdateTouchingOnlyNameDoesNotPrompt and its siblings below
// are AC-16c: the configure subset is exactly right.
func TestProjectOps_UpdateTouchingOnlyNameDoesNotPrompt(t *testing.T) {
	_, store := pgwSandbox(t)
	proj := mkStoreProject(t, store, ProjectKindLocal, "renameable", t.TempDir())

	// Deny() proves the point harder than Allow(): if this update reached
	// the gate at all, it would refuse, not merely "also succeed."
	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ProjectOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	newName := "renamed"
	_, found, err := ops.Update(context.Background(), proj.ID, projectUpdateFields{Name: &newName}, func() McpSurfaces { return nil }, auditViaCLI, "")
	if err != nil {
		t.Fatalf("a name-only update must not reach the gate at all: %v", err)
	}
	if !found {
		t.Fatal("project not found")
	}
}

func TestProjectOps_AllowCwdAuthTurningOnIsGated(t *testing.T) {
	_, store := pgwSandbox(t)
	proj := mkStoreProject(t, store, ProjectKindLocal, "cwd-project", t.TempDir())

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ProjectOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	on := true
	_, _, err = ops.Update(context.Background(), proj.ID, projectUpdateFields{AllowCwdAuth: &on}, func() McpSurfaces { return nil }, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("turning on allow_cwd_auth: err = %v, want presence.ErrRefused", err)
	}
	if store.Get().Projects[0].AllowCwdAuth {
		t.Fatal("allow_cwd_auth was set despite the gate refusing")
	}
}

func TestProjectOps_WideningAllowedToolsIsGated(t *testing.T) {
	_, store := pgwSandbox(t)
	proj := mkStoreProject(t, store, ProjectKindLocal, "tools-project", t.TempDir())

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ProjectOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	tools := map[string][]string{"macmcp": {"mail_*"}}
	_, _, err = ops.Update(context.Background(), proj.ID, projectUpdateFields{AllowedTools: &tools}, func() McpSurfaces { return nil }, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("widening allowed_tools: err = %v, want presence.ErrRefused", err)
	}
}

// TestProjectOps_AllowExternalTurningOnIsGated is AC-16c's argument
// extended to allow_external (ADR-011 decision 2c): it is the same
// widening act as allowed_tools or allow_cwd_auth turning on — an agent
// holding a project token whose project it can edit does not need to mint
// anything to reach outbound, it just widens the grant it already has —
// so it must be refused the same way.
func TestProjectOps_AllowExternalTurningOnIsGated(t *testing.T) {
	_, store := pgwSandbox(t)
	proj := mkStoreProject(t, store, ProjectKindLocal, "external-project", t.TempDir())

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ProjectOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}

	external := map[string]bool{"macmcp": true}
	_, _, err = ops.Update(context.Background(), proj.ID, projectUpdateFields{AllowExternal: &external}, func() McpSurfaces { return nil }, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("turning on allow_external: err = %v, want presence.ErrRefused", err)
	}
	if store.Get().Projects[0].AllowExternal["macmcp"] {
		t.Fatal("allow_external was set despite the gate refusing")
	}
}

// TestCredentialOps_DigestBindsNameClassesAndTTL is AC-22d's argument for
// credential.mint specifically: changing any digested field must move the
// digest, so a grant answered for one mint cannot be redeemed for another.
func TestCredentialOps_DigestBindsNameClassesAndTTL(t *testing.T) {
	base := credentialMintRequest{Name: "eve-view", Classes: []string{"read"}}
	baseDigest := base.presenceDigest()

	variants := []credentialMintRequest{
		{Name: "eve-edit", Classes: []string{"read"}},
		{Name: "eve-view", Classes: []string{"read", "grant"}},
		{Name: "eve-view", Classes: []string{"read"}, TTL: hourTTL},
	}
	for i, v := range variants {
		if v.presenceDigest() == baseDigest {
			t.Errorf("variant %d (%+v) produced the same digest as the base request", i, v)
		}
	}
	if base.presenceDigest() != base.presenceDigest() {
		t.Fatal("presenceDigest is not deterministic over the same request")
	}
}

const hourTTL = 3600_000_000_000 // one hour, in time.Duration's nanosecond units

// TestProjectUpdateFields_DigestBindsAllEightGrantShapeFields is AC-9,
// standing guard over §2.4's trap: projectUpdateFields.presenceDigest must
// keep binding all eight grant-shape fields even though a future narrowing
// of project.grant's GATE to fire only on allow_cwd_auth (ADR-018, blocked
// on the local cli-admin identity binding) will make it look natural to
// shrink the digest to match. Changing any one of the eight, holding the
// rest fixed, must move the digest — the prompt authorises the request,
// not the reason the request was privileged.
func TestProjectUpdateFields_DigestBindsAllEightGrantShapeFields(t *testing.T) {
	base := projectUpdateFields{
		AllowedMcpIDs: ptr([]string{"macmcp"}),
		AllowedTools:  ptr(map[string][]string{"macmcp": {"mail_*"}}),
		Access:        ptr(map[string]string{"macmcp": "read"}),
		Context:       ptr(map[string]json.RawMessage{"macmcp": json.RawMessage(`{"a":1}`)}),
		AllowExternal: ptr(map[string]bool{"macmcp": false}),
		AllowCwdAuth:  ptr(false),
		Kind:          ptr(ProjectKindLocal),
		Path:          ptr("/tmp/base"),
	}
	baseDigest := base.presenceDigest("proj-x")

	variants := []struct {
		name   string
		mutate func(f *projectUpdateFields)
	}{
		{"allowed_mcp_ids", func(f *projectUpdateFields) { f.AllowedMcpIDs = ptr([]string{"fsmcp"}) }},
		{"allowed_tools", func(f *projectUpdateFields) { f.AllowedTools = ptr(map[string][]string{"macmcp": {"*"}}) }},
		{"access", func(f *projectUpdateFields) { f.Access = ptr(map[string]string{"macmcp": "write"}) }},
		{"context", func(f *projectUpdateFields) {
			f.Context = ptr(map[string]json.RawMessage{"macmcp": json.RawMessage(`{"a":2}`)})
		}},
		{"allow_external", func(f *projectUpdateFields) { f.AllowExternal = ptr(map[string]bool{"macmcp": true}) }},
		{"allow_cwd_auth", func(f *projectUpdateFields) { f.AllowCwdAuth = ptr(true) }},
		{"kind", func(f *projectUpdateFields) { f.Kind = ptr(ProjectKindRemote) }},
		{"path", func(f *projectUpdateFields) { f.Path = ptr("/tmp/other") }},
	}
	for _, v := range variants {
		variant := base
		v.mutate(&variant)
		if variant.presenceDigest("proj-x") == baseDigest {
			t.Errorf("changing %s alone did not move the digest", v.name)
		}
	}
	if base.presenceDigest("proj-x") != base.presenceDigest("proj-x") {
		t.Fatal("presenceDigest is not deterministic over the same request")
	}
}

// TestMcpFields_PresenceGrantBoundToID is the concrete form of the
// substitution the id-in-digest fix closes: Add upserts by id, so a grant
// approved for one id must never redeem against a request that resolves to
// a different one, even when every other field (display_name, command, ...)
// is byte-for-byte identical. Exercises the real Gate — Request to mint,
// Redeem against a different id's digest — rather than only comparing
// digests, so a regression that dropped id from the digest but happened to
// leave two unequal-looking Digest values would still be caught here.
func TestMcpFields_PresenceGrantBoundToID(t *testing.T) {
	gate := allowGate(t)
	same := mcpFields{DisplayName: "Same Name", Command: "/bin/true"}
	digestA := same.presenceDigest("id-a")
	digestB := same.presenceDigest("id-b")

	grant, err := gate.Request(context.Background(), "mcp.register", digestA, "register the MCP")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := gate.Redeem(grant, "mcp.register", digestB); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted for id %q redeemed against id %q: err = %v, want ErrGrantInvalid", "id-a", "id-b", err)
	}

	// Positive control: the same digest the grant was minted for still
	// redeems, so the refusal above is the id binding and not a broken gate.
	grant2, err := gate.Request(context.Background(), "mcp.register", digestA, "register the MCP")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := gate.Redeem(grant2, "mcp.register", digestA); err != nil {
		t.Fatalf("redeeming against the id it was minted for: %v", err)
	}
}

// TestServiceFields_PresenceGrantBoundToID is TestMcpFields_PresenceGrantBoundToID's
// argument for service.register, which has carried id in its digest since
// before this change — this pins that it still does.
func TestServiceFields_PresenceGrantBoundToID(t *testing.T) {
	gate := allowGate(t)
	same := serviceFields{DisplayName: "Same Name", Command: "/bin/true"}
	digestA := same.presenceDigest("id-a")
	digestB := same.presenceDigest("id-b")

	grant, err := gate.Request(context.Background(), "service.register", digestA, "register the service")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := gate.Redeem(grant, "service.register", digestB); !errors.Is(err, presence.ErrGrantInvalid) {
		t.Fatalf("a grant minted for id %q redeemed against id %q: err = %v, want ErrGrantInvalid", "id-a", "id-b", err)
	}
}

// TestServiceFields_DigestDistinguishesAbsentFromExplicitZeroValue is
// AC-22d's argument applied to the update-path fix: a request that leaves
// autostart out of the wire payload (Autostart == nil, "preserve whatever is
// stored") and one that explicitly sets it false ("turn it off") are two
// different acts on the record and must not share a grant.
func TestServiceFields_DigestDistinguishesAbsentFromExplicitZeroValue(t *testing.T) {
	off := false
	absent := serviceFields{DisplayName: "Svc", Command: "/bin/x"}
	explicitFalse := serviceFields{DisplayName: "Svc", Command: "/bin/x", Autostart: &off}

	if absent.presenceDigest("svc") == explicitFalse.presenceDigest("svc") {
		t.Fatal("autostart absent and autostart=false explicit produced the same digest")
	}
}
