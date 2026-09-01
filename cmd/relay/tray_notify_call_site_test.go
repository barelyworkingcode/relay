package main

// The notifier's CALL SITE, not its arithmetic — tray_notify_test.go already
// owns the rate limiter's bounds. What is pinned here is everything the
// notifier cannot pin about itself: that the arguments come from the same
// read the tray's own "Pending enrolment requests: N" line comes from, that
// nothing an unauthenticated peer typed reaches the banner, that clicking the
// banner opens a window rather than reaching a gate, and that the click lands
// on the page the banner is about.
//
// The claim that exactly ONE place drives the notifier lives with the other
// structural guards for this feature, in enrolment_sas_seam_test.go.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/dop251/goja"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// notifierTrayApp builds the smallest App that updateMenuWithSettings will
// run against, with the notifier's clock and sink under the test's control.
// The sandbox comes first: the menu build reads settings, and the headline
// testing rule is that no test touches the real config dir.
func notifierTrayApp(t *testing.T) (*App, *recordingPlatform, *enrolmentRequestTable, *fakeClock) {
	t.Helper()
	mkEmptySandboxRelayHome(t)
	rp := &recordingPlatform{}
	table := newEnrolmentRequestTable()
	s := &config.Settings{}
	clk := newFakeClock()
	app := &App{
		platform: rp,
		registry: &trayRegistry{},
		store:    fixedStore{s: s},
		extMgr:   mcpbroker.NewManager(nil),
		ipcCtx:   &IPCContext{EnrolmentOps: &EnrolmentOps{Requests: table}},
	}
	app.enrolNotifier = newPendingEnrolmentNotifier(clk.now, rp.Notify)
	return app, rp, table, clk
}

// rebuildMenu forces a repaint: the poller suppresses an unchanged menu JSON,
// and a test that did not clear the cache would observe the previous string.
func (a *App) rebuildMenu() {
	a.lastMenuJSON = ""
	a.updateMenuWithSettings(a.store.Get())
}

func (p *recordingPlatform) notifications() []notifyCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]notifyCall(nil), p.notes...)
}

// ---------------------------------------------------------------------------
// AC-33 (the call site) — the arguments are the ones the menu line uses
// ---------------------------------------------------------------------------

// Every bound in tray_notify.go is a function of what the call site passes.
// This drives the real tray path and checks each argument by its observable
// effect, so a call site that hard-coded `false` for settingsOpen, or passed
// the row count where the unapproved count belongs, fails here rather than in
// production a month later.
func TestUpdateMenuWithSettings_DrivesTheNotifierFromTheSamePendingRead(t *testing.T) {
	app, rp, table, clk := notifierTrayApp(t)

	// Nothing pending: no line, no banner.
	app.rebuildMenu()
	if n := len(rp.notifications()); n != 0 {
		t.Fatalf("an empty table produced %d notifications", n)
	}

	l1, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "Lodge 1")
	app.rebuildMenu()
	notes := rp.notifications()
	if len(notes) != 1 {
		t.Fatalf("one new request produced %d notifications, want 1", len(notes))
	}
	if !strings.Contains(notes[0].body, "1 machine is waiting") {
		t.Errorf("the banner does not carry the count the menu line carries: %q", notes[0].body)
	}
	if !strings.Contains(rp.lastMenu(), "Pending enrolment requests: 1") {
		t.Errorf("the menu line disagrees with the banner: %s", rp.lastMenu())
	}

	// A repaint with no new row moves no generation, so it says nothing —
	// this is what keeps a 2-second poll from being a 2-second banner.
	clk.advance(2 * time.Hour)
	app.rebuildMenu()
	if n := len(rp.notifications()); n != 1 {
		t.Fatalf("a repaint with nothing new produced %d notifications, want 1", n)
	}

	// The Settings window is the panel that already updates live, so a
	// banner over it is noise. Proves settingsOpen reaches the notifier.
	app.settingsOpen.Store(true)
	if _, err := table.Lodge(genClientCSRPEM(t, "hermes-cal"), "", "", "", "10.0.0.6:1"); err != nil {
		t.Fatalf("Lodge 2: %v", err)
	}
	clk.advance(2 * time.Hour)
	app.rebuildMenu()
	if n := len(rp.notifications()); n != 1 {
		t.Fatalf("a lodge while Settings was open produced %d notifications, want 1", n)
	}
	app.settingsOpen.Store(false)

	// An approved row still needs no decision, so it is not counted — the
	// unapproved count, not the row count, is what reaches the notifier.
	table.MarkApproved(l1.RequestID, "hermes-mail", nil, "127.0.0.1:9910", "cert", "ca")
	app.rebuildMenu()
	if !strings.Contains(rp.lastMenu(), "Pending enrolment requests: 1") {
		t.Fatalf("approving one row did not drop the count: %s", rp.lastMenu())
	}

	// Fill the table to its cap. A full table refuses lodges, so there is
	// nothing new to approve and no banner is raised for the last slot.
	for i := len(table.List()); i < maxPendingEnrolmentRequests; i++ {
		// A distinct source host each time: the per-source throttle would
		// otherwise refuse the second filler and the table would never fill.
		addr := fmt.Sprintf("10.0.1.%d:1", i)
		_, err := table.Lodge(genClientCSRPEM(t, "filler"), "", "", "", addr)
		assertNoErr(t, err, "filler lodge %d", i)
	}
	clk.advance(2 * time.Hour)
	before := len(rp.notifications())
	app.rebuildMenu()
	if n := len(rp.notifications()); n != before {
		t.Fatalf("a full table produced %d new notifications, want 0", n-before)
	}
}

// The notifier is built on first use rather than at App construction, so the
// tray's own path (an App that never had one installed) must still notify.
func TestUpdateMenuWithSettings_BuildsTheNotifierWhenNoneWasInstalled(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	rp := &recordingPlatform{}
	table := newEnrolmentRequestTable()
	s := &config.Settings{}
	app := &App{
		platform: rp,
		registry: &trayRegistry{},
		store:    fixedStore{s: s},
		extMgr:   mcpbroker.NewManager(nil),
		ipcCtx:   &IPCContext{EnrolmentOps: &EnrolmentOps{Requests: table}},
	}

	if _, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "", "", "10.0.0.5:1"); err != nil {
		t.Fatalf("Lodge: %v", err)
	}
	app.rebuildMenu()
	if n := len(rp.notifications()); n != 1 {
		t.Fatalf("an App with no pre-installed notifier raised %d notifications, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// AC-34 — the banner carries no attacker-supplied text and no code
// ---------------------------------------------------------------------------

// The label and the requested profile are hostile input from an
// unauthenticated peer; a notification banner is not a place to render either.
// The SAS is withheld too — it belongs beside the refuse button and the key
// hash, and it frequently does not exist yet when the banner fires.
func TestPendingEnrolmentNotification_CarriesNoAttackerSuppliedTextAndNoCode(t *testing.T) {
	app, rp, table, _ := notifierTrayApp(t)
	seedCAInto(t, table)

	// The longest strings the server will accept, in the charset it accepts.
	hostileLabel := strings.Repeat("L", maxEnrolmentLabelBytes)
	hostileProfile := strings.Repeat("P", maxEnrolmentLabelBytes)

	c := newSASClient(t, "hostile")
	l, err := table.Lodge(c.csrPEM, hostileLabel, hostileProfile, c.commit, "10.0.0.5:1")
	assertNoErr(t, err, "Lodge with a commitment")
	if _, err := table.Poll(l.RequestID, c.open()); err != nil {
		t.Fatalf("Poll with a good open: %v", err)
	}
	view := viewFor(t, table, l.RequestID)
	if !view.SASReady || view.SAS == "" {
		t.Fatalf("fixture did not produce a ready comparison code: %+v", view)
	}

	app.rebuildMenu()
	notes := rp.notifications()
	if len(notes) != 1 {
		t.Fatalf("got %d notifications, want 1", len(notes))
	}
	text := notes[0].title + "\n" + notes[0].body
	for _, banned := range []struct{ what, value string }{
		{"the machine-supplied label", hostileLabel},
		{"the machine-supplied requested profile", hostileProfile},
		{"the comparison code", view.SAS},
		{"the request id", l.RequestID},
	} {
		if strings.Contains(text, banned.value) {
			t.Errorf("the notification carries %s:\n%s", banned.what, text)
		}
	}
	if !strings.Contains(text, "1 machine is waiting") {
		t.Errorf("the notification does not carry the one thing it may carry — a count:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// AC-35 — clicking the notification opens a window and nothing else
// ---------------------------------------------------------------------------

// externalFieldNames is every struct field in package main whose declared
// type is qualified (pkg.Type) — a field holding a value from another
// package. A call through one of those (a.wg.Add, a.settingsOpen.Store) is
// not a call to anything declared here, and resolving it by bare method name
// is how `a.wg.Add(1)` would otherwise read as a call to McpOps.Add.
func externalFieldNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]bool{}
	var isQualified func(ast.Expr) bool
	isQualified = func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.StarExpr:
			return isQualified(x.X)
		case *ast.ArrayType:
			return isQualified(x.Elt)
		case *ast.SelectorExpr:
			return true
		case *ast.IndexExpr: // atomic.Pointer[T]
			return isQualified(x.X)
		}
		return false
	}
	for _, name := range gateASTFiles(t, root) {
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		assertNoErr(t, err, "parse %s", name)
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, fld := range st.Fields.List {
				if !isQualified(fld.Type) {
					continue
				}
				for _, id := range fld.Names {
					out[id.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// callGraph maps a declaration ("Recv.Method", or a bare function name) to
// every name it calls. A method call is keyed by its bare selector name and
// resolved against every receiver that declares it: an over-approximation,
// and deliberately the safe direction — this guard proves something is NOT
// reachable, so reaching too much can only make it stricter. The two edges it
// does NOT follow are the ones that resolve to nothing here at all: a call
// qualified by an imported package name, and a call through a struct field
// whose type comes from another package.
func callGraph(t *testing.T, root string) (edges map[string][]string, byName map[string][]string) {
	t.Helper()
	fset := token.NewFileSet()
	external := externalFieldNames(t, root)
	edges = map[string][]string{}
	byName = map[string][]string{}
	for _, name := range gateASTFiles(t, root) {
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		assertNoErr(t, err, "parse %s", name)
		imports := map[string]bool{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = true
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := enclosingMethodName(fd)
			byName[fd.Name.Name] = append(byName[fd.Name.Name], key)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					edges[key] = append(edges[key], fn.Name)
				case *ast.SelectorExpr:
					if id, ok := fn.X.(*ast.Ident); ok && imports[id.Name] {
						return true
					}
					if sel, ok := fn.X.(*ast.SelectorExpr); ok && external[sel.Sel.Name] {
						return true
					}
					edges[key] = append(edges[key], fn.Sel.Name)
				}
				return true
			})
		}
	}
	return edges, byName
}

// reachableFrom walks the call graph from one declaration, returning every
// declaration key it can reach.
func reachableFrom(start string, edges, byName map[string][]string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, callee := range edges[cur] {
			for _, decl := range byName[callee] {
				if !seen[decl] {
					seen[decl] = true
					queue = append(queue, decl)
				}
			}
		}
	}
	return seen
}

// The banner's click handler and the tray line's click handler do the same
// thing and nothing else: put the Settings window on the Remote Clients page.
// Neither carries a request id, and neither can reach a presence prompt — an
// unauthenticated peer that can raise a banner must not be able to raise a
// dialog by getting the operator to click it.
func TestNotificationClick_ReachesAWindowAndNoGatedMethod(t *testing.T) {
	root := gateASTModuleRoot(t)
	edges, byName := callGraph(t, root)

	gated := map[string]bool{}
	for _, site := range scanRequireGateCallSites(t, root) {
		gated[site.method] = true
	}
	if len(gated) == 0 {
		t.Fatal("found no requireGate call sites at all; the scan is misconfigured and would pass vacuously")
	}

	for _, entry := range []string{"App.onNotificationClick", "App.openRemoteClientsPage"} {
		if _, ok := byName[strings.SplitN(entry, ".", 2)[1]]; !ok {
			t.Fatalf("%s is not declared in package main; the scan is misconfigured", entry)
		}
		reached := reachableFrom(entry, edges, byName)
		if !reached["App.openSettingsWindow"] {
			t.Errorf("%s does not reach openSettingsWindow — clicking must open the window that can show the request", entry)
		}
		if !reached["App.emitSettingsEvent"] {
			t.Errorf("%s does not reach emitSettingsEvent — clicking must select the Remote Clients page", entry)
		}
		for method := range gated {
			if reached[method] {
				t.Errorf("%s can reach %s, which calls requireGate: a click on a banner an unauthenticated peer caused must not be able to raise a presence prompt", entry, method)
			}
		}
	}
}

// The two click paths are the same act, so they are one function. A second
// implementation is how they drift into two different behaviours.
func TestNotificationClickAndTrayLineClick_AreTheSameAct(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	rp := &recordingPlatform{}
	app := &App{
		platform: rp,
		registry: &trayRegistry{},
		store:    fixedStore{s: &config.Settings{}},
		extMgr:   mcpbroker.NewManager(nil),
	}

	app.onMenuClick(menuIDPendingEnrolments)
	afterMenu := rp.settings
	app.onSettingsClose()
	app.onNotificationClick()

	if afterMenu != 1 {
		t.Fatalf("the tray line opened Settings %d times, want 1", afterMenu)
	}
	if rp.settings != 2 {
		t.Fatalf("the notification click opened Settings %d times in total, want 2", rp.settings)
	}
}

// ---------------------------------------------------------------------------
// Arriving on Remote Clients — the banner's whole point
// ---------------------------------------------------------------------------

// A window that is not up yet has no document to receive an emit, so the page
// rides in on the first paint. Landing the operator on Services after they
// clicked a banner asking them to approve something is landing them nowhere.
func TestOpenRemoteClientsPage_ColdOpenSeedsThePageIntoTheFirstPaint(t *testing.T) {
	app, p, _ := ilTrayApp(t)

	app.onNotificationClick()

	doc := p.lastDoc(t)
	if !strings.Contains(doc, `initialPage: "remote"`) {
		t.Fatalf("a cold open did not seed the Remote Clients page into its first paint")
	}
	// The emit is not the channel here and must not be relied on: at the
	// moment it would fire, the WebView has no document.
	if strings.Contains(p.allJS(), "showPage") {
		t.Errorf("a cold open tried to reach the window through an emit: %s", p.allJS())
	}
}

// The seed is single-use, exactly like a minted login code: every later
// "Settings..." click must open where it always did.
func TestOpenRemoteClientsPage_TheSeededPageIsConsumedByTheOpenItServed(t *testing.T) {
	app, p, _ := ilTrayApp(t)

	app.onNotificationClick()
	app.onSettingsClose()
	app.openSettingsWindow()

	if got := p.lastDoc(t); !strings.Contains(got, `initialPage: ""`) {
		t.Fatal("a page seeded for one open survived into the next one")
	}
}

// A window that IS up is never reloaded by OpenSettings, so for that one the
// emit is the only channel — the mirror image of the cold path above.
func TestOpenRemoteClientsPage_WarmOpenSelectsThePageThroughAnEmit(t *testing.T) {
	app, p, _ := ilTrayApp(t)
	app.openSettingsWindow()

	app.onMenuClick(menuIDPendingEnrolments)

	if !strings.Contains(p.allJS(), `showPage("remote")`) {
		t.Fatalf("a warm open did not select Remote Clients: %s", p.allJS())
	}
	// And nothing is left seeded behind it, or the NEXT cold open would land
	// on Remote Clients for a request nobody mentioned.
	if got := p.lastDoc(t); !strings.Contains(got, `initialPage: ""`) {
		t.Fatalf("a warm open left a page seeded for the next window")
	}
}

// The page id crossing the Go/JS boundary is one string, and it is the one
// web/src/app.js's showPage actually branches on.
func TestSettingsBoot_HonoursTheSeededInitialPage(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(domShim); err != nil {
		t.Fatalf("dom shim: %v", err)
	}
	if _, err := vm.RunString(`window.__RELAY_INIT__.initialPage = ` + strconv.Quote(settingsPageRemoteClients) + `;`); err != nil {
		t.Fatalf("seeding initialPage: %v", err)
	}
	if _, err := vm.RunString(bundleForTest(t, "web/src/entry.js", "")); err != nil {
		t.Fatalf("app bundle threw on load: %v", err)
	}

	if got := evalString(t, vm, `window.state.page`); got != settingsPageRemoteClients {
		t.Fatalf("the window booted on page %q, want %q", got, settingsPageRemoteClients)
	}
	if got := evalString(t, vm, `document.getElementById('content').innerHTML.indexOf('Remote Clients') >= 0`); got != "true" {
		t.Fatal("the window booted on the Remote Clients page id but painted something else")
	}
}

// With nothing seeded, the boot is exactly what it was: no page forced, and a
// minted login code still wins the tab.
func TestSettingsBoot_UnseededBootIsUnchanged(t *testing.T) {
	vm := newAppVM(t)
	if got := evalString(t, vm, `window.state.page`); got == settingsPageRemoteClients {
		t.Fatalf("an unseeded boot forced the Remote Clients page")
	}
}
