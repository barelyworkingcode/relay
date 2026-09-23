package main

// Coverage for reordering services and hiding them from the tray menu:
// ServiceOps.Move / SetMenuHidden, their HTTP and IPC doors, and the tray
// menu builder's handling of order and the hidden flag. The property that
// runs through all of it: neither operation is a lifecycle event, so no
// Registry call, presence prompt or issuance record may follow from one.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// lifecycleSpy is a service.Manager that records every call that could
// start, stop or restart a service, including the bulk entry points
// svcRecorder leaves to noopServiceManager.
type lifecycleSpy struct {
	svcRecorder
	bulkMu sync.Mutex
	bulk   []string
}

func (r *lifecycleSpy) StartAllAutostart([]config.ServiceConfig) {
	r.bulkMu.Lock()
	defer r.bulkMu.Unlock()
	r.bulk = append(r.bulk, "StartAllAutostart")
}
func (r *lifecycleSpy) StopAll() {
	r.bulkMu.Lock()
	defer r.bulkMu.Unlock()
	r.bulk = append(r.bulk, "StopAll")
}
func (r *lifecycleSpy) ReclaimOrphans([]config.ServiceConfig) {
	r.bulkMu.Lock()
	defer r.bulkMu.Unlock()
	r.bulk = append(r.bulk, "ReclaimOrphans")
}

func (r *lifecycleSpy) assertNoLifecycleCalls(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	started, stopped, reloaded := slices.Clone(r.started), slices.Clone(r.stopped), slices.Clone(r.reloaded)
	r.mu.Unlock()
	r.bulkMu.Lock()
	bulk := slices.Clone(r.bulk)
	r.bulkMu.Unlock()
	if len(started)+len(stopped)+len(reloaded)+len(bulk) != 0 {
		t.Fatalf("reorder/hide touched the registry: started=%v stopped=%v reloaded=%v bulk=%v", started, stopped, reloaded, bulk)
	}
}

type menuOrderFixture struct {
	store    config.SettingsStore
	ops      *ServiceOps
	reg      *lifecycleSpy
	prompts  *presencetest.Recording
	onChange *int
}

// newMenuOrderOps seeds a, b, c (b running) plus the built-in session host
// record, and builds a ServiceOps whose Gate records every prompt and whose
// Issuance is nil: reaching either would show up as a prompt count or a
// fail-closed refusal.
func newMenuOrderOps(t *testing.T) menuOrderFixture {
	t.Helper()
	store := newCLISandboxStore(t)
	for _, svc := range []config.ServiceConfig{
		{ID: "a", DisplayName: "A", Command: "/bin/true"},
		{ID: "b", DisplayName: "B", Command: "/bin/true", Autostart: true},
		{ID: "c", DisplayName: "C", Command: "/bin/true"},
		{ID: config.RelaySessionsServiceID, DisplayName: "Session Host"},
	} {
		seedService(t, store, svc)
	}
	reg := &lifecycleSpy{svcRecorder: svcRecorder{running: map[string]bool{"b": true}}}
	prompts := presencetest.NewRecording(nil)
	gate, err := presence.NewGate(prompts)
	assertNoErr(t, err, "NewGate")
	changes := 0
	ops := &ServiceOps{Store: store, Registry: reg, Gate: gate, OnChange: func() { changes++ }}
	return menuOrderFixture{store: store, ops: ops, reg: reg, prompts: prompts, onChange: &changes}
}

func storedServiceIDs(store config.SettingsStore) []string {
	var ids []string
	for _, svc := range store.Get().Services {
		ids = append(ids, svc.ID)
	}
	return ids
}

func storedService(t *testing.T, store config.SettingsStore, id string) config.ServiceConfig {
	t.Helper()
	svc, _ := config.FindServiceByID(store.Get(), id)
	if svc == nil {
		t.Fatalf("service %q not in store", id)
	}
	return *svc
}

// ---------------------------------------------------------------------------
// ServiceOps
// ---------------------------------------------------------------------------

func TestServiceOpsMove_ReordersAndFiresOnChange(t *testing.T) {
	f := newMenuOrderOps(t)

	assertNoErr(t, f.ops.Move("c", 0), "Move(c, 0)")

	want := []string{"c", "a", "b", config.RelaySessionsServiceID}
	if got := storedServiceIDs(f.store); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if *f.onChange != 1 {
		t.Fatalf("OnChange fired %d times, want 1", *f.onChange)
	}
	f.reg.assertNoLifecycleCalls(t)
	if n := f.prompts.Calls(); n != 0 {
		t.Fatalf("Move reached the presence provider %d time(s), want 0", n)
	}
}

func TestServiceOpsMove_SessionHostRow(t *testing.T) {
	f := newMenuOrderOps(t)

	assertNoErr(t, f.ops.Move(config.RelaySessionsServiceID, 0), "Move(session host, 0)")

	if got := storedServiceIDs(f.store); got[0] != config.RelaySessionsServiceID {
		t.Fatalf("order = %v, want the session host first", got)
	}
	f.reg.assertNoLifecycleCalls(t)
}

func TestServiceOpsMove_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		index   int
		wantErr error
	}{
		{"unknown id", "ghost", 0, errServiceNotFound},
		{"negative index", "a", -1, errServiceInvalid},
		{"index equal to length", "a", 4, errServiceInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMenuOrderOps(t)
			before := storedServiceIDs(f.store)

			err := f.ops.Move(tc.id, tc.index)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Move(%q, %d) = %v, want errors.Is %v", tc.id, tc.index, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("error %q does not name the id %q", err, tc.id)
			}
			if got := storedServiceIDs(f.store); !slices.Equal(got, before) {
				t.Fatalf("refused move changed the order: %v, want %v", got, before)
			}
			if *f.onChange != 0 {
				t.Fatalf("OnChange fired on a refused move")
			}
			f.reg.assertNoLifecycleCalls(t)
		})
	}
}

func TestServiceOpsSetMenuHidden_HidesWithoutTouchingLifecycle(t *testing.T) {
	f := newMenuOrderOps(t)

	assertNoErr(t, f.ops.SetMenuHidden("b", true), "SetMenuHidden(b, true)")

	b := storedService(t, f.store, "b")
	if !b.HideFromMenu {
		t.Fatal("b not hidden")
	}
	if !b.Autostart {
		t.Fatal("hiding b cleared its autostart")
	}
	if !f.reg.IsRunning("b") {
		t.Fatal("hiding b stopped it")
	}
	if got := storedServiceIDs(f.store); !slices.Contains(got, "b") {
		t.Fatalf("hidden service unregistered: %v", got)
	}
	if *f.onChange != 1 {
		t.Fatalf("OnChange fired %d times, want 1", *f.onChange)
	}

	assertNoErr(t, f.ops.SetMenuHidden("b", false), "SetMenuHidden(b, false)")
	if storedService(t, f.store, "b").HideFromMenu {
		t.Fatal("b still hidden after SetMenuHidden(false)")
	}

	assertNoErr(t, f.ops.SetMenuHidden(config.RelaySessionsServiceID, true), "SetMenuHidden(session host)")
	if !storedService(t, f.store, config.RelaySessionsServiceID).HideFromMenu {
		t.Fatal("session host not hidden")
	}

	f.reg.assertNoLifecycleCalls(t)
	if n := f.prompts.Calls(); n != 0 {
		t.Fatalf("SetMenuHidden reached the presence provider %d time(s), want 0", n)
	}
}

func TestServiceOpsSetMenuHidden_UnknownID(t *testing.T) {
	f := newMenuOrderOps(t)

	err := f.ops.SetMenuHidden("ghost", true)
	if !errors.Is(err, errServiceNotFound) {
		t.Fatalf("SetMenuHidden(ghost) = %v, want errServiceNotFound", err)
	}
	if *f.onChange != 0 {
		t.Fatal("OnChange fired on a refused SetMenuHidden")
	}
	for _, svc := range f.store.Get().Services {
		if svc.HideFromMenu {
			t.Fatalf("%s hidden by a refused call", svc.ID)
		}
	}
}

// Editing a service in the Settings form must not un-hide it: the form
// carries no hide_from_menu field.
func TestServiceOpsUpdate_PreservesHideFromMenu(t *testing.T) {
	store := newCLISandboxStore(t)
	seedService(t, store, config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old", HideFromMenu: true})
	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Update(context.Background(), "svc1", serviceFields{
		DisplayName: "Svc1",
		Command:     "/bin/new",
	}, auditViaIPC, "")
	assertNoErr(t, err, "Update")

	got := storedService(t, store, "svc1")
	if got.Command != "/bin/new" {
		t.Fatalf("Update did not apply: %+v", got)
	}
	if !got.HideFromMenu {
		t.Fatal("Update cleared HideFromMenu")
	}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func doRawBody(t *testing.T, method, url, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	assertNoErr(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "%s %s", method, url)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	assertNoErr(t, err, "read body")
	return resp, out
}

func seedABC(t *testing.T, store config.SettingsStore) {
	t.Helper()
	for _, id := range []string{"a", "b", "c"} {
		seedService(t, store, config.ServiceConfig{ID: id, DisplayName: strings.ToUpper(id), Command: "/bin/true"})
	}
}

func TestServiceRoutesPosition_ReturnsNewOrder(t *testing.T) {
	reg := &lifecycleSpy{}
	changes := 0
	srv, store := newServiceRoutesServer(t, reg, func() { changes++ })
	defer srv.Close()
	seedABC(t, store)

	resp, body := doJSON(t, "PUT", srv.URL+"/api/services/c/position", map[string]any{"index": 0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var listed []map[string]any
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var ids []string
	for _, v := range listed {
		id, _ := v["id"].(string)
		ids = append(ids, id)
	}
	if want := []string{"c", "a", "b"}; !slices.Equal(ids, want) {
		t.Fatalf("response order = %v, want %v", ids, want)
	}
	if got := storedServiceIDs(store); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("stored order = %v", got)
	}
	if changes != 1 {
		t.Fatalf("OnChange fired %d times, want 1", changes)
	}
	reg.assertNoLifecycleCalls(t)
}

func TestServiceRoutesPosition_Errors(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"missing index", "/api/services/a/position", `{}`, http.StatusBadRequest},
		{"bad json", "/api/services/a/position", `{"index":`, http.StatusBadRequest},
		{"unknown id", "/api/services/ghost/position", `{"index":0}`, http.StatusNotFound},
		{"negative index", "/api/services/a/position", `{"index":-1}`, http.StatusBadRequest},
		{"index past end", "/api/services/a/position", `{"index":3}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &lifecycleSpy{}
			srv, store := newServiceRoutesServer(t, reg, nil)
			defer srv.Close()
			seedABC(t, store)

			resp, body := doRawBody(t, "PUT", srv.URL+tc.path, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d; body %s", resp.StatusCode, tc.want, body)
			}
			if got := storedServiceIDs(store); !slices.Equal(got, []string{"a", "b", "c"}) {
				t.Fatalf("refused request changed the order: %v", got)
			}
			reg.assertNoLifecycleCalls(t)
		})
	}
}

func TestServiceRoutesMenu_HidesAndShows(t *testing.T) {
	reg := &lifecycleSpy{svcRecorder: svcRecorder{running: map[string]bool{"b": true}}}
	changes := 0
	srv, store := newServiceRoutesServer(t, reg, func() { changes++ })
	defer srv.Close()
	seedABC(t, store)

	resp, body := doJSON(t, "PUT", srv.URL+"/api/services/b/menu", map[string]any{"hidden": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if view["id"] != "b" || view["hide_from_menu"] != true {
		t.Fatalf("view = %s, want id b with hide_from_menu true", body)
	}
	if !storedService(t, store, "b").HideFromMenu {
		t.Fatal("b not persisted as hidden")
	}
	if changes != 1 {
		t.Fatalf("OnChange fired %d times, want 1", changes)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/services", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []map[string]any
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list %s: %v", body, err)
	}
	for _, v := range listed {
		hidden := v["hide_from_menu"] == true
		if hidden != (v["id"] == "b") {
			t.Fatalf("list entry %v: hide_from_menu = %v", v["id"], v["hide_from_menu"])
		}
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/services/b/menu", map[string]any{"hidden": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("show: status %d, body %s", resp.StatusCode, body)
	}
	if storedService(t, store, "b").HideFromMenu {
		t.Fatal("b still hidden after hidden:false")
	}
	reg.assertNoLifecycleCalls(t)
}

func TestServiceRoutesMenu_UnknownID(t *testing.T) {
	srv, store := newServiceRoutesServer(t, &lifecycleSpy{}, nil)
	defer srv.Close()
	seedABC(t, store)

	resp, body := doJSON(t, "PUT", srv.URL+"/api/services/ghost/menu", map[string]any{"hidden": true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404; body %s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// IPC
// ---------------------------------------------------------------------------

func newMenuOrderIPC(t *testing.T) (*IPCContext, *recordingUI, config.SettingsStore, *lifecycleSpy, *int) {
	t.Helper()
	store := newCLISandboxStore(t)
	seedABC(t, store)
	reg := &lifecycleSpy{}
	ipc, ui := newServicesIPC(t, store, reg)
	changes := 0
	ipc.Ops.OnChange = func() { changes++ }
	return ipc, ui, store, reg, &changes
}

func dispatchIPC(t *testing.T, ipc *IPCContext, msgType string, payload any) {
	t.Helper()
	h, ok := ipcHandlers[msgType]
	if !ok {
		t.Fatalf("no IPC handler for %q", msgType)
	}
	h(ipc, mustJSON(t, payload))
}

func TestIPCMoveService(t *testing.T) {
	ipc, ui, store, reg, changes := newMenuOrderIPC(t)

	dispatchIPC(t, ipc, MsgMoveService, map[string]any{"id": "a", "index": 2})

	if got := storedServiceIDs(store); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("order = %v, want [b c a]", got)
	}
	if ui.hasEvent("onSettingsError") {
		t.Fatalf("unexpected onSettingsError: %s", lastSettingsError(t, ui))
	}
	if *changes != 1 {
		t.Fatalf("OnChange fired %d times, want 1", *changes)
	}
	reg.assertNoLifecycleCalls(t)
}

func TestIPCMoveService_Errors(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantID  string
	}{
		{"unknown id", map[string]any{"id": "ghost", "index": 0}, "ghost"},
		{"out of range", map[string]any{"id": "a", "index": 7}, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ipc, ui, store, reg, changes := newMenuOrderIPC(t)

			dispatchIPC(t, ipc, MsgMoveService, tc.payload)

			if msg := lastSettingsError(t, ui); !strings.Contains(msg, tc.wantID) {
				t.Fatalf("onSettingsError %q does not name %q", msg, tc.wantID)
			}
			if got := storedServiceIDs(store); !slices.Equal(got, []string{"a", "b", "c"}) {
				t.Fatalf("refused move changed the order: %v", got)
			}
			if *changes != 0 {
				t.Fatal("OnChange fired on a refused move")
			}
			reg.assertNoLifecycleCalls(t)
		})
	}
}

func TestIPCUpdateServiceMenuHidden(t *testing.T) {
	ipc, ui, store, reg, changes := newMenuOrderIPC(t)

	dispatchIPC(t, ipc, MsgUpdateServiceMenuHidden, map[string]any{"id": "c", "hidden": true})

	if !storedService(t, store, "c").HideFromMenu {
		t.Fatal("c not hidden")
	}
	if ui.hasEvent("onSettingsError") {
		t.Fatalf("unexpected onSettingsError: %s", lastSettingsError(t, ui))
	}
	if *changes != 1 {
		t.Fatalf("OnChange fired %d times, want 1", *changes)
	}

	dispatchIPC(t, ipc, MsgUpdateServiceMenuHidden, map[string]any{"id": "c", "hidden": false})
	if storedService(t, store, "c").HideFromMenu {
		t.Fatal("c still hidden after hidden:false")
	}
	reg.assertNoLifecycleCalls(t)
}

func TestIPCUpdateServiceMenuHidden_UnknownID(t *testing.T) {
	ipc, ui, _, _, changes := newMenuOrderIPC(t)

	dispatchIPC(t, ipc, MsgUpdateServiceMenuHidden, map[string]any{"id": "ghost", "hidden": true})

	if msg := lastSettingsError(t, ui); msg == "" {
		t.Fatal("empty onSettingsError")
	}
	if *changes != 0 {
		t.Fatal("OnChange fired on a refused call")
	}
}

// ---------------------------------------------------------------------------
// Tray menu
// ---------------------------------------------------------------------------

// menuTitlesBeforeSettings returns every title that precedes "Settings...",
// which is where the service block and its trailing separator live.
func menuTitlesBeforeSettings(t *testing.T, items []menuEntry) []string {
	t.Helper()
	var titles []string
	for _, it := range items {
		if it.ID == menuIDSettings {
			return titles
		}
		titles = append(titles, it.Title)
	}
	t.Fatal("menu has no Settings... item")
	return nil
}

// serviceRows drops separators from the pre-Settings block and fails unless
// the block ends in one, i.e. a non-empty service section is closed off.
func serviceRows(t *testing.T, items []menuEntry) []string {
	t.Helper()
	before := menuTitlesBeforeSettings(t, items)
	if len(before) == 0 || before[len(before)-1] != "-" {
		t.Fatalf("no separator ahead of Settings...: %q", before)
	}
	var rows []string
	for _, title := range before {
		if title != "-" {
			rows = append(rows, title)
		}
	}
	return rows
}

func assertNoAdjacentSeparators(t *testing.T, items []menuEntry) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		if items[i].Title == "-" && items[i-1].Title == "-" {
			t.Fatalf("adjacent separators at %d/%d in menu %+v", i-1, i, items)
		}
	}
}

func TestUpdateMenuWithSettings_FollowsStoredOrder(t *testing.T) {
	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}}

	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "z", DisplayName: "Zed"},
		{ID: "a", DisplayName: "Alpha"},
		{ID: "m", DisplayName: "Mid"},
	}})
	got := serviceRows(t, parseMenu(t, rp.lastMenu()))
	if want := []string{"Zed", "Alpha", "Mid"}; !slices.Equal(got, want) {
		t.Fatalf("service block = %q, want %q", got, want)
	}

	// A reorder reaches the menu on the next rebuild, no restart involved.
	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "m", DisplayName: "Mid"},
		{ID: "z", DisplayName: "Zed"},
		{ID: "a", DisplayName: "Alpha"},
	}})
	got = serviceRows(t, parseMenu(t, rp.lastMenu()))
	if want := []string{"Mid", "Zed", "Alpha"}; !slices.Equal(got, want) {
		t.Fatalf("reordered service block = %q, want %q", got, want)
	}
}

func TestUpdateMenuWithSettings_SkipsHiddenServices(t *testing.T) {
	rp := &recordingPlatform{}
	reg := &trayRegistry{running: map[string]bool{"b": true}}
	app := &App{platform: rp, registry: reg}

	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "a", DisplayName: "Alpha"},
		{ID: "b", DisplayName: "Bravo", HideFromMenu: true},
		{ID: "c", DisplayName: "Charlie"},
	}})

	items := parseMenu(t, rp.lastMenu())
	got := serviceRows(t, items)
	if want := []string{"Alpha", "Charlie"}; !slices.Equal(got, want) {
		t.Fatalf("service block = %q, want %q", got, want)
	}
	for _, it := range items {
		if strings.Contains(it.Title, "Bravo") {
			t.Fatalf("hidden service appears in menu: %+v", it)
		}
	}
	for menuID, svcID := range app.svcMenuMap {
		if svcID == "b" {
			t.Fatalf("hidden service b is clickable via svcMenuMap[%d]", menuID)
		}
	}
	// Each visible row's click still resolves to its own service.
	for _, it := range items {
		if it.Title == "Alpha" && app.svcMenuMap[it.ID] != "a" {
			t.Fatalf("Alpha row id %d maps to %q", it.ID, app.svcMenuMap[it.ID])
		}
		if it.Title == "Charlie" && app.svcMenuMap[it.ID] != "c" {
			t.Fatalf("Charlie row id %d maps to %q", it.ID, app.svcMenuMap[it.ID])
		}
	}
	if len(reg.started)+len(reg.stopped)+reg.stopAllCount != 0 {
		t.Fatalf("building the menu touched the registry: started=%v stopped=%v", reg.started, reg.stopped)
	}
}

func TestUpdateMenuWithSettings_SessionHostHidden(t *testing.T) {
	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{running: map[string]bool{config.RelaySessionsServiceID: true}}}

	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: config.RelaySessionsServiceID, DisplayName: "Session Host", HideFromMenu: true},
		{ID: "a", DisplayName: "Alpha"},
	}})

	items := parseMenu(t, rp.lastMenu())
	for _, it := range items {
		if strings.Contains(it.Title, "Session Host") {
			t.Fatalf("hidden session host row appears in menu: %+v", it)
		}
	}
	if got, want := serviceRows(t, items), []string{"Alpha"}; !slices.Equal(got, want) {
		t.Fatalf("service block = %q, want %q", got, want)
	}
}

func TestUpdateMenuWithSettings_SessionHostReordered(t *testing.T) {
	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}}

	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "a", DisplayName: "Alpha"},
		{ID: config.RelaySessionsServiceID, DisplayName: "Session Host"},
		{ID: "b", DisplayName: "Bravo"},
	}})

	got := serviceRows(t, parseMenu(t, rp.lastMenu()))
	if len(got) != 3 || got[0] != "Alpha" || !strings.HasPrefix(got[1], "Session Host") || got[2] != "Bravo" {
		t.Fatalf("service rows = %q, want [Alpha, Session Host (...), Bravo]", got)
	}
}

func TestUpdateMenuWithSettings_AllHiddenLeavesNoServiceSection(t *testing.T) {
	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{running: map[string]bool{"a": true}}}

	app.updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "a", DisplayName: "Alpha", HideFromMenu: true},
		{ID: config.RelaySessionsServiceID, DisplayName: "Session Host", HideFromMenu: true},
	}})

	items := parseMenu(t, rp.lastMenu())
	// With no service rows, the only thing ahead of Settings... is the
	// separator that always precedes it; a second one would be the stray
	// separator the service block leaves behind.
	got := menuTitlesBeforeSettings(t, items)
	if want := []string{"-"}; !slices.Equal(got, want) {
		t.Fatalf("items before Settings... = %q, want %q", got, want)
	}
	if len(app.svcMenuMap) != 0 {
		t.Fatalf("svcMenuMap = %v, want empty", app.svcMenuMap)
	}
	assertNoAdjacentSeparators(t, items)
}

// The same menu with no services at all, as a baseline the all-hidden case
// must match exactly.
func TestUpdateMenuWithSettings_AllHiddenMatchesNoServices(t *testing.T) {
	hiddenPlat := &recordingPlatform{}
	(&App{platform: hiddenPlat, registry: &trayRegistry{}}).updateMenuWithSettings(&config.Settings{Services: []config.ServiceConfig{
		{ID: "a", DisplayName: "Alpha", HideFromMenu: true},
	}})
	emptyPlat := &recordingPlatform{}
	(&App{platform: emptyPlat, registry: &trayRegistry{}}).updateMenuWithSettings(&config.Settings{})

	if hiddenPlat.lastMenu() != emptyPlat.lastMenu() {
		t.Fatalf("all-hidden menu differs from the no-services menu:\nhidden: %s\nempty:  %s", hiddenPlat.lastMenu(), emptyPlat.lastMenu())
	}
}
