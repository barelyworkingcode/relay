package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Absent means on; explicit false means off
// ---------------------------------------------------------------------------

// adControlDoor drives one request through the door every control-plane route
// is registered behind, so what is asserted is the production path from a
// Settings value to a row on disk — not RecordDecision called by hand.
func adControlDoor(t *testing.T, rec *AuditRecorder) {
	t.Helper()
	rr := &RouteRegistrar{Mux: http.NewServeMux(), Transport: TransportSocket, Auditor: rec}
	rr.Handle(ClassGrant, "POST /api/enrolments", func(w http.ResponseWriter, r *http.Request) {})
	rr.Mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/enrolments", nil))
}

func adStartRecorder(t *testing.T, s *Settings) *AuditRecorder {
	t.Helper()
	rec := startAuditRecorder(s)
	if rec != nil {
		t.Cleanup(rec.Close)
	}
	return rec
}

func TestAdSettingsWithNoAuditBlockRecordsAControlDecision(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	s := &Settings{Version: currentSettingsVersion}
	if s.Audit != nil {
		t.Fatal("fixture is not the case under test: it carries an audit block")
	}

	rec := adStartRecorder(t, s)
	if !rec.Enabled() {
		t.Fatal("no audit block left auditing off, so a fresh install records no control decision")
	}
	adControlDoor(t, rec)

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Event != AuditEventControlDecision {
		t.Errorf("event = %q, want %q", ev.Event, AuditEventControlDecision)
	}
	if ev.Path != "/api/enrolments" || ev.Class != string(ClassGrant) {
		t.Errorf("record does not name the decision: path=%q class=%q", ev.Path, ev.Class)
	}
}

// The upgrade path: an operator who deliberately turned auditing off stays off.
func TestAdExplicitlyDisabledAuditRecordsNothing(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	off := false
	s := &Settings{Version: currentSettingsVersion, Audit: &AuditConfig{Enabled: &off}}

	rec := adStartRecorder(t, s)
	if rec.Enabled() {
		t.Fatal("an explicit audit.enabled=false was overridden by the default")
	}
	adControlDoor(t, rec)

	path := filepath.Join(dir, "logs", "audit", "toolcalls.jsonl")
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		t.Fatalf("auditing is off yet %s holds %d bytes:\n%s", path, len(data), data)
	}
}

// ---------------------------------------------------------------------------
// Round-trip: neither absence nor an explicit false may change meaning
// ---------------------------------------------------------------------------

func adWriteSettings(t *testing.T, dir, body string) *FileSettingsStore {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}
	return NewSettingsStoreAt(dir)
}

func adReadRawSettings(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	return raw
}

func TestAdExistingSettingsWithNoAuditBlockRoundTripUnchanged(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := adWriteSettings(t, dir, `{"version":1,"external_mcps":[],"services":[],"projects":[]}`)

	assertNoErr(t, store.With(func(s *Settings) { s.AdminSecret = "rewritten" }), "With")

	if raw, ok := adReadRawSettings(t, dir)["audit"]; ok {
		t.Errorf("a rewrite invented an audit block the operator never wrote: %s", raw)
	}
	if !store.Get().Audit.resolve().Enabled {
		t.Error("an absent audit block resolved to disabled after a rewrite")
	}
}

func TestAdExistingExplicitFalseSurvivesARewrite(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := adWriteSettings(t, dir,
		`{"version":1,"external_mcps":[],"services":[],"projects":[],"audit":{"enabled":false}}`)

	assertNoErr(t, store.With(func(s *Settings) { s.AdminSecret = "rewritten" }), "With")

	raw, ok := adReadRawSettings(t, dir)["audit"]
	if !ok {
		t.Fatal("the audit block vanished on rewrite, so an operator's choice to disable reads as absent — which now means enabled")
	}
	var cfg AuditConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("audit block is not valid JSON: %v", err)
	}
	if cfg.Enabled == nil {
		t.Fatal("enabled was dropped on rewrite, re-enabling auditing for an operator who turned it off")
	}
	if *cfg.Enabled {
		t.Errorf("enabled = true after rewrite, want false")
	}
	if store.Get().Audit.resolve().Enabled {
		t.Error("an explicit false resolved to enabled after a rewrite")
	}
}

// ---------------------------------------------------------------------------
// A new install says in its own file what it is doing
// ---------------------------------------------------------------------------

func TestAdNewInstallWritesAuditEnabledExplicitly(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	raw, ok := adReadRawSettings(t, dir)["audit"]
	if !ok {
		t.Fatal("a new install's settings.json has no audit block, so what an operator reads there does not say what relay is doing")
	}
	var cfg AuditConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("audit block is not valid JSON: %v", err)
	}
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Errorf("a new install declares audit.enabled = %v, want an explicit true", cfg.Enabled)
	}
}

// ---------------------------------------------------------------------------
// On by default means every install writes, so every install must be bounded
// ---------------------------------------------------------------------------

func TestAdDefaultInstallLogIsBounded(t *testing.T) {
	mkEmptySandboxRelayHome(t)
	rec := adStartRecorder(t, &Settings{Version: currentSettingsVersion})
	if rec == nil {
		t.Fatal("no recorder for a default install")
	}

	if rec.cfg.MaxFileBytes != auditDefaultMaxFileBytes {
		t.Errorf("max_file_bytes = %d with no audit block, want %d", rec.cfg.MaxFileBytes, auditDefaultMaxFileBytes)
	}
	if rec.cfg.Generations != auditDefaultGenerations {
		t.Errorf("generations = %d with no audit block, want %d", rec.cfg.Generations, auditDefaultGenerations)
	}
	// This is subtle: max_file_bytes = 0 is not "unbounded" but "rotate on
	// every write" (log_rotate.go), so a lost default is a pathological log
	// rather than a large one, and either way the retention window is gone.
	if rec.cfg.MaxFileBytes <= 0 || rec.cfg.Generations <= 0 {
		t.Fatalf("a default install's log has no bound: %d bytes x %d generations",
			rec.cfg.MaxFileBytes, rec.cfg.Generations)
	}
}

// ---------------------------------------------------------------------------
// ADR-010's rule is unchanged: it is now reachable only by an explicit opt-out
// ---------------------------------------------------------------------------

func adRemoteStore(t *testing.T, audit *AuditConfig) (string, SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	assertNoErr(t, store.With(func(s *Settings) {
		s.Remote = &RemoteConfig{Enabled: ptr(true), Listen: "127.0.0.1:0"}
		s.Audit = audit
	}), "seed settings")
	return dir, store
}

func TestAdRemoteListenerStillRefusesWithAuditExplicitlyDisabled(t *testing.T) {
	off := false
	_, store := adRemoteStore(t, &AuditConfig{Enabled: &off})
	rec := adStartRecorder(t, store.Get())

	rs, err := NewRemoteServer(context.Background(), store, &appRouter{store: store, audit: rec}, rec)
	if rs != nil {
		rs.Close()
		t.Fatal("a remote listener was bound while auditing was explicitly disabled")
	}
	if err == nil {
		t.Fatal("the remote listener started with auditing explicitly disabled")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("the refusal does not explain itself to an operator: %v", err)
	}
}

func TestAdRemoteListenerStartsWithNoAuditBlock(t *testing.T) {
	_, store := adRemoteStore(t, nil)
	rec := adStartRecorder(t, store.Get())

	rs, err := NewRemoteServer(context.Background(), store, &appRouter{store: store, audit: rec}, rec)
	if err != nil {
		t.Fatalf("a remote listener was refused on an install that never wrote an audit block: %v", err)
	}
	if rs == nil {
		t.Fatal("no listener and no error for an enabled remote block")
	}
	rs.Close()
}
