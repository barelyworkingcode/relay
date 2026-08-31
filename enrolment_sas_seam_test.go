package main

// The STRUCTURAL guards for ADR-019's registration flow, in one file.
//
// TestEnrolment_AC12_NoPresencePromptEverForAnyNumberOfLodges
// (enrolment_request_channel_test.go) proves behaviourally that no lodge
// reaches a presence prompt as the code stands today. These four guards are
// what make it stay true. The argument this codebase makes everywhere else --
// two dispatch tables rather than one `if isRemote` check -- is that a silent
// widening should become a loud error; a property with no structural guard is
// exactly the silent kind. A `notify func(string, string)` field added to the
// pending table in six months is the failure mode, and nothing else in this
// suite catches it.
//
// The four, in the order §4.11 states them:
//
//  1. No push       -- the lodge path references nothing UI-shaped.
//  2. No callback   -- the table holds no func and no channel field.
//  3. One call site -- pendingEnrolmentNotifier.tick is called from exactly
//                      one place, and it is trayapp.go.
//  4. No prompt     -- extended behaviourally, through the tray's read as
//                      well as the peer's write.
//
// The fifth guard for this feature is the notification click handler's
// reachability scan (AC-35), which lives beside the notifier's own call-site
// tests in tray_notify_call_site_test.go because it walks the whole package
// rather than this seam.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"relaygo/presence"
	"relaygo/presence/presencetest"
)

// seamLodgePathFiles is the lodge path: every non-test source file an
// unauthenticated peer's bytes reach before an operator has decided anything.
var seamLodgePathFiles = []string{"enrolment_requests.go", "enrolment_request_server.go"}

// ---------------------------------------------------------------------------
// §4.11 guard 1 (AC-30) — no push: the lodge path references nothing UI-shaped
// ---------------------------------------------------------------------------

// seamForbiddenIdents is the union of §4.11's list and AC-30's. Each name is
// the head of a path from a stranger's socket to something that can prompt,
// notify, sign or store: Gate/Require/Evaluate are presence, Notify/Platform/
// cocoa are the screen, App is the tray, EnrolmentOps is issuance.
var seamForbiddenIdents = []string{
	"Gate", "Require", "Evaluate",
	"Notify", "Platform", "cocoa", "App",
	"EnrolmentOps", "presence",
}

// seamForbiddenImports names the two §4.4 calls out by path. presence is the
// prompt; crypto/tls is the other half of ADR-018's listener separation, and
// an import of it here would mean this listener had grown a handshake to be
// confused with the tool plane's.
var seamForbiddenImports = []string{"relaygo/presence", "crypto/tls"}

// This is an AST scan and must stay one. A grep-based version of this guard
// fails on its first run and then gets defanged: `Gate`, `Require`, `App` and
// `presence` all appear in these files as prose in doc comments, and `App`
// and `Require` are substrings of `Approve` and `Required`. Parsing with mode
// 0 leaves comments out of the tree entirely, so a file's own comment saying
// "this file never touches presence.Gate" can neither satisfy this guard nor
// trip it, and only real identifiers are examined.
func TestEnrolmentSeam_LodgePathReferencesNothingUIShaped(t *testing.T) {
	root := gateASTModuleRoot(t)
	forbidden := map[string]bool{}
	for _, name := range seamForbiddenIdents {
		forbidden[name] = true
	}

	fset := token.NewFileSet()
	var identsSeen int
	var sawKnownIdent bool
	for _, name := range seamLodgePathFiles {
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		assertNoErr(t, err, "parse %s", name)

		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range seamForbiddenImports {
				if path == bad {
					t.Errorf("%s imports %q — the lodge path must not be able to reach it at all", name, bad)
				}
			}
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			if forbidden[local] {
				t.Errorf("%s imports %q under the name %q, which is on the forbidden list", name, path, local)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// A SelectorExpr's Sel is itself an *ast.Ident and is walked
			// here too, so `presence.Gate` is caught on both halves.
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			identsSeen++
			if id.Name == "Lodge" {
				sawKnownIdent = true
			}
			if forbidden[id.Name] {
				t.Errorf("%s references the identifier %q at %s — this is the whole of ADR-018 §8 P1: "+
					"a stranger's bytes reach this file, and nothing they reach may name a prompt, a screen or an issuer",
					name, id.Name, fset.Position(id.Pos()))
			}
			return true
		})
	}

	// Without these the guard would pass on an empty parse, a renamed file
	// or a scan that silently visited nothing.
	if identsSeen < 100 {
		t.Fatalf("the scan visited only %d identifiers across %v; it is misconfigured and would pass vacuously", identsSeen, seamLodgePathFiles)
	}
	if !sawKnownIdent {
		t.Fatalf("the scan never saw the identifier Lodge in %v; it is not reading the files it names", seamLodgePathFiles)
	}
}

// ---------------------------------------------------------------------------
// §4.11 guard 2 (AC-31) — the table holds no callback and no channel
// ---------------------------------------------------------------------------

// A callback field or a channel field is HOW a push would arrive: the table
// is the one object a lodge writes to, so it is the one object that could be
// handed something to call. Everything the tray learns, it learns by reading
// LodgeGeneration on a timer it already runs.
//
// This is deliberately scoped to enrolmentRequestTable alone, and it looks
// like it should also cover EnrolmentRequestServer. It must not:
// EnrolmentRequestServer.cancel is a context.CancelFunc, a legitimate Func
// field that a naively widened check fails on the day it is widened — and the
// two ways out of that failure are deleting this guard or allowlisting the
// server wholesale, both of which lose the property. The table is the correct
// scope because the table is what Lodge writes to.
func TestEnrolmentSeam_TableHoldsNoCallbackOrChannelField(t *testing.T) {
	// Allowlisted BY NAME, never by position or count, so adding an inert
	// field (caCertPEM, caSPKI and lodgeGen all arrived this way) never
	// requires touching this guard, and renaming a seam does.
	allowedFuncFields := map[string]bool{
		"now":  true, // the injected clock, for expiry tests
		"rand": true, // the injected CSPRNG, for request-id tests
	}

	typ := reflect.TypeOf(enrolmentRequestTable{})
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		seen[f.Name] = true
		switch f.Type.Kind() {
		case reflect.Func:
			if !allowedFuncFields[f.Name] {
				t.Errorf("enrolmentRequestTable.%s is a %s — a callback field on the pending table is how an "+
					"unauthenticated lodge would become a push; the tray must keep learning by reading LodgeGeneration",
					f.Name, f.Type)
			}
		case reflect.Chan:
			t.Errorf("enrolmentRequestTable.%s is a %s — a channel on the pending table is a push with a queue in front of it",
				f.Name, f.Type)
		}
	}

	// The allowlist names two fields that must actually exist, or it is
	// permitting nothing and would go on passing after a rename.
	for name := range allowedFuncFields {
		if !seen[name] {
			t.Errorf("the allowlist names enrolmentRequestTable.%s, which no longer exists — the allowlist has gone stale", name)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.11 guard 3 (AC-32) — the notification is raised from exactly one place
// ---------------------------------------------------------------------------

// A second call site silently doubles every bound tray_notify_test.go pins:
// two ticks per poll is two notifications a minute, not one. AST rather than
// grep for the same reason guard 1 is: a mention in a comment or a string
// must not be able to satisfy it, and a real call in a file nobody reads must
// not be able to hide.
func TestEnrolmentSeam_NotifierTickHasExactlyOneCallSite(t *testing.T) {
	root := gateASTModuleRoot(t)
	fset := token.NewFileSet()

	type site struct{ file, method, recv string }
	var sites []site
	for _, name := range gateASTFiles(t, root) {
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		assertNoErr(t, err, "parse %s", name)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			method := enclosingMethodName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "tick" {
					return true
				}
				recv := ""
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					recv = inner.Sel.Name
				}
				sites = append(sites, site{name, method, recv})
				return true
			})
		}
	}

	if len(sites) != 1 {
		t.Fatalf("found %d call sites of .tick(): %+v — want exactly one, in trayapp.go", len(sites), sites)
	}
	got := sites[0]
	if got.file != "trayapp.go" {
		t.Errorf("the notifier is driven from %s, want trayapp.go", got.file)
	}
	if got.method != "App.updateMenuWithSettings" {
		t.Errorf("the notifier is driven from %s, want App.updateMenuWithSettings — the count and the banner must come from one read", got.method)
	}
	if got.recv != "enrolNotifier" {
		t.Errorf("the single .tick() call is on %q, not the pending-enrolment notifier", got.recv)
	}
}

// ---------------------------------------------------------------------------
// §4.11 guard 4 (AC-29, extended) — nothing a peer can cause reaches presence
// ---------------------------------------------------------------------------

// seamPeerDrivenSurface drives every outcome an unauthenticated peer can
// produce against one table: lodge with a commitment and without, an
// idempotent re-lodge, a mismatched commitment, a poll that opens correctly
// and one that does not, the table filled to its cap and refused past it,
// expired and refilled, a malformed CSR, an oversized label and an oversized
// requested_profile.
//
// Each phase gets a fresh table where the previous one has been driven into
// the ceremony limiter's escalated backoff on purpose — the limiter refusing
// is itself one of the outcomes under test, and a refusal is not a place a
// prompt could appear either.
func seamPeerDrivenSurface(t *testing.T, seedCA bool, drive func(table *enrolmentRequestTable, at time.Time)) {
	t.Helper()

	// Phase 1: the comparison surface, every outcome.
	{
		table := newEnrolmentRequestTable()
		if seedCA {
			seedCAInto(t, table)
		}
		now := time.Now()
		table.setClock(func() time.Time { return now })

		legacy := genClientCSRPEM(t, "seam-legacy")
		if _, err := table.Lodge(legacy, "vm-legacy", "", "", "10.0.0.1:1"); err != nil {
			t.Fatalf("legacy lodge: %v", err)
		}

		good := newSASClient(t, "seam-good")
		gl, err := table.Lodge(good.csrPEM, "vm-good", "p_mail", good.commit, "10.0.0.2:1")
		assertNoErr(t, err, "lodge with a commitment")
		// An idempotent re-lodge: same CSR, same commitment, no new row.
		if _, err := table.Lodge(good.csrPEM, "vm-good", "p_mail", good.commit, "10.0.0.3:1"); err != nil {
			t.Fatalf("idempotent re-lodge: %v", err)
		}
		// A mismatched commitment for the same key is refused.
		other := newSASClient(t, "seam-good")
		if _, err := table.Lodge(good.csrPEM, "vm-good", "", other.commit, "10.0.0.4:1"); err == nil {
			t.Fatal("a re-lodge under a different commitment was accepted")
		}
		if _, err := table.Poll(gl.RequestID, good.open()); err != nil {
			t.Fatalf("poll with a good open: %v", err)
		}

		bad := newSASClient(t, "seam-bad")
		bl, err := table.Lodge(bad.csrPEM, "vm-bad", "", bad.commit, "10.0.0.5:1")
		assertNoErr(t, err, "lodge for a bad open")
		if _, err := table.Poll(bl.RequestID, otherNonce(t)); err == nil {
			t.Fatal("a poll opening the commitment wrongly was accepted")
		}
		// And a poll for a row that never existed.
		if _, err := table.Poll("req_nothing", ""); err != nil {
			t.Fatalf("poll for an unknown row: %v", err)
		}

		drive(table, now)
	}

	// Phase 2: the bounds — filled to the cap, refused past it, expired,
	// refilled, and fed malformed and oversized input.
	{
		table := newEnrolmentRequestTable()
		if seedCA {
			seedCAInto(t, table)
		}
		now := time.Now()
		table.setClock(func() time.Time { return now })

		fillAndOverflow := func(prefix string) {
			for i := 0; i < maxPendingEnrolmentRequests; i++ {
				addr := fmt.Sprintf("10.0.1.%d:1", i)
				if _, err := table.Lodge(genClientCSRPEM(t, fmt.Sprintf("%s-%d", prefix, i)), "", "", "", addr); err != nil {
					t.Fatalf("%s lodge %d: %v", prefix, i, err)
				}
			}
			if _, err := table.Lodge(genClientCSRPEM(t, prefix+"-overflow"), "", "", "", "10.0.2.1:1"); err == nil {
				t.Fatalf("%s: the overflow lodge was accepted", prefix)
			}
		}
		fillAndOverflow("fill1")
		drive(table, now)

		now = now.Add(enrolmentRequestTTL + time.Second)
		fillAndOverflow("fill2")
		drive(table, now)

		if _, err := table.Lodge([]byte("not a csr"), "", "", "", "10.0.3.1:1"); err == nil {
			t.Fatal("a malformed CSR was accepted")
		}
		if _, err := table.Lodge(genClientCSRPEM(t, "seam-label"), strings.Repeat("x", maxEnrolmentLabelBytes+1), "", "", "10.0.3.2:1"); err == nil {
			t.Fatal("an oversized label was accepted")
		}
		if _, err := table.Lodge(genClientCSRPEM(t, "seam-profile"), "", strings.Repeat("p", maxEnrolmentLabelBytes+1), "", "10.0.3.3:1"); err == nil {
			t.Fatal("an oversized requested_profile was accepted")
		}
		drive(table, now)
	}
}

// The behavioural half, extended past AC-29's own: it is no longer only the
// peer's WRITE that must not reach presence, but the tray's READ of what that
// write left behind. The tray now polls the table every two seconds, projects
// it for the panel, counts it for the menu bar and derives a notification
// from it — four new paths that run automatically, without an operator, on
// input a stranger controls. A real Recording provider is wired into a real
// EnrolmentOps and a real App, every one of those paths is driven against
// every state a peer can put a row into, and the provider must never once be
// touched.
func TestEnrolmentSeam_NoPresenceCallFromALodgeOrFromTheTrayReadingIt(t *testing.T) {
	store := newCLISandboxStore(t)
	recording := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(recording)
	assertNoErr(t, err, "NewGate")

	rp := &recordingPlatform{}
	settings := store.Get()

	seamPeerDrivenSurface(t, true, func(table *enrolmentRequestTable, at time.Time) {
		// A REAL gated core, holding the real gate, over this table: a
		// recording provider that nothing can reach proves nothing.
		ops := &EnrolmentOps{Store: store, Gate: gate, Audit: nil, Requests: table}
		app := &App{
			platform: rp,
			registry: &trayRegistry{},
			store:    store,
			extMgr:   NewExternalMcpManager(nil),
			ipcCtx:   &IPCContext{EnrolmentOps: ops},
		}

		// Every tray path that touches the table, in the order the poller
		// runs them.
		app.lastMenuJSON = ""
		app.updateMenuWithSettings(settings)
		app.settingsOpen.Store(true)
		app.pushEnrolmentRequests()
		app.lastMenuJSON = ""
		app.updateMenuWithSettings(settings)
		app.settingsOpen.Store(false)

		// And the projections the CLI and IPC doors read, including the one
		// that consults settings for a client-id suggestion.
		views := ops.PendingRequests()
		_ = countUnapprovedEnrolmentRequests(views)
		_ = pendingEnrolmentRequestViewsOf(views, settings)
		_ = ops.LodgeGeneration()
		for _, v := range views {
			if _, ok := table.Get(v.RequestID); !ok {
				t.Fatalf("a listed row %s is not gettable", v.RequestID)
			}
		}

		if n := recording.Calls(); n != 0 {
			t.Fatalf("the presence provider was called %d time(s) at %s — a lodge, or the tray reading one, "+
				"reached the prompt", n, at.Format(time.RFC3339))
		}
	})

	if n := recording.Calls(); n != 0 {
		t.Fatalf("the presence provider was called %d time(s) across the whole peer-driven surface, want 0", n)
	}
	// The gate is genuinely live: the same provider, asked by something that
	// IS allowed to ask, does reach it. Without this the assertions above
	// would keep passing against a gate wired to nothing, which is the one
	// way a guard like this rots into decoration.
	if _, err := gate.Require(context.Background(), "enrolment.sign",
		presence.Digest(sha256.Sum256([]byte("seam liveness check"))), "seam liveness check"); err != nil {
		t.Fatalf("the liveness check itself was refused: %v", err)
	}
	if recording.Calls() != 1 {
		t.Fatalf("the gate under test reaches no provider (calls=%d); every assertion above was vacuous", recording.Calls())
	}
}
