package main

// Hermetic tests for the Passkeys IPC handlers and the tray item that mints a
// bootstrap code. Mirrors the CLI coverage in login_cmd_test.go: every handler
// proves it (a) emits the right event, (b) persists through the SettingsStore,
// and (c) refuses what login_ops.go refuses, with login_ops.go's wording.
//
// Two are load-bearing. TestILPasskeyHandlers_NeverEmitKeyMaterial is the
// counterpart of the enrolment suite's key-material test: a passkey record
// holds an ES256 public key, and no byte of it may reach a WebView from any
// handler or from the first paint. And
// TestILRevokePasskey_DoesNotEndTheSessionsItSignedIn pins the fact the whole
// tab is built around — a revoked passkey stops the next login and does
// nothing to a credential it already minted.
//
// What is NOT covered here: the Cocoa half. NSMenu item construction, the
// WKWebView, and the click that reaches goOnMenuClick are outside the hermetic
// tier (docs/testing.md, "Not covered by the suite"). Everything on the Go side of
// that boundary is — the menu JSON, App.showLoginCode, and the document handed
// to Platform.OpenSettings.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/dop251/goja"
)

// ilPlatform captures what the tray hands Cocoa. recordingPlatform in
// trayapp_test.go counts OpenSettings calls without keeping the document, and
// the whole question here is what the document contains.
type ilPlatform struct {
	mu   sync.Mutex
	docs []string
	js   []string
}

func (p *ilPlatform) Init()                      {}
func (p *ilPlatform) Run()                       {}
func (p *ilPlatform) SetupTray([]byte, int, int) {}
func (p *ilPlatform) UpdateMenu(string)          {}
func (p *ilPlatform) OpenSettings(html string) {
	p.mu.Lock()
	p.docs = append(p.docs, html)
	p.mu.Unlock()
}
func (p *ilPlatform) EvalSettingsJS(js string) { p.mu.Lock(); p.js = append(p.js, js); p.mu.Unlock() }
func (p *ilPlatform) DispatchToMain(fn func()) { fn() }
func (p *ilPlatform) OpenURL(string)           {}
func (p *ilPlatform) Notify(string, string)    {}

func (p *ilPlatform) lastDoc(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.docs) == 0 {
		t.Fatal("no settings document was opened")
	}
	return p.docs[len(p.docs)-1]
}

func (p *ilPlatform) allJS() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.js, "\n")
}

// ilIPC stands up an IPCContext over a sandboxed store, matching
// newEnrolmentIPC. Nothing here can touch the real config dir.
func ilIPC(t *testing.T) (*IPCContext, config.SettingsStore, *recordingUI) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	ui := &recordingUI{}
	return &IPCContext{
		Ctx:                    context.Background(),
		Store:                  store,
		UI:                     ui,
		Platform:               stubPlatform{},
		Registry:               noopServiceManager{},
		Enhanced:               NewEnhancedServiceRegistry(nil),
		UpdateMenu:             func() {},
		PushServiceStatusBatch: func() {},
		GoFunc:                 func(fn func()) { fn() },
		NotifyReconcile:        func(string) error { return nil },
		NotifyReloadMcp:        func(string, string) error { return nil },
		LoginOps:               &LoginOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)},
		EvePasskeyOps:          &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)},
	}, store, ui
}

// ilXMarker / ilYMarker stand in for the public key coordinates. They are
// chosen to be recognisable in any encoding a leak could take.
const (
	ilXMarker = "AAAAPASSKEYXCOORDAAAA"
	ilYMarker = "BBBBPASSKEYYCOORDBBBB"
)

func ilSeedPasskey(t *testing.T, store config.SettingsStore, id, name string, signCount uint32, counters bool) {
	t.Helper()
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Passkeys = append(s.Passkeys, config.Passkey{
			ID:               id,
			Name:             name,
			X:                []byte(ilXMarker),
			Y:                []byte(ilYMarker),
			SignCount:        signCount,
			CounterSupported: counters,
			UserHandle:       "relay-owner",
			Created:          "2026-08-20T09:14:00Z",
		})
	}), "seed passkey %s", id)
}

// ilSeedSession mints a credential named the way the ceremony names one, so
// the sign-out gate sees exactly what it would see in production.
func ilSeedSession(t *testing.T, store config.SettingsStore, credentialID string, ttl time.Duration) config.APICredential {
	t.Helper()
	var cred config.APICredential
	var mintErr error
	assertNoErr(t, store.With(func(s *config.Settings) {
		cred, _, mintErr = mintAPICredentialFor(s, loginCredentialName(credentialID), loginCredentialClasses, ttl)
	}), "seed login session")
	assertNoErr(t, mintErr, "seed login session")
	return cred
}

func ilDecodeViews[T any](t *testing.T, arg interface{}) []T {
	t.Helper()
	raw, ok := arg.(json.RawMessage)
	if !ok {
		t.Fatalf("event argument was %T, want json.RawMessage", arg)
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", string(raw), err)
	}
	return out
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// The first-run state. It must emit two empty ARRAYS, not null and not a
// missing argument: the tab's empty state — the one that tells an operator
// where a login code comes from — is what renders off them, and a null would
// take the render down instead of showing it.
func TestILListPasskeys_NoneRegistered(t *testing.T) {
	ipc, _, ui := ilIPC(t)

	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))

	args, ok := findEvent(ui, "onPasskeysReloaded")
	if !ok {
		t.Fatalf("expected onPasskeysReloaded; got %+v", ui.events)
	}
	if len(args) != 3 {
		t.Fatalf("onPasskeysReloaded carried %d arguments, want passkeys, sessions and eve passkeys", len(args))
	}
	for i, label := range []string{"passkeys", "sessions", "eve passkeys"} {
		raw, isRaw := args[i].(json.RawMessage)
		if !isRaw {
			t.Fatalf("%s argument was %T, want json.RawMessage", label, args[i])
		}
		if string(raw) != "[]" {
			t.Errorf("%s with none registered = %s, want an empty array", label, raw)
		}
	}
}

// Several registered: every one is listed, the name and creation time are
// carried, and the credential id is ABBREVIATED for display while the full one
// travels as the revoke handle.
func TestILListPasskeys_SeveralRegistered(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	const longID = "AAAABBBBCCCCDDDDEEEEFFFF"
	ilSeedPasskey(t, store, longID, "MacBook Touch ID", 0, false)
	ilSeedPasskey(t, store, "short-id", "YubiKey 5C", 7, true)

	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))

	args, ok := findEvent(ui, "onPasskeysReloaded")
	if !ok {
		t.Fatalf("expected onPasskeysReloaded; got %+v", ui.events)
	}
	views := ilDecodeViews[passkeyView](t, args[0])
	if len(views) != 2 {
		t.Fatalf("listed %d passkeys, want 2: %+v", len(views), views)
	}
	if views[0].Name != "MacBook Touch ID" || views[1].Name != "YubiKey 5C" {
		t.Errorf("names did not survive the projection: %+v", views)
	}
	if views[0].Created != "2026-08-20T09:14:00Z" {
		t.Errorf("created = %q, want the stored value", views[0].Created)
	}
	if views[1].SignCount != 7 || !views[1].CounterSupported {
		t.Errorf("sign count and counter support did not survive: %+v", views[1])
	}
	// The abbreviation is what reaches the screen; a value long enough to be
	// mistaken for a secret must not.
	if views[0].Short != abbreviatePasskeyID(longID) || views[0].Short == longID {
		t.Errorf("short = %q, want the abbreviated form of %q", views[0].Short, longID)
	}
	// And the full id still travels, because revoke is keyed by it.
	if views[0].ID != longID {
		t.Errorf("id = %q, want the full credential id — revoke could not address this row", views[0].ID)
	}
}

// An expired session is left out. It already authenticates nothing, so a
// sign-out button beside it would offer to undo something already undone.
func TestILListPasskeys_OmitsExpiredSessions(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	live := ilSeedSession(t, store, "cred-live", loginCredentialTTL)
	dead := ilSeedSession(t, store, "cred-dead", time.Hour)
	assertNoErr(t, store.With(func(s *config.Settings) {
		findAPICredential(s, dead.ID).Expires = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	}), "age the second session out")

	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))

	args, _ := findEvent(ui, "onPasskeysReloaded")
	sessions := ilDecodeViews[loginSessionView](t, args[1])
	if len(sessions) != 1 || sessions[0].ID != live.ID {
		t.Fatalf("sessions = %+v, want only the live one (%s)", sessions, live.ID)
	}
}

// ---------------------------------------------------------------------------
// Revoke
// ---------------------------------------------------------------------------

func TestILRevokePasskey_DeletesTheRecordAndNamesIt(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	ilSeedPasskey(t, store, "cred-keep", "YubiKey 5C", 3, true)
	ilSeedPasskey(t, store, "cred-drop", "Old Laptop", 0, false)

	ipcRevokePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "cred-drop"}))

	args, ok := findEvent(ui, "onPasskeyRevoked")
	if !ok {
		t.Fatalf("expected onPasskeyRevoked; got %+v", ui.events)
	}
	if got, _ := args[0].(string); got != "cred-drop" {
		t.Errorf("revoked event named id %q, want cred-drop", got)
	}
	// The name rides along: the confirmation the operator reads afterwards
	// has to say what was cut, and the record is gone by then.
	if got, _ := args[1].(string); got != "Old Laptop" {
		t.Errorf("revoked event named %q, want the passkey's name", got)
	}

	remaining := store.Get().Passkeys
	if len(remaining) != 1 || remaining[0].ID != "cred-keep" {
		t.Fatalf("after revoke the store holds %+v, want only cred-keep", remaining)
	}
}

func TestILRevokePasskey_UnknownIDIsRefusedByName(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	ilSeedPasskey(t, store, "cred-keep", "YubiKey 5C", 3, true)

	ipcRevokePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "ghost"}))

	args, ok := findEvent(ui, "onPasskeyError")
	if !ok {
		t.Fatalf("expected onPasskeyError; got %+v", ui.events)
	}
	msg, _ := args[0].(string)
	if !strings.Contains(msg, "ghost") {
		t.Errorf("refusal must name the unknown id; got: %s", msg)
	}
	if _, revoked := findEvent(ui, "onPasskeyRevoked"); revoked {
		t.Error("a refused revoke announced a revocation")
	}
	if len(store.Get().Passkeys) != 1 {
		t.Error("a refused revoke changed the passkey list")
	}
}

// The fact the whole tab exists to state. Revoking a passkey stops the NEXT
// login; a credential that passkey already minted is a separate record with
// its own twelve-hour lifetime and keeps authenticating. If this ever becomes
// false the tab's revoke confirmation is lying in the other direction.
func TestILRevokePasskey_DoesNotEndTheSessionsItSignedIn(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	ilSeedPasskey(t, store, "cred-drop", "Old Laptop", 0, false)
	session := ilSeedSession(t, store, "cred-drop", loginCredentialTTL)

	ipcRevokePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "cred-drop"}))

	if _, ok := findEvent(ui, "onPasskeyRevoked"); !ok {
		t.Fatalf("setup: the revoke did not land; got %+v", ui.events)
	}
	if findAPICredential(store.Get(), session.ID) == nil {
		t.Fatal("revoking a passkey deleted a credential it had already minted")
	}
	if authenticateAPICredential(store.Get(), "") != nil {
		t.Fatal("setup: the empty token authenticated")
	}
	// Still listed, so the operator can see the session that survived and end
	// it deliberately.
	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))
	args, _ := findEvent(ui, "onPasskeysReloaded")
	sessions := ilDecodeViews[loginSessionView](t, args[1])
	if len(sessions) != 1 || sessions[0].ID != session.ID {
		t.Fatalf("sessions after revoking the passkey = %+v, want the surviving one", sessions)
	}
}

// ---------------------------------------------------------------------------
// Eve passkeys (docs/eve-passkey-enrolment.md's second half)
// ---------------------------------------------------------------------------

func TestILListPasskeys_IncludesTheEveMirrorAsAThirdArgument(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "eve-1", Label: "iPhone", Created: "2026-09-07T10:00:00Z"}}
	}), "seed an eve passkey")

	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))

	args, ok := findEvent(ui, "onPasskeysReloaded")
	if !ok {
		t.Fatalf("expected onPasskeysReloaded; got %+v", ui.events)
	}
	views := ilDecodeViews[evePasskeyView](t, args[2])
	if len(views) != 1 || views[0].ID != "eve-1" || views[0].Label != "iPhone" {
		t.Fatalf("eve passkeys argument = %+v, want the seeded mirror", views)
	}
}

func TestILRevokeEvePasskey_RecordsAPendingRevocationAndNamesTheID(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "eve-keep"}, {ID: "eve-drop"}}
	}), "seed two eve passkeys")

	ipcRevokeEvePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "eve-drop"}))

	args, ok := findEvent(ui, "onEvePasskeyRevoked")
	if !ok {
		t.Fatalf("expected onEvePasskeyRevoked; got %+v", ui.events)
	}
	if got, _ := args[0].(string); got != "eve-drop" {
		t.Errorf("revoked event named id %q, want eve-drop", got)
	}

	revs := store.Get().EvePasskeyRevocations
	if len(revs) != 1 || revs[0].ID != "eve-drop" {
		t.Fatalf("no pending revocation was written: %+v", revs)
	}
	// Unlike relay's own RevokePasskey, this never deletes the mirror entry
	// -- eve's own report is the acknowledgement (decision 12).
	if len(store.Get().EvePasskeys) != 2 {
		t.Fatalf("revoking pruned the mirror directly: %+v", store.Get().EvePasskeys)
	}
}

func TestILRevokeEvePasskey_UnknownIDIsRefusedByName(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "eve-keep"}, {ID: "eve-other"}}
	}), "seed two eve passkeys")

	ipcRevokeEvePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "ghost"}))

	args, ok := findEvent(ui, "onPasskeyError")
	if !ok {
		t.Fatalf("expected onPasskeyError; got %+v", ui.events)
	}
	msg, _ := args[0].(string)
	if !strings.Contains(msg, "ghost") {
		t.Errorf("refusal must name the unknown id; got: %s", msg)
	}
	if _, revoked := findEvent(ui, "onEvePasskeyRevoked"); revoked {
		t.Error("a refused revoke announced a revocation")
	}
	if len(store.Get().EvePasskeyRevocations) != 0 {
		t.Error("a refused revoke wrote a pending record anyway")
	}
}

// ---------------------------------------------------------------------------
// Sign out
// ---------------------------------------------------------------------------

func TestILSignOutLogin_RevokesOneSessionAndLeavesTheRest(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	keep := ilSeedSession(t, store, "cred-a", loginCredentialTTL)
	drop := ilSeedSession(t, store, "cred-b", loginCredentialTTL)

	ipcSignOutLogin(ipc, mustRaw(t, map[string]interface{}{"id": drop.ID}))

	args, ok := findEvent(ui, "onLoginSessionRevoked")
	if !ok {
		t.Fatalf("expected onLoginSessionRevoked; got %+v", ui.events)
	}
	if got, _ := args[0].(string); got != drop.ID {
		t.Errorf("signed-out event named %q, want %q", got, drop.ID)
	}
	if findAPICredential(store.Get(), drop.ID) != nil {
		t.Error("the signed-out credential survived")
	}
	if findAPICredential(store.Get(), keep.ID) == nil {
		t.Error("signing one browser out took another with it")
	}
}

// The gate. This door signs a BROWSER out; it is not a general "revoke any
// control-plane credential" button. An operator's long-lived credential
// reached from a WebView would be exactly the widening ADR-015 decision 4
// exists to prevent, and the refusal names the CLI that does own that act.
func TestILSignOutLogin_RefusesACredentialThatIsNotALoginSession(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	script, _, err := mintAPICredential(store, credentialMintRequest{Name: "nightly-rotator", Classes: []string{"grant"}})
	assertNoErr(t, err, "mint the operator's own credential")

	ipcSignOutLogin(ipc, mustRaw(t, map[string]interface{}{"id": script.ID}))

	args, ok := findEvent(ui, "onPasskeyError")
	if !ok {
		t.Fatalf("expected onPasskeyError; got %+v", ui.events)
	}
	msg, _ := args[0].(string)
	for _, want := range []string{"nightly-rotator", "not a browser login session", "relay credential revoke"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must contain %q; got: %s", want, msg)
		}
	}
	if findAPICredential(store.Get(), script.ID) == nil {
		t.Fatal("a credential that is not a login session was revoked from the settings WebView")
	}
	if _, revoked := findEvent(ui, "onLoginSessionRevoked"); revoked {
		t.Error("a refused sign-out announced a revocation")
	}
}

// A record under the reserved legacy name is refused too — a WebView must not
// be able to remove a record only relay's own start may delete.
//
// This is deliberate: the property is defended twice and no single change
// falsifies it. The name does not begin loginCredentialPrefix, so SignOut's
// own gate refuses it; and revokeAPICredentialIf refuses the reserved name to
// every caller regardless. Removing either alone leaves the test passing,
// which is the point of asserting it separately from
// TestILSignOutLogin_RefusesACredentialThatIsNotALoginSession.
func TestILSignOutLogin_RefusesTheLegacyFrontendCredential(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		addAPICredential(s, config.APICredential{ID: "il-legacy-id", Name: legacyFrontendCredentialName, Hash: config.HashToken("il-legacy-token"), Classes: frontendConsumerClasses})
	}), "seed the legacy credential")
	legacyID := store.Get().APICredentials[0].ID

	ipcSignOutLogin(ipc, mustRaw(t, map[string]interface{}{"id": legacyID}))

	if _, ok := findEvent(ui, "onPasskeyError"); !ok {
		t.Fatalf("expected onPasskeyError; got %+v", ui.events)
	}
	if findAPICredential(store.Get(), legacyID) == nil {
		t.Fatal("the legacy frontend credential was revoked from the settings WebView")
	}
}

// ---------------------------------------------------------------------------
// Key material
// ---------------------------------------------------------------------------

// The one that matters. A passkey record holds an ES256 public key; not one
// byte of it may ride out on an IPC event, in any field, on any argument, from
// any handler — nor on the first paint, which is the other way this data
// reaches the page. Checked in both the raw and base64 spellings because
// []byte marshals as base64 and a leak would arrive wearing that.
func TestILPasskeyHandlers_NeverEmitKeyMaterial(t *testing.T) {
	ipc, store, ui := ilIPC(t)
	ilSeedPasskey(t, store, "cred-a", "YubiKey 5C", 3, true)
	ilSeedPasskey(t, store, "cred-b", "Old Laptop", 0, false)
	session := ilSeedSession(t, store, "cred-a", loginCredentialTTL)

	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))
	ipcRevokePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "cred-b"}))
	ipcRevokePasskey(ipc, mustRaw(t, map[string]interface{}{"id": "ghost"}))
	ipcSignOutLogin(ipc, mustRaw(t, map[string]interface{}{"id": session.ID}))
	ipcListPasskeys(ipc, mustRaw(t, map[string]interface{}{"type": MsgListPasskeys}))

	surfaces := map[string]string{
		"IPC events":  emittedJSON(t, ui),
		"first paint": renderSettingsDocument(store.Get(), nil, nil, nil, nil, "", overviewSeed{}),
	}
	needles := []string{
		ilXMarker, ilYMarker,
		base64.StdEncoding.EncodeToString([]byte(ilXMarker)),
		base64.StdEncoding.EncodeToString([]byte(ilYMarker)),
		base64.RawURLEncoding.EncodeToString([]byte(ilXMarker)),
		base64.RawURLEncoding.EncodeToString([]byte(ilYMarker)),
	}
	for surface, text := range surfaces {
		for _, needle := range needles {
			if strings.Contains(text, needle) {
				t.Fatalf("public key material reached %s as %q", surface, needle)
			}
		}
		// A credential hash verifies a live secret and a user handle is a
		// registration input; neither is key material and neither has any
		// business on a rendering surface, for the reason
		// `relay credential list` prints no hash either.
		for _, c := range []string{`"x":`, `"y":`, `"hash":`, `"user_handle":`} {
			if strings.Contains(text, c) {
				t.Errorf("%s carries a %s field", surface, c)
			}
		}
	}
}

// The first paint carries the same two lists the IPC view does, so the tab is
// correct before any message is exchanged — and carries no code when none was
// minted, which is every ordinary open.
func TestILRenderSettingsDocument_SeedsPasskeysAndSessions(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	const id = "AAAABBBBCCCCDDDDEEEE"
	ilSeedPasskey(t, store, id, "MacBook Touch ID", 0, false)
	session := ilSeedSession(t, store, id, loginCredentialTTL)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "eve-first-paint", Label: "iPhone"}}
	}), "seed an eve passkey")

	html := renderSettingsDocument(store.Get(), nil, nil, nil, nil, "", overviewSeed{})

	for _, want := range []string{"MacBook Touch ID", abbreviatePasskeyID(id), session.Name, "loginCode: null", "iPhone", abbreviatePasskeyID("eve-first-paint")} {
		if !strings.Contains(html, want) {
			t.Errorf("first paint is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// The tray item
// ---------------------------------------------------------------------------

func ilTrayApp(t *testing.T) (*App, *ilPlatform, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	p := &ilPlatform{}
	app := &App{
		ctx:      context.Background(),
		store:    store,
		platform: p,
		registry: &trayRegistry{},
		extMgr:   mcpbroker.NewManager(nil),
	}
	app.loginOps = &LoginOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
	return app, p, store
}

func TestILTrayMenu_OffersTheLoginCodeItem(t *testing.T) {
	app, _, _ := ilTrayApp(t)

	app.updateMenuWithSettings(&config.Settings{})

	menu := app.lastMenuJSON
	if !strings.Contains(menu, "Show Login Code...") {
		t.Fatalf("the tray menu offers no way to get a login code: %s", menu)
	}
	var items []struct {
		Title   string `json:"title"`
		ID      int    `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(menu), &items); err != nil {
		t.Fatalf("menu JSON: %v", err)
	}
	for _, it := range items {
		if it.Title != "Show Login Code..." {
			continue
		}
		if it.ID != menuIDLoginCode || !it.Enabled {
			t.Fatalf("login-code item = %+v, want id %d and enabled", it, menuIDLoginCode)
		}
		return
	}
	t.Fatal("no login-code item in the parsed menu")
}

// With the window closed the code has to arrive in the first paint: Cocoa
// drops a script evaluated against a WebView that does not exist yet.
func TestILTrayLoginCode_ClosedWindowGetsTheCodeInItsFirstPaint(t *testing.T) {
	app, p, store := ilTrayApp(t)

	app.onMenuClick(menuIDLoginCode)
	app.wg.Wait()

	doc := p.lastDoc(t)
	anchor := store.Get().LoginBootstrap
	if anchor == nil {
		t.Fatal("no bootstrap anchor was written")
	}
	code := ilCodeFromDocument(t, doc)
	if code == "" {
		t.Fatalf("the opened document carries no login code")
	}
	// Only the SHA-256 is stored — the document holds the one copy of the
	// plaintext that will ever exist.
	if config.HashToken(code) != anchor.Hash {
		t.Fatal("the code shown is not the code the anchor will accept")
	}
	if strings.Contains(mustMarshalJSON("anchor", anchor), code) {
		t.Fatal("the plaintext code was persisted")
	}
	if !strings.Contains(doc, anchor.Expires) {
		t.Errorf("the document does not show when the code expires")
	}
	if js := p.allJS(); strings.Contains(js, "onLoginCodeMinted") {
		t.Errorf("a closed window was sent an emit that Cocoa would drop: %s", js)
	}
}

// With the window already open there is no reload, so the emit is the only
// channel — and the code must not also be left behind to reappear in the next
// window's first paint.
func TestILTrayLoginCode_OpenWindowGetsAnEmitAndNothingIsLeftBehind(t *testing.T) {
	app, p, store := ilTrayApp(t)

	app.onMenuClick(menuIDSettings) // open the window first
	app.wg.Wait()
	app.onMenuClick(menuIDLoginCode)
	app.wg.Wait()

	js := p.allJS()
	if !strings.Contains(js, "onLoginCodeMinted") {
		t.Fatalf("an open window was never told about the minted code: %q", js)
	}
	anchor := store.Get().LoginBootstrap
	if anchor == nil {
		t.Fatal("no bootstrap anchor was written")
	}
	if app.pendingLoginCode != nil {
		t.Error("a code was queued for a first paint that already happened")
	}

	// Close and reopen: the code is not resurrected. It is single use and two
	// minutes old at best, and a stale one on screen reads as one that works.
	app.onSettingsClose()
	app.onMenuClick(menuIDSettings)
	app.wg.Wait()
	if got := ilCodeFromDocument(t, p.lastDoc(t)); got != "" {
		t.Fatalf("a later window re-showed a spent login code: %q", got)
	}
}

// Minting twice replaces rather than accumulates, so the second code is the
// only one that works — the property mintBootstrapCode already has, asserted
// here because the panel's wording depends on it.
func TestILTrayLoginCode_SecondCodeReplacesTheFirst(t *testing.T) {
	app, p, store := ilTrayApp(t)

	app.onMenuClick(menuIDLoginCode)
	app.wg.Wait()
	first := ilCodeFromDocument(t, p.lastDoc(t))

	app.onSettingsClose()
	app.onMenuClick(menuIDLoginCode)
	app.wg.Wait()
	second := ilCodeFromDocument(t, p.lastDoc(t))

	if first == "" || second == "" || first == second {
		t.Fatalf("two mints produced %q and %q", first, second)
	}
	s := store.Get()
	if err := consumeBootstrapCode(s, first); err == nil {
		t.Fatal("the replaced code still registers a passkey")
	}
	if err := consumeBootstrapCode(s, second); err != nil {
		t.Fatalf("the current code was refused: %v", err)
	}
}

// ilCodeFromDocument reads the seeded loginCode.code out of a rendered
// settings document, or "" when none was seeded.
func ilCodeFromDocument(t *testing.T, doc string) string {
	t.Helper()
	const key = `"code":"`
	idx := strings.Index(doc, key)
	if idx < 0 {
		return ""
	}
	rest := doc[idx+len(key):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("unterminated code value in the settings document")
	}
	return rest[:end]
}

// ---------------------------------------------------------------------------
// The tab itself, under goja — same bundle + DOM shim as
// settings_enrolments_ui_test.go.
//
// Two states are pinned here because they are the two an operator is most
// likely to meet and least likely to recover from unaided: a machine with no
// passkeys (the first-run state, where the panel is the only thing that can
// say where a code comes from) and a refused operation (where a panel that
// showed nothing would look like a button that did nothing).
// ---------------------------------------------------------------------------

func ilSeedPasskeyVM(t *testing.T, passkeysJSON, sessionsJSON, codeJSON string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.__confirmed = [];
		window.__confirmAnswer = true;
		window.confirm = function(msg){ window.__confirmed.push(String(msg)); return window.__confirmAnswer; };
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'passkeys';
		window.state.passkeys = ` + passkeysJSON + `;
		window.state.loginSessions = ` + sessionsJSON + `;
		window.state.loginCode = ` + codeJSON + `;
		window.state.passkeyError = null;
		window.state.passkeyRevoked = null;
		window.state.loginSignedOut = null;
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding passkey state: %v", err)
	}
	return vm
}

const ilPasskeysFixture = `[{
	id: 'AAAABBBBCCCCDDDDEEEE', short: 'AAAABBBBCCCC…', name: 'MacBook Touch ID',
	created: '2026-08-20T09:14:00Z', sign_count: 0, counter_supported: false
}]`

const ilSessionsFixture = `[{
	id: 'a3f1c8de-5b21-4f70-9e6a-2d4c81b0e957',
	name: 'login AAAABBBBCCCC… 2026-08-28T08:12:04Z',
	created: '2026-08-28T08:12:04Z', expires: '2026-08-28T20:12:04Z'
}]`

// The first-run state. It is the normal state of a fresh install, so the panel
// has to say what a passkey is FOR and name both ways to get a code — a page
// that just said "none" would leave the operator with no next step and a login
// page they cannot get past.
func TestILPasskeysTab_EmptyStateNamesBothWaysToGetACode(t *testing.T) {
	vm := ilSeedPasskeyVM(t, `[]`, `[]`, `null`)
	html := evalString(t, vm, `window.renderPasskeys()`)

	for _, want := range []string{
		"No passkeys are registered",
		"how you sign in to relay from a browser",
		"Show Login Code...", // the tray item, spelled as the menu spells it
		"relay login enrol",  // and the path that works over SSH
		"two minutes",
		"cannot be self-service",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the empty state is missing %q\n%s", want, html)
		}
	}
	// The sessions half has its own empty state; "no passkeys" must not
	// silently stand in for "nobody is signed in".
	if !strings.Contains(html, "No browser is signed in") {
		t.Errorf("the signed-in-browsers section has no empty state\n%s", html)
	}
}

// A refused operation says what failed. The message is the Go side's verbatim,
// so the panel and `relay login revoke` give the same account of the same
// refusal.
func TestILPasskeysTab_FailureStateShowsTheRefusal(t *testing.T) {
	vm := ilSeedPasskeyVM(t, ilPasskeysFixture, ilSessionsFixture, `null`)
	if _, err := vm.RunString(`window.onPasskeyError('no passkey found with id "ghost"')`); err != nil {
		t.Fatalf("onPasskeyError: %v", err)
	}
	html := evalString(t, vm, `window.renderPasskeys()`)

	if !strings.Contains(html, `no passkey found with id`) || !strings.Contains(html, "ghost") {
		t.Errorf("the refusal did not reach the screen\n%s", html)
	}
	if !strings.Contains(html, "proj-error") {
		t.Error("the refusal is not rendered as an error")
	}
	// And the list is still there: a failed revoke must not empty the panel.
	if !strings.Contains(html, "MacBook Touch ID") {
		t.Error("a failed operation blanked the passkey list")
	}
}

// The code banner. Three facts have to be on it, and each is a real mistake
// when missing: what it does, how long it lasts, and that minting another one
// kills this one (mintBootstrapCode replaces rather than accumulates).
func TestILPasskeysTab_CodeBannerStatesWhatTheCodeIsAndIsNot(t *testing.T) {
	vm := ilSeedPasskeyVM(t, `[]`, `[]`, `{code:'c0ffee1234', expires:'2026-08-28T12:00:00Z', ttl:'2m0s', url:'http://localhost:8790/relay/login'}`)
	html := evalString(t, vm, `window.renderPasskeys()`)

	for _, want := range []string{
		"c0ffee1234",
		"2026-08-28T12:00:00Z",
		"2m0s",
		"single use",
		"NOT a password",
		"Showing another code replaces this one",
		"http://localhost:8790/relay/login",
		"not recoverable",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the code banner is missing %q\n%s", want, html)
		}
	}
}

// A machine with no TCP listener has nowhere to redeem a code. Say so, rather
// than printing a code and a login page that does not exist.
func TestILPasskeysTab_CodeBannerSaysThereIsNoLoginPageWhenTheBindIsAbsent(t *testing.T) {
	vm := ilSeedPasskeyVM(t, `[]`, `[]`, `{code:'c0ffee1234', expires:'2026-08-28T12:00:00Z', ttl:'2m0s', url:''}`)
	html := evalString(t, vm, `window.renderPasskeys()`)

	if !strings.Contains(html, "RELAY_API_LISTEN") || !strings.Contains(html, "no login page") {
		t.Errorf("a code was offered with no page to redeem it on\n%s", html)
	}
}

// The revoke confirmation names the sessions that SURVIVE it. "Are you sure?"
// over a credential id is not a decision anyone can make, and an operator who
// read "revoked" as "signed out" would be wrong for up to twelve hours.
func TestILPasskeysTab_RevokeConfirmationSaysSessionsSurvive(t *testing.T) {
	vm := ilSeedPasskeyVM(t, ilPasskeysFixture, ilSessionsFixture, `null`)
	if _, err := vm.RunString(`window.revokePasskey('AAAABBBBCCCCDDDDEEEE')`); err != nil {
		t.Fatalf("revokePasskey: %v", err)
	}

	prompt := evalString(t, vm, `window.__confirmed.join('\n')`)
	for _, want := range []string{"MacBook Touch ID", "does NOT sign anyone out", "1 browser session"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the revoke confirmation is missing %q\n%s", want, prompt)
		}
	}
	sent := evalString(t, vm, `window.__sent.join('\n')`)
	if !strings.Contains(sent, `"type":"revoke_passkey"`) || !strings.Contains(sent, "AAAABBBBCCCCDDDDEEEE") {
		t.Errorf("the confirmed revoke sent %q", sent)
	}
}

const ilEvePasskeysFixture = `[
	{ id: 'eve-cred-a', short: 'eve-cred-a…', label: 'iPhone', created: '2026-09-07T10:00:00Z', last_used: '2026-09-07T11:00:00Z', revocation_pending: false },
	{ id: 'eve-cred-b', short: 'eve-cred-b…', label: 'MacBook', created: '2026-09-06T10:00:00Z', last_used: '', revocation_pending: true }
]`

// The eve section renders under relay's own passkey list: a non-pending row
// gets a Revoke button, a pending one shows the pending text instead
// (docs/eve-passkey-enrolment.md: "A row with a pending revocation shows
// that text instead of the button").
func TestILPasskeysTab_EveSectionShowsRevokeOrPendingPerRow(t *testing.T) {
	vm := ilSeedPasskeyVM(t, `[]`, `[]`, `null`)
	if _, err := vm.RunString(`window.state.evePasskeys = ` + ilEvePasskeysFixture + `;`); err != nil {
		t.Fatalf("seeding eve passkeys: %v", err)
	}
	html := evalString(t, vm, `window.renderPasskeys()`)

	for _, want := range []string{"Eve passkeys", "iPhone", "MacBook", "revocation pending"} {
		if !strings.Contains(html, want) {
			t.Errorf("the eve section is missing %q\n%s", want, html)
		}
	}
}

// Revoking an eve passkey sends the eve-specific IPC message type, never the
// relay one -- the two must not collide on the wire.
func TestILPasskeysTab_RevokeEvePasskeySendsTheEveMessageType(t *testing.T) {
	vm := ilSeedPasskeyVM(t, `[]`, `[]`, `null`)
	if _, err := vm.RunString(`window.state.evePasskeys = ` + ilEvePasskeysFixture + `;`); err != nil {
		t.Fatalf("seeding eve passkeys: %v", err)
	}
	if _, err := vm.RunString(`window.revokeEvePasskey('eve-cred-a')`); err != nil {
		t.Fatalf("revokeEvePasskey: %v", err)
	}

	sent := evalString(t, vm, `window.__sent.join('\n')`)
	if !strings.Contains(sent, `"type":"revoke_eve_passkey"`) || !strings.Contains(sent, "eve-cred-a") {
		t.Errorf("the confirmed eve revoke sent %q", sent)
	}
}

// Signing out is a different act from revoking, and the panel says which is
// which at the point of the click.
func TestILPasskeysTab_SignOutConfirmationDoesNotClaimToRevokeAPasskey(t *testing.T) {
	vm := ilSeedPasskeyVM(t, ilPasskeysFixture, ilSessionsFixture, `null`)
	if _, err := vm.RunString(`window.signOutLogin('a3f1c8de-5b21-4f70-9e6a-2d4c81b0e957')`); err != nil {
		t.Fatalf("signOutLogin: %v", err)
	}

	prompt := evalString(t, vm, `window.__confirmed.join('\n')`)
	for _, want := range []string{"run the login ceremony again", "No passkey is revoked"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the sign-out confirmation is missing %q\n%s", want, prompt)
		}
	}
	sent := evalString(t, vm, `window.__sent.join('\n')`)
	if !strings.Contains(sent, `"type":"sign_out_login"`) {
		t.Errorf("sign out sent %q", sent)
	}
}
