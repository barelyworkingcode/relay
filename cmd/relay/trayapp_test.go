package main

// Coverage for the pure, deterministic tray logic: menu construction
// (updateMenuWithSettings), click routing (onMenuClick/toggleService), and the
// drain-then-kill shutdown ordering (cleanup). None of this touches Cocoa — it
// runs against a recording Platform and a fake service.Manager — yet it was
// previously untested despite being exactly the off-by-one-prone (menu-ID →
// service-ID) and ordering-sensitive (orphan-prevention) code that breaks the
// tray silently.

import (
	"context"
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingPlatform captures menu/UI calls and runs DispatchToMain inline so a
// test can observe the effect of a tray action synchronously.
type recordingPlatform struct {
	mu       sync.Mutex
	menus    []string
	settings int
	urls     []string
	notes    []notifyCall
}

func (p *recordingPlatform) Init()                      {}
func (p *recordingPlatform) Run()                       {}
func (p *recordingPlatform) SetupTray([]byte, int, int) {}
func (p *recordingPlatform) UpdateMenu(menuJSON string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.menus = append(p.menus, menuJSON)
}
func (p *recordingPlatform) OpenSettings(string)      { p.mu.Lock(); p.settings++; p.mu.Unlock() }
func (p *recordingPlatform) EvalSettingsJS(string)    {}
func (p *recordingPlatform) DispatchToMain(fn func()) { fn() }
func (p *recordingPlatform) OpenURL(u string)         { p.mu.Lock(); p.urls = append(p.urls, u); p.mu.Unlock() }
func (p *recordingPlatform) Notify(title, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notes = append(p.notes, notifyCall{title, body})
}

func (p *recordingPlatform) menuCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.menus) }
func (p *recordingPlatform) lastMenu() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.menus) == 0 {
		return ""
	}
	return p.menus[len(p.menus)-1]
}

// trayRegistry is a service.Manager that records lifecycle calls and lets a test
// control which services are "running". Embeds noopServiceManager for the
// methods the tray tests don't exercise.
type trayRegistry struct {
	noopServiceManager
	mu           sync.Mutex
	running      map[string]bool
	started      []string
	stopped      []string
	stopAllCount int
}

func (r *trayRegistry) Start(c *config.ServiceConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, c.ID)
	if r.running == nil {
		r.running = map[string]bool{}
	}
	r.running[c.ID] = true
	return nil
}
func (r *trayRegistry) Stop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, id)
	delete(r.running, id)
}
func (r *trayRegistry) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id]
}
func (r *trayRegistry) PIDsByServiceID() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	pid := 1000
	for id, on := range r.running {
		if on {
			out[id] = pid
			pid++
		}
	}
	return out
}
func (r *trayRegistry) StopAll() { r.mu.Lock(); defer r.mu.Unlock(); r.stopAllCount++ }
func (r *trayRegistry) Runtime() map[string]service.ServiceRuntime {
	return map[string]service.ServiceRuntime{}
}

// menuEntry mirrors the fields updateMenuWithSettings marshals.
type menuEntry struct {
	Title string `json:"title"`
	ID    int    `json:"id"`
	On    bool   `json:"on"`
	URL   string `json:"url"`
}

func parseMenu(t *testing.T, jsonStr string) []menuEntry {
	t.Helper()
	var items []menuEntry
	if err := json.Unmarshal([]byte(jsonStr), &items); err != nil {
		t.Fatalf("unmarshal menu JSON: %v\n%s", err, jsonStr)
	}
	return items
}

func TestUpdateMenuWithSettings_BuildsServiceItemsAndMapping(t *testing.T) {
	rp := &recordingPlatform{}
	reg := &trayRegistry{running: map[string]bool{"svc-b": true}} // only B running
	app := &App{platform: rp, registry: reg}

	s := &config.Settings{Services: []config.ServiceConfig{
		{ID: "svc-a", DisplayName: "Service A", URL: "http://a.local"},
		{ID: "svc-b", DisplayName: "Service B"},
	}}
	app.updateMenuWithSettings(s)

	// Menu-ID → service-ID map must resolve by stable ID, not positional drift.
	if got := app.svcMenuMap[menuIDSvcBase+0]; got != "svc-a" {
		t.Errorf("svcMenuMap[base+0] = %q, want svc-a", got)
	}
	if got := app.svcMenuMap[menuIDSvcBase+1]; got != "svc-b" {
		t.Errorf("svcMenuMap[base+1] = %q, want svc-b", got)
	}

	items := parseMenu(t, rp.lastMenu())
	byID := map[int]menuEntry{}
	for _, it := range items {
		byID[it.ID] = it
	}
	// Running state reflected in the toggle dot.
	if byID[menuIDSvcBase+0].On {
		t.Error("svc-a should not show as running")
	}
	if !byID[menuIDSvcBase+1].On {
		t.Error("svc-b should show as running")
	}
	// Service URL carried through.
	if byID[menuIDSvcBase+0].URL != "http://a.local" {
		t.Errorf("svc-a URL = %q, want http://a.local", byID[menuIDSvcBase+0].URL)
	}
	// Settings + Exit always present.
	if _, ok := byID[menuIDSettings]; !ok {
		t.Error("menu missing Settings item")
	}
	if _, ok := byID[menuIDExit]; !ok {
		t.Error("menu missing Exit item")
	}
}

func TestUpdateMenuWithSettings_SuppressesNoOpUpdate(t *testing.T) {
	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}}
	s := &config.Settings{Services: []config.ServiceConfig{{ID: "x", DisplayName: "X"}}}

	app.updateMenuWithSettings(s)
	app.updateMenuWithSettings(s) // identical → must not re-push to the platform

	if n := rp.menuCount(); n != 1 {
		t.Fatalf("expected 1 menu push (second suppressed as no-op), got %d", n)
	}
}

func TestOnMenuClick_StartsStoppedService(t *testing.T) {
	rp := &recordingPlatform{}
	reg := &trayRegistry{}
	s := &config.Settings{Services: []config.ServiceConfig{{ID: "svc-x", DisplayName: "X", Command: "/bin/true"}}}
	app := &App{platform: rp, registry: reg, store: fixedStore{s: s}}
	app.updateMenuWithSettings(s) // populate svcMenuMap

	app.onMenuClick(menuIDSvcBase + 0) // not running → Start (in a tracked goroutine)
	app.wg.Wait()

	if len(reg.started) != 1 || reg.started[0] != "svc-x" {
		t.Fatalf("expected Start(svc-x), got started=%v", reg.started)
	}
	if len(reg.stopped) != 0 {
		t.Errorf("did not expect any Stop, got %v", reg.stopped)
	}
}

func TestOnMenuClick_StopsRunningService(t *testing.T) {
	rp := &recordingPlatform{}
	reg := &trayRegistry{running: map[string]bool{"svc-x": true}}
	s := &config.Settings{Services: []config.ServiceConfig{{ID: "svc-x", DisplayName: "X"}}}
	app := &App{platform: rp, registry: reg, store: fixedStore{s: s}}
	app.updateMenuWithSettings(s)

	app.onMenuClick(menuIDSvcBase + 0) // running → Stop (in a tracked goroutine)
	app.wg.Wait()

	if len(reg.stopped) != 1 || reg.stopped[0] != "svc-x" {
		t.Fatalf("expected Stop(svc-x), got stopped=%v", reg.stopped)
	}
	if len(reg.started) != 0 {
		t.Errorf("did not expect any Start, got %v", reg.started)
	}
}

func TestOnMenuClick_NonServiceIDDoesNotToggle(t *testing.T) {
	// An ID between the fixed items and the service base (e.g. a stale or
	// unknown ID) must not start or stop anything. menuIDExit is deliberately
	// not exercised here because its handler calls os.Exit.
	reg := &trayRegistry{}
	s := &config.Settings{Services: []config.ServiceConfig{{ID: "svc-x", DisplayName: "X"}}}
	app := &App{platform: &recordingPlatform{}, registry: reg, store: fixedStore{s: s}}
	app.updateMenuWithSettings(s)

	app.onMenuClick(50) // not Settings(2), not Exit(3), below svc base(100)

	if len(reg.started) != 0 || len(reg.stopped) != 0 {
		t.Errorf("unexpected lifecycle calls: started=%v stopped=%v", reg.started, reg.stopped)
	}
}

func TestCleanup_IsIdempotentAndStopsServices(t *testing.T) {
	mkEmptySandboxRelayHome(t)

	ctx, cancel := context.WithCancel(context.Background())
	reg := &trayRegistry{}
	frontendChannel := NewFrontendChannel()
	ep, err := frontendChannel.Ensure()
	if err != nil {
		t.Fatalf("Ensure frontend channel: %v", err)
	}
	// Ensure only reserves the path; simulate a live socket so Close's
	// removal is observable.
	if err := os.WriteFile(ep.Socket, nil, 0600); err != nil {
		t.Fatalf("seed frontend socket file: %v", err)
	}
	app := &App{
		ctx:             ctx,
		cancel:          cancel,
		extMgr:          mcpbroker.NewManager(nil),
		registry:        reg,
		platform:        &recordingPlatform{},
		frontendChannel: frontendChannel,
	}

	app.cleanup()
	app.cleanup() // cleanupOnce → second call is a no-op

	if ctx.Err() == nil {
		t.Error("cleanup should cancel the app context")
	}
	if reg.stopAllCount != 1 {
		t.Errorf("StopAll called %d times, want exactly 1 (idempotent)", reg.stopAllCount)
	}
	if _, err := os.Stat(ep.Socket); !os.IsNotExist(err) {
		t.Errorf("frontend socket should be removed by cleanup, stat err=%v", err)
	}
}

// TestWaitWithTimeout_ReturnsFalseWhenWaitGroupNeverFinishes is item 7's
// core-mechanism test: a WaitGroup that never reaches zero (standing in for
// a goFunc-tracked goroutine permanently stuck dispatch_sync'ing to the
// very main thread that is waiting on it) must not hang waitWithTimeout
// forever.
func TestWaitWithTimeout_ReturnsFalseWhenWaitGroupNeverFinishes(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1) // never Done()

	start := time.Now()
	if waitWithTimeout(&wg, 50*time.Millisecond) {
		t.Fatal("expected false for a WaitGroup that never reaches zero")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("returned before the timeout elapsed: %v", elapsed)
	}
}

// TestWaitWithTimeout_ReturnsTrueWhenWaitGroupFinishesInTime is the control
// case: a WaitGroup that finishes well inside the bound reports true, so
// cleanup()'s ordinary (non-deadlocked) shutdown path is unaffected.
func TestWaitWithTimeout_ReturnsTrueWhenWaitGroupFinishesInTime(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()
	if !waitWithTimeout(&wg, time.Second) {
		t.Fatal("expected true when the group finishes before the deadline")
	}
}

// TestCleanup_DoesNotHangOnATrackedGoroutineThatNeverFinishes reproduces
// item 7's tray shutdown deadlock at the App level: cleanup() itself calls
// a.wg.Wait() as its last step, and a goFunc-tracked goroutine that never
// returns (in production: ResetMcpPermissions stuck dispatch_sync'ing to
// the same main thread cleanup() is running on) must not turn cleanup()
// into a permanent hang. Runs for roughly cleanupWaitGroupTimeout.
func TestCleanup_DoesNotHangOnATrackedGoroutineThatNeverFinishes(t *testing.T) {
	mkEmptySandboxRelayHome(t)

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		extMgr:   mcpbroker.NewManager(nil),
		registry: &trayRegistry{},
		platform: &recordingPlatform{},
	}
	app.goFunc(func() { select {} }) // never returns

	done := make(chan struct{})
	go func() {
		app.cleanup()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(cleanupWaitGroupTimeout + 3*time.Second):
		t.Fatal("cleanup() did not return within its bounded wait for tracked goroutines")
	}
}

// TestUpdateMenuWithSettings_ReflectsPendingEnrolmentRequestCount is the
// tray's whole surface for the enrolment-request channel (spec §2): a
// count built from the SAME table EnrolmentOps.PendingRequests reads, shown
// only when there is something pending, and a click that reaches nothing
// but Settings.
func TestUpdateMenuWithSettings_ReflectsPendingEnrolmentRequestCount(t *testing.T) {
	// updateMenuWithSettings -> renderSettingsDocument -> remoteConfigViewOf
	// reads ca.crt off disk (enrolment.CAFingerprintFromDisk); sandbox first so that
	// read never touches the real ConfigDir (headline testing rule).
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

	app.updateMenuWithSettings(s)
	if strings.Contains(rp.lastMenu(), "Pending enrolment requests") {
		t.Fatalf("menu shows a pending line with nothing pending: %s", rp.lastMenu())
	}

	l1, err := table.Lodge(genClientCSRPEM(t, "hermes-mail"), "", "", "", "10.0.0.5:1")
	assertNoErr(t, err, "Lodge 1")
	if _, err := table.Lodge(genClientCSRPEM(t, "hermes-cal"), "", "", "", "10.0.0.6:1"); err != nil {
		t.Fatalf("Lodge 2: %v", err)
	}
	// The poller suppresses a repaint whose JSON is unchanged from the last
	// push; force one so this assertion observes the new count rather than
	// the cached string from the call above.
	app.lastMenuJSON = ""
	app.updateMenuWithSettings(s)
	if !strings.Contains(rp.lastMenu(), "Pending enrolment requests: 2") {
		t.Fatalf("menu does not show 2 pending requests: %s", rp.lastMenu())
	}

	// Approving one drops the count to 1: an approved-but-not-yet-collected
	// row needs nothing further from this menu (countUnapprovedEnrolmentRequests's
	// own doc comment) — it is real and already on disk, per spec §2.
	table.MarkApproved(l1.RequestID, "hermes-mail", nil, "127.0.0.1:9910", "cert", "ca")
	app.lastMenuJSON = ""
	app.updateMenuWithSettings(s)
	if !strings.Contains(rp.lastMenu(), "Pending enrolment requests: 1") {
		t.Fatalf("menu does not show 1 pending request after an approval: %s", rp.lastMenu())
	}

	// A click on the line opens Settings and nothing else: no request id is
	// carried on this path, and no gated core method is anywhere near it —
	// the structural answer to "a network peer cannot spam prompts" (spec §2).
	app.onMenuClick(menuIDPendingEnrolments)
	if rp.settings != 1 {
		t.Fatalf("clicking the pending line opened Settings %d times, want 1", rp.settings)
	}
}
