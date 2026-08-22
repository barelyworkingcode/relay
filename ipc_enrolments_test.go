package main

// Hermetic tests for the Remote Clients IPC handlers. Mirrors the CLI coverage
// in enrolment_test.go: every handler proves it (a) emits the right event,
// (b) persists through the SettingsStore, and (c) refuses what enrolment.go
// refuses, with enrolment.go's wording rather than a second opinion.
//
// The load-bearing one is TestIPCCreateEnrolment_NeverEmitsKeyMaterial: the
// bundle contains a client private key, and the whole reason the IPC surface
// hands back a directory rather than the bundle struct is that key material
// must never reach a WebView.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"relaygo/bridge"
)

// newEnrolmentIPC stands up an IPCContext over a sandboxed store whose config
// dir also holds the CA and the emitted bundles — nothing here can touch the
// real ~/Library/Application Support/relay.
//
// auditEnabled builds a bare recorder rather than a real one: the only method
// this surface calls on it is Enabled(), and starting a writer goroutine to
// answer one bool would be the tail wagging the dog. A nil recorder is the
// genuine "auditing is off" state — Enabled() is nil-safe.
func newEnrolmentIPC(t *testing.T, auditEnabled bool) (*IPCContext, SettingsStore, *recordingUI) {
	t.Helper()
	_, store := newEnrolmentSandbox(t)
	ui := &recordingUI{}
	var rec *AuditRecorder
	if auditEnabled {
		rec = &AuditRecorder{cfg: resolvedAuditConfig{Enabled: true}}
	}
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
		Audit:                  rec,
	}, store, ui
}

// emittedJSON renders every event the UI recorded as one JSON blob, which is
// what a leak test needs: it does not matter which argument of which event
// something rode out on, only that it left the process.
func emittedJSON(t *testing.T, ui *recordingUI) string {
	t.Helper()
	ui.mu.Lock()
	defer ui.mu.Unlock()
	var b strings.Builder
	for _, ev := range ui.events {
		b.WriteString(ev.Name)
		for _, a := range ev.Args {
			data, err := json.Marshal(a)
			if err != nil {
				t.Fatalf("marshal event arg: %v", err)
			}
			b.Write(data)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestIPCCreateEnrolment_PersistsAndEmitsBundleDirectory(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	ipcCreateEnrolment(ipc, mustRaw(t, map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	}))

	args, ok := findEvent(ui, "onEnrolmentCreated")
	if !ok {
		t.Fatalf("expected onEnrolmentCreated; got %+v", ui.events)
	}
	var created Enrolment
	if err := json.Unmarshal(args[0].(json.RawMessage), &created); err != nil {
		t.Fatalf("unmarshal enrolment: %v", err)
	}
	if created.ClientID != "hermes-mail" || !created.GrantsProject(mail.ID) {
		t.Fatalf("emitted enrolment does not match the request: %+v", created)
	}
	// An unset budget takes the conservative default, never "unlimited".
	if created.Budget.MaxCalls != defaultEnrolmentMaxCalls {
		t.Errorf("unset budget did not default: %+v", created.Budget)
	}
	if store.Get().FindEnrolment("hermes-mail") == nil {
		t.Error("enrolment was not persisted")
	}

	// The bundle payload is a directory and nothing else. A second key on this
	// object is the shape a key would arrive in.
	var bundle map[string]interface{}
	if err := json.Unmarshal(args[1].(json.RawMessage), &bundle); err != nil {
		t.Fatalf("unmarshal bundle view: %v", err)
	}
	if len(bundle) != 1 {
		t.Fatalf("bundle view carries more than the directory: %+v", bundle)
	}
	dir, _ := bundle["dir"].(string)
	if dir == "" {
		t.Fatal("bundle view has no dir")
	}
	if _, err := os.Stat(filepath.Join(dir, "client.key")); err != nil {
		t.Errorf("bundle directory has no client.key on disk: %v", err)
	}
}

// The one that matters. createEnrolment writes a private key to disk; not one
// byte of it may ride out on an IPC event, in any field, on any argument.
func TestIPCCreateEnrolment_NeverEmitsKeyMaterial(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	ipcCreateEnrolment(ipc, mustRaw(t, map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	}))

	args, ok := findEvent(ui, "onEnrolmentCreated")
	if !ok {
		t.Fatalf("expected onEnrolmentCreated; got %+v", ui.events)
	}
	var bundle map[string]string
	if err := json.Unmarshal(args[1].(json.RawMessage), &bundle); err != nil {
		t.Fatalf("unmarshal bundle view: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(bundle["dir"], "client.key"))
	if err != nil {
		t.Fatalf("read the emitted key: %v", err)
	}

	emitted := emittedJSON(t, ui)
	// The literal key bytes, in whole and in part. A partial match catches a
	// truncated "preview" as surely as a whole one catches a full copy.
	if strings.Contains(emitted, strings.TrimSpace(string(keyPEM))) {
		t.Fatal("the client private key was emitted to the settings UI")
	}
	for _, needle := range []string{"PRIVATE KEY", "-----BEGIN", "BEGIN EC", "BEGIN RSA"} {
		if strings.Contains(emitted, needle) {
			t.Fatalf("PEM material %q reached the settings UI: %s", needle, emitted)
		}
	}
	// Body-of-the-key check: any long line from the PEM appearing anywhere in
	// the emitted payload is key material regardless of how it was framed.
	for _, line := range strings.Split(string(keyPEM), "\n") {
		if len(line) < 40 {
			continue
		}
		if strings.Contains(emitted, line) {
			t.Fatalf("a line of the private key reached the settings UI")
		}
	}
}

// A grant naming a local project is refused, and the refusal is enrolment.go's
// — it names the project so the operator knows which grant to drop.
func TestIPCCreateEnrolment_RefusesLocalProjectGrant(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	local := mkStoreProject(t, store, ProjectKindLocal, "Workspace", t.TempDir())

	ipcCreateEnrolment(ipc, mustRaw(t, map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{local.ID},
	}))

	args, ok := findEvent(ui, "onEnrolmentError")
	if !ok {
		t.Fatalf("expected onEnrolmentError; got %+v", ui.events)
	}
	msg, _ := args[0].(string)
	for _, want := range []string{local.ID, "Workspace", "local project"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name %q; got: %s", want, msg)
		}
	}
	// A refused enrolment persists nothing and emits no credential.
	if got := store.Get().Enrolments; len(got) != 0 {
		t.Fatalf("refused enrolment was persisted: %+v", got)
	}
	if _, ok := findEvent(ui, "onEnrolmentCreated"); ok {
		t.Fatal("a refused enrolment emitted a bundle")
	}
}

func TestIPCCreateEnrolment_RequiresClientID(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	ipcCreateEnrolment(ipc, mustRaw(t, map[string]interface{}{"client_id": "   "}))

	args, ok := findEvent(ui, "onEnrolmentError")
	if !ok {
		t.Fatalf("expected onEnrolmentError; got %+v", ui.events)
	}
	if msg, _ := args[0].(string); !strings.Contains(msg, "client id is required") {
		t.Errorf("unexpected refusal: %s", msg)
	}
	if len(store.Get().Enrolments) != 0 {
		t.Error("an enrolment was created without a client id")
	}
}

// A duplicate client id is refused rather than shadowing the first record.
func TestIPCCreateEnrolment_RefusesDuplicateClientID(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	body := mustRaw(t, map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	})
	ipcCreateEnrolment(ipc, body)
	ipcCreateEnrolment(ipc, body)

	args, ok := findEvent(ui, "onEnrolmentError")
	if !ok {
		t.Fatalf("expected onEnrolmentError on the second create; got %+v", ui.events)
	}
	if msg, _ := args[0].(string); !strings.Contains(msg, "already exists") {
		t.Errorf("unexpected refusal: %s", msg)
	}
	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("want 1 enrolment after a duplicate create, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Revoke
// ---------------------------------------------------------------------------

// Revoking deletes the record, removes the bundle, and — the half a record
// deletion cannot do — fires the hook that severs live connections. Going
// through revokeEnrolment rather than RemoveEnrolment is what buys that third
// property, so the test asserts on it directly.
func TestIPCRevokeEnrolment_DeletesRecordBundleAndFiresHook(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	ipcCreateEnrolment(ipc, mustRaw(t, map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	}))
	created := store.Get().FindEnrolment("hermes-mail")
	if created == nil {
		t.Fatal("setup: enrolment was not created")
	}
	fingerprint := created.Fingerprint
	// bridge.ConfigDir() is redirected to the sandbox by newEnrolmentSandbox.
	bundleDir := filepath.Join(bridge.ConfigDir(), enrolmentBundleDir, "hermes-mail")

	var hookClient, hookFingerprint string
	SetEnrolmentRevocationHook(func(clientID, fp string) {
		hookClient, hookFingerprint = clientID, fp
	})
	t.Cleanup(func() { SetEnrolmentRevocationHook(nil) })

	ipcRevokeEnrolment(ipc, mustRaw(t, map[string]interface{}{"client_id": "hermes-mail"}))

	args, ok := findEvent(ui, "onEnrolmentRevoked")
	if !ok {
		t.Fatalf("expected onEnrolmentRevoked; got %+v", ui.events)
	}
	if got, _ := args[0].(string); got != "hermes-mail" {
		t.Errorf("revoked event named %q", got)
	}
	// The fingerprint rides along because once the record is gone it is the
	// only thing that identifies this client's calls in the audit log.
	if got, _ := args[1].(string); got != fingerprint {
		t.Errorf("revoked event fingerprint = %q, want the full %q", got, fingerprint)
	}
	if store.Get().FindEnrolment("hermes-mail") != nil {
		t.Error("the enrolment record survived revocation")
	}
	if _, err := os.Stat(bundleDir); !os.IsNotExist(err) {
		t.Errorf("the emitted bundle survived revocation: %v", err)
	}
	if hookClient != "hermes-mail" || hookFingerprint != fingerprint {
		t.Errorf("revocation hook got (%q, %q); live connections would not be severed", hookClient, hookFingerprint)
	}
}

func TestIPCRevokeEnrolment_UnknownClientIsRefusedByName(t *testing.T) {
	ipc, _, ui := newEnrolmentIPC(t, true)
	ipcRevokeEnrolment(ipc, mustRaw(t, map[string]interface{}{"client_id": "ghost"}))

	args, ok := findEvent(ui, "onEnrolmentError")
	if !ok {
		t.Fatalf("expected onEnrolmentError; got %+v", ui.events)
	}
	if msg, _ := args[0].(string); !strings.Contains(msg, "ghost") {
		t.Errorf("refusal must name the client id; got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// The remote block
// ---------------------------------------------------------------------------

// Saving an address without turning the listener on must NOT write
// enabled: true. Opening a network listener is a thing the operator says, not
// a thing relay infers from an address being present.
func TestIPCUpdateRemoteConfig_CreatingTheBlockDoesNotEnableIt(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	if store.Get().Remote != nil {
		t.Fatal("setup: expected no remote block")
	}

	ipcUpdateRemoteConfig(ipc, mustRaw(t, map[string]interface{}{
		"enabled": false,
		"listen":  "127.0.0.1:9911",
	}))

	cfg := store.Get().Remote
	if cfg == nil {
		t.Fatal("the remote block was not created")
	}
	if cfg.Enabled != nil {
		t.Errorf("disabled collapsed to an explicit %v rather than an absent key", *cfg.Enabled)
	}
	if cfg.Listen != "127.0.0.1:9911" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.resolve().Enabled {
		t.Error("a block created by setting an address resolved to enabled")
	}

	args, ok := findEvent(ui, "onRemoteConfigUpdated")
	if !ok {
		t.Fatalf("expected onRemoteConfigUpdated; got %+v", ui.events)
	}
	var view remoteConfigView
	if err := json.Unmarshal(args[0].(json.RawMessage), &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	// present-but-disabled: configured true, enabled false. The UI needs both
	// to distinguish it from an absent block.
	if !view.Configured || view.Enabled {
		t.Errorf("view = %+v, want configured=true enabled=false", view)
	}
}

func TestIPCUpdateRemoteConfig_EnableThenRemove(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)

	ipcUpdateRemoteConfig(ipc, mustRaw(t, map[string]interface{}{
		"enabled": true,
		"listen":  "127.0.0.1:9910",
	}))
	cfg := store.Get().Remote
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled {
		t.Fatalf("enabling did not write enabled: true; got %+v", cfg)
	}

	// Removing returns the install to "no block at all", which is a different
	// state from a disabled listener: nothing is opened, and nothing was ever
	// asked for.
	ipcUpdateRemoteConfig(ipc, mustRaw(t, map[string]interface{}{"remove": true}))
	if got := store.Get().Remote; got != nil {
		t.Fatalf("remove left a block behind: %+v", got)
	}

	args, _ := findEvent(ui, "onRemoteConfigUpdated")
	var view remoteConfigView
	if err := json.Unmarshal(args[0].(json.RawMessage), &view); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	if view.Configured || view.Enabled {
		t.Errorf("view after remove = %+v, want configured=false enabled=false", view)
	}
	// With no block, the effective address is still reported so the UI can
	// show what a listener WOULD bind — and it is loopback.
	if view.Effective != defaultRemoteListen {
		t.Errorf("effective = %q, want the loopback default %q", view.Effective, defaultRemoteListen)
	}
}

func TestIPCUpdateRemoteConfig_RefusesMalformedListen(t *testing.T) {
	ipc, store, ui := newEnrolmentIPC(t, true)
	for _, addr := range []string{"not-an-address", "127.0.0.1", "127.0.0.1:not-a-port"} {
		ipcUpdateRemoteConfig(ipc, mustRaw(t, map[string]interface{}{
			"enabled": true,
			"listen":  addr,
		}))
		args, ok := findEvent(ui, "onRemoteConfigError")
		if !ok {
			t.Fatalf("listen %q was accepted; got %+v", addr, ui.events)
		}
		if msg, _ := args[0].(string); !strings.Contains(msg, addr) {
			t.Errorf("refusal must quote the address; got %s", msg)
		}
		if got := store.Get().Remote; got != nil {
			t.Fatalf("a malformed address was persisted: %+v", got)
		}
	}
}

// A blank address stores blank rather than freezing today's default into
// settings.json — resolve() applies the default at read time instead.
func TestIPCUpdateRemoteConfig_BlankListenStaysBlank(t *testing.T) {
	ipc, store, _ := newEnrolmentIPC(t, true)
	ipcUpdateRemoteConfig(ipc, mustRaw(t, map[string]interface{}{"enabled": true, "listen": "  "}))

	cfg := store.Get().Remote
	if cfg == nil || cfg.Listen != "" {
		t.Fatalf("blank listen was not stored blank: %+v", cfg)
	}
	if got := cfg.resolve().Listen; got != defaultRemoteListen {
		t.Errorf("blank listen resolved to %q, want %q", got, defaultRemoteListen)
	}
}

// Disabled auditing is a hard stop for remote access — the listener refuses to
// start — so the view reports it alongside the block's own state and the UI
// renders "off as a consequence" rather than "configured".
func TestRemoteConfigView_ReportsAuditAsTheGateOnRemoteAccess(t *testing.T) {
	ipcOn, storeOn, _ := newEnrolmentIPC(t, true)
	ipcUpdateRemoteConfig(ipcOn, mustRaw(t, map[string]interface{}{"enabled": true, "listen": "127.0.0.1:9910"}))
	if v := remoteConfigViewOf(storeOn.Get(), ipcOn.Audit.Enabled()); !v.Enabled || !v.AuditEnabled {
		t.Errorf("audit on: view = %+v, want enabled + audit_enabled", v)
	}

	// Same block, no recorder: the block still reads enabled (that is what is
	// configured) while audit_enabled is false, which is the pair the UI turns
	// into "remote access is off because auditing is disabled".
	ipcOff, storeOff, _ := newEnrolmentIPC(t, false)
	ipcUpdateRemoteConfig(ipcOff, mustRaw(t, map[string]interface{}{"enabled": true, "listen": "127.0.0.1:9910"}))
	v := remoteConfigViewOf(storeOff.Get(), ipcOff.Audit.Enabled())
	if !v.Enabled {
		t.Errorf("audit off: block should still read as configured-enabled, got %+v", v)
	}
	if v.AuditEnabled {
		t.Errorf("audit off: view claims auditing is on: %+v", v)
	}
}

// The first paint carries the same three facts as the IPC view, so the tab is
// correct before any message is exchanged.
func TestRenderSettingsHTML_SeedsEnrolmentsAndRemoteBlock(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")
	if _, err := createEnrolment(store, enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: []string{mail.ID}}); err != nil {
		t.Fatalf("createEnrolment: %v", err)
	}
	store.With(func(s *Settings) {
		enabled := true
		s.Remote = &RemoteConfig{Enabled: &enabled, Listen: "127.0.0.1:9910"}
	})

	s := store.Get()
	html := renderSettingsHTML(s, nil, nil, nil)
	fingerprint := s.Enrolments[0].Fingerprint

	for _, want := range []string{"hermes-mail", fingerprint, `"configured":true`, `"listen":"127.0.0.1:9910"`} {
		if !strings.Contains(html, want) {
			t.Errorf("first paint is missing %q", want)
		}
	}
	// And no key material rides along on the first paint either.
	for _, forbidden := range []string{"PRIVATE KEY", "-----BEGIN"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("first paint carries %q", forbidden)
		}
	}
}

// Every __X_JSON__ token in the shell must be substituted by renderSettingsHTML.
// An unreplaced one leaves window.__RELAY_INIT__ syntactically invalid, which
// does not fail loudly in one tab — it throws on load and takes the entire
// settings bundle with it. Adding a token to web/shell.html without adding it
// to the replacer is the whole failure mode, so this asserts on the class of
// mistake rather than on today's list. (cmd/devui carries the same list for
// the same reason.)
func TestRenderSettingsHTML_LeavesNoUnsubstitutedPlaceholder(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	html := renderSettingsHTML(store.Get(), nil, nil, nil)
	if left := regexp.MustCompile(`__[A-Z0-9_]+_JSON__`).FindAllString(html, -1); len(left) > 0 {
		t.Fatalf("renderSettingsHTML left placeholders unsubstituted: %v", left)
	}
}
