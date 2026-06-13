package main

// Coverage for the pure, deterministic tray logic: menu construction
// (updateMenuWithSettings), click routing (onMenuClick/toggleService), and the
// drain-then-kill shutdown ordering (cleanup). None of this touches Cocoa — it
// runs against a recording Platform and a fake ServiceManager — yet it was
// previously untested despite being exactly the off-by-one-prone (menu-ID →
// service-ID) and ordering-sensitive (orphan-prevention) code that breaks the
// tray silently.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// recordingPlatform captures menu/UI calls and runs DispatchToMain inline so a
// test can observe the effect of a tray action synchronously.
type recordingPlatform struct {
	mu       sync.Mutex
	menus    []string
	settings int
	urls     []string
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

func (p *recordingPlatform) menuCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.menus) }
func (p *recordingPlatform) lastMenu() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.menus) == 0 {
		return ""
	}
	return p.menus[len(p.menus)-1]
}

// trayRegistry is a ServiceManager that records lifecycle calls and lets a test
// control which services are "running". Embeds noopServiceManager for the
// methods the tray tests don't exercise.
type trayRegistry struct {
	noopServiceManager
	mu                 sync.Mutex
	running            map[string]bool
	started            []string
	stopped            []string
	stopAllCount       int
	closeFrontendCount int
}

func (r *trayRegistry) Start(c *ServiceConfig) error {
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
func (r *trayRegistry) CloseFrontendChannel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeFrontendCount++
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

	s := &Settings{Services: []ServiceConfig{
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
	s := &Settings{Services: []ServiceConfig{{ID: "x", DisplayName: "X"}}}

	app.updateMenuWithSettings(s)
	app.updateMenuWithSettings(s) // identical → must not re-push to the platform

	if n := rp.menuCount(); n != 1 {
		t.Fatalf("expected 1 menu push (second suppressed as no-op), got %d", n)
	}
}

func TestOnMenuClick_StartsStoppedService(t *testing.T) {
	rp := &recordingPlatform{}
	reg := &trayRegistry{}
	s := &Settings{Services: []ServiceConfig{{ID: "svc-x", DisplayName: "X", Command: "/bin/true"}}}
	app := &App{platform: rp, registry: reg, store: fixedStore{s: s}}
	app.updateMenuWithSettings(s) // populate svcMenuMap

	app.onMenuClick(menuIDSvcBase + 0) // not running → Start

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
	s := &Settings{Services: []ServiceConfig{{ID: "svc-x", DisplayName: "X"}}}
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
	s := &Settings{Services: []ServiceConfig{{ID: "svc-x", DisplayName: "X"}}}
	app := &App{platform: &recordingPlatform{}, registry: reg, store: fixedStore{s: s}}
	app.updateMenuWithSettings(s)

	app.onMenuClick(50) // not Settings(2), not Exit(3), below svc base(100)

	if len(reg.started) != 0 || len(reg.stopped) != 0 {
		t.Errorf("unexpected lifecycle calls: started=%v stopped=%v", reg.started, reg.stopped)
	}
}

func TestCleanup_IsIdempotentAndStopsServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg := &trayRegistry{}
	app := &App{
		ctx:      ctx,
		cancel:   cancel,
		extMgr:   NewExternalMcpManager(nil),
		registry: reg,
		platform: &recordingPlatform{},
	}

	app.cleanup()
	app.cleanup() // cleanupOnce → second call is a no-op

	if ctx.Err() == nil {
		t.Error("cleanup should cancel the app context")
	}
	if reg.stopAllCount != 1 {
		t.Errorf("StopAll called %d times, want exactly 1 (idempotent)", reg.stopAllCount)
	}
	if reg.closeFrontendCount != 1 {
		t.Errorf("CloseFrontendChannel called %d times, want exactly 1", reg.closeFrontendCount)
	}
}
