package main

import (
	"errors"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// TestUpdateMenuWithSettings_SurfacesDegradedSealedStore is S7's tray-level
// half of §5.6 clause 1: "the tray must show this state rather than failing
// obscurely." A degraded store must not be discoverable only by opening
// Settings — the menu itself names the exact refusal reason, the same text
// SealStatus() reports and ResolveSealedStore names (§5.6's table).
func TestUpdateMenuWithSettings_SurfacesDegradedSealedStore(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	reason := errors.New("the sealed store is bound to key aaaaaaaaaaaaaaaa, settings.json expects key bbbbbbbbbbbbbbbb")
	store := config.NewSettingsStoreDegraded(dir, reason)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized on a degraded store")

	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}, store: store}
	app.updateMenuWithSettings(store.Get())

	menu := rp.lastMenu()
	if !strings.Contains(menu, "bound to key aaaaaaaaaaaaaaaa") || !strings.Contains(menu, "expects key bbbbbbbbbbbbbbbb") {
		t.Fatalf("degraded menu does not name the reason: %s", menu)
	}

	items := parseMenu(t, menu)
	want := "⚠ Sealed store: " + store.SealStatus().Error()
	for _, it := range items {
		if it.Title == want {
			return
		}
	}
	t.Fatalf("no disabled menu line names the degraded reason verbatim (want %q): %s", want, menu)
}

// TestUpdateMenuWithSettings_NoDegradedLineWhenHealthy is the negative
// control: a working sealed store must not show the warning line at all —
// otherwise every menu build would carry dead noise.
func TestUpdateMenuWithSettings_NoDegradedLineWhenHealthy(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}, store: store}
	app.updateMenuWithSettings(store.Get())

	if strings.Contains(rp.lastMenu(), "Sealed store:") {
		t.Fatalf("a healthy store's menu names a degraded reason: %s", rp.lastMenu())
	}
}

// TestUpdateMenuWithSettings_AlwaysOffersReset is §5.6 clause 5: the tray
// always offers the break-glass, not only once already degraded — an
// operator who wants to start over does not have to wait for a failure
// first, and AC-25d's "no second door" is about CLI/env/flag doors, not
// about hiding the one door that IS meant to exist.
func TestUpdateMenuWithSettings_AlwaysOffersReset(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	rp := &recordingPlatform{}
	app := &App{platform: rp, registry: &trayRegistry{}, store: store}
	app.updateMenuWithSettings(store.Get())

	items := parseMenu(t, rp.lastMenu())
	for _, it := range items {
		if it.ID == menuIDResetSealedStore {
			return
		}
	}
	t.Fatal("menu has no Reset Sealed Store item")
}
