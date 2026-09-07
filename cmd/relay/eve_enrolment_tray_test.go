package main

// Tray-menu coverage for the eve passkey enrolment window
// (docs/eve-passkey-enrolment.md): the "Allow Eve Passkey Enrolment…" item
// sits right after "Show Login Code...", clicking it opens a window through
// the same core `relay eve enrol` uses, and the disabled countdown line
// appears only while a window is open and tracks Status(). Built directly
// on recordingPlatform/trayRegistry (trayapp_test.go), the same fake-tray
// harness the service-menu tests use.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

func eveTrayApp(t *testing.T) (*App, *recordingPlatform, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	p := &recordingPlatform{}
	app := &App{
		ctx:      context.Background(),
		store:    store,
		platform: p,
		registry: &trayRegistry{},
	}
	app.eveEnrolmentOps = &EveEnrolmentOps{
		Store:    store,
		Gate:     allowGate(t),
		Audit:    enabledIssuanceRecorder(t),
		OnChange: func() { app.updateMenu() },
		Notify:   p.Notify,
	}
	return app, p, store
}

type eveMenuItem struct {
	Title   string `json:"title"`
	ID      int    `json:"id"`
	Enabled bool   `json:"enabled"`
}

func eveParseMenu(t *testing.T, menuJSON string) []eveMenuItem {
	t.Helper()
	var items []eveMenuItem
	if err := json.Unmarshal([]byte(menuJSON), &items); err != nil {
		t.Fatalf("menu JSON: %v", err)
	}
	return items
}

func TestEveTrayMenu_OffersTheEnrolmentItemRightAfterLoginCode(t *testing.T) {
	app, _, _ := eveTrayApp(t)

	app.updateMenuWithSettings(&config.Settings{})
	items := eveParseMenu(t, app.lastMenuJSON)

	loginIdx, eveIdx := -1, -1
	for i, it := range items {
		switch it.Title {
		case "Show Login Code...":
			loginIdx = i
		case "Allow Eve Passkey Enrolment…":
			eveIdx = i
			if it.ID != menuIDEveEnrolment || !it.Enabled {
				t.Fatalf("eve-enrolment item = %+v, want id %d and enabled", it, menuIDEveEnrolment)
			}
		}
	}
	if loginIdx < 0 {
		t.Fatal("no login-code item in the parsed menu")
	}
	if eveIdx < 0 {
		t.Fatal("no eve-enrolment item in the parsed menu")
	}
	if eveIdx != loginIdx+1 {
		t.Fatalf("eve-enrolment item is at index %d, want immediately after login-code (index %d)", eveIdx, loginIdx)
	}
}

func TestEveTrayMenu_CountdownLineOnlyWhileOpen(t *testing.T) {
	app, _, store := eveTrayApp(t)

	app.updateMenuWithSettings(store.Get())
	if strings.Contains(app.lastMenuJSON, "Eve enrolment open") {
		t.Fatalf("countdown line present with no window open: %s", app.lastMenuJSON)
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: time.Now().UTC().Add(4*time.Minute + 12*time.Second).Format(time.RFC3339)}
	}), "seed an open window")
	app.updateMenuWithSettings(store.Get())
	if !strings.Contains(app.lastMenuJSON, "Eve enrolment open — 4m ") {
		t.Fatalf("no countdown line while a window is open: %s", app.lastMenuJSON)
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = nil
	}), "close the window")
	app.updateMenuWithSettings(store.Get())
	if strings.Contains(app.lastMenuJSON, "Eve enrolment open") {
		t.Fatalf("countdown line survives the window closing: %s", app.lastMenuJSON)
	}
}

// Clicking the item runs Open in a tracked goroutine (never on the calling
// thread — onMenuClick's own doc comment gives the reason in full for
// showLoginCode, which openEveEnrolment mirrors) and raises the console
// notification docs/eve-passkey-enrolment.md specifies.
func TestEveTrayMenuClick_OpensAWindowAndNotifies(t *testing.T) {
	app, p, store := eveTrayApp(t)

	app.onMenuClick(menuIDEveEnrolment)
	app.wg.Wait()

	if store.Get().EveEnrolment == nil {
		t.Fatal("clicking the menu item did not open a window")
	}
	found := false
	for _, n := range p.notes {
		if strings.Contains(n.body, "Eve passkey enrolment open for 5 minutes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no notification naming the open window; got %+v", p.notes)
	}
}
