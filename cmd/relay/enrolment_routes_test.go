package main

// HTTP coverage for the ADR-014 enrolments slice: RegisterEnrolmentRoutes
// shares EnrolmentOps with the Remote Clients tab's IPC handlers
// (ipc_enrolments_test.go), so these tests focus on the envelope — status
// codes, request/response shape — rather than re-proving grant and
// client-id validation internal/enrolment already covers.
//
// TestEnrolmentRoutes_CreateNeverLeaksKeyMaterial is the one that matters:
// enrolment.Create writes a client private key to disk, and not one byte of
// it may ride out over the wire in any response.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/enrolment"
)

func newEnrolmentRoutesServer(t *testing.T, onChange func()) (*httptest.Server, config.SettingsStore) {
	t.Helper()
	store := newCLISandboxStore(t)
	ops := &EnrolmentOps{Store: store, OnChange: onChange, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterEnrolmentRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	return httptest.NewServer(mux), store
}

func TestEnrolmentRoutes_CreateListGet(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()
	mail := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")

	resp, body := doJSON(t, "POST", srv.URL+"/api/enrolments", map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{mail.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created enrolmentView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ClientID != "hermes-mail" {
		t.Errorf("client_id = %q", created.ClientID)
	}
	if created.Dir == "" {
		t.Fatal("create response has no bundle directory")
	}
	if created.Fingerprint == "" {
		t.Error("create response has no fingerprint")
	}
	if len(created.ProjectIDs) != 1 || created.ProjectIDs[0] != mail.ID {
		t.Errorf("project_ids = %v", created.ProjectIDs)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/enrolments/hermes-mail", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, body %s", resp.StatusCode, body)
	}
	var fetched enrolmentView
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode fetched: %v", err)
	}
	if fetched.ClientID != "hermes-mail" || fetched.Fingerprint != created.Fingerprint {
		t.Errorf("fetched mismatch: %+v", fetched)
	}
	if fetched.Dir != "" {
		t.Error("GET must not carry a bundle directory — only create does")
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/enrolments", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []enrolmentView
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ClientID != "hermes-mail" {
		t.Errorf("expected 1 enrolment, got %+v", listed)
	}
}

func TestEnrolmentRoutes_UnknownID404(t *testing.T) {
	srv, _ := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	cases := []struct{ method, path string }{
		{"GET", "/api/enrolments/nope"},
		{"DELETE", "/api/enrolments/nope"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, body := doJSON(t, c.method, srv.URL+c.path, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, body %s", resp.StatusCode, body)
			}
		})
	}
}

func TestEnrolmentRoutes_CreateRefusesLocalProjectGrant(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()
	local := mkStoreProject(t, store, config.ProjectKindLocal, "Workspace", t.TempDir())

	resp, body := doJSON(t, "POST", srv.URL+"/api/enrolments", map[string]interface{}{
		"client_id":   "hermes-mail",
		"project_ids": []string{local.ID},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), local.ID) {
		t.Errorf("refusal must name the offending project: %s", body)
	}
	if got := store.Get().Enrolments; len(got) != 0 {
		t.Fatalf("a refused create must persist nothing: %+v", got)
	}
}

func TestEnrolmentRoutes_CreateRefusesDuplicateClientID(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	body := map[string]interface{}{"client_id": "hermes-mail"}
	resp, raw := doJSON(t, "POST", srv.URL+"/api/enrolments", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: status %d, body %s", resp.StatusCode, raw)
	}
	resp, raw = doJSON(t, "POST", srv.URL+"/api/enrolments", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate create: status %d, body %s", resp.StatusCode, raw)
	}
	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("want 1 enrolment after a duplicate create, got %d", got)
	}
}

func TestEnrolmentRoutes_CreateMalformedJSON(t *testing.T) {
	srv, _ := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/enrolments", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// The load-bearing test in this file. enrolment.Create writes a client
// private key to disk; the whole reason the HTTP response hands back a
// bundle directory rather than the bundle's contents is that the key must
// never cross this boundary — not whole, not truncated, not as a "preview".
func TestEnrolmentRoutes_CreateNeverLeaksKeyMaterial(t *testing.T) {
	srv, _ := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/enrolments", map[string]interface{}{
		"client_id": "hermes-mail",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created enrolmentView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(created.Dir, "client.key"))
	if err != nil {
		t.Fatalf("read the emitted key: %v", err)
	}

	raw := string(body)
	// The literal key bytes, in whole and in part. A partial match catches a
	// truncated "preview" as surely as a whole one catches a full copy.
	if strings.Contains(raw, strings.TrimSpace(string(keyPEM))) {
		t.Fatal("the client private key was returned in the create response")
	}
	for _, needle := range []string{"PRIVATE KEY", "-----BEGIN", "BEGIN EC", "BEGIN RSA"} {
		if strings.Contains(raw, needle) {
			t.Fatalf("PEM material %q reached the HTTP response: %s", needle, raw)
		}
	}
	for _, line := range strings.Split(string(keyPEM), "\n") {
		if len(line) < 40 {
			continue
		}
		if strings.Contains(raw, line) {
			t.Fatal("a line of the private key reached the HTTP response")
		}
	}

	// Every field on the wire is one enrolmentViewOf/createdViewOf can
	// produce; an unexpected field is the shape a key would arrive in.
	var generic map[string]interface{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode as generic map: %v", err)
	}
	for k := range generic {
		switch k {
		case "client_id", "fingerprint", "project_ids", "budget", "created_at", "dir", "bundle_error":
		default:
			t.Errorf("unexpected field %q in create response", k)
		}
	}
}

func TestEnrolmentRoutes_RevokeFiresHookAnd204(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/enrolments", map[string]interface{}{"client_id": "hermes-mail"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	fingerprint := enrolment.Find(store.Get(), "hermes-mail").Fingerprint

	var hookClient, hookFingerprint string
	enrolment.SetRevocationHook(func(clientID, fp string) {
		hookClient, hookFingerprint = clientID, fp
	})
	t.Cleanup(func() { enrolment.SetRevocationHook(nil) })

	resp, body = doJSON(t, "DELETE", srv.URL+"/api/enrolments/hermes-mail", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, body %s", resp.StatusCode, body)
	}
	if enrolment.Find(store.Get(), "hermes-mail") != nil {
		t.Error("the enrolment record survived revocation")
	}
	if hookClient != "hermes-mail" || hookFingerprint != fingerprint {
		t.Errorf("revocation hook got (%q, %q); live connections would not be severed", hookClient, hookFingerprint)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/enrolments/hermes-mail", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 on second delete, got %d", resp.StatusCode)
	}
}

func TestEnrolmentRoutes_RemoteConfigGetAndPut(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/remote", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, body %s", resp.StatusCode, body)
	}
	var view remoteConfigView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Configured {
		t.Errorf("no block configured yet: %+v", view)
	}
	if view.Effective != defaultRemoteListen {
		t.Errorf("effective = %q, want the loopback default %q", view.Effective, defaultRemoteListen)
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/remote", map[string]interface{}{
		"enabled": true,
		"listen":  "127.0.0.1:9910",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: status %d, body %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.Configured || !view.Enabled {
		t.Errorf("view after enabling = %+v", view)
	}
	cfg := store.Get().Remote
	if cfg == nil || cfg.Enabled == nil || !*cfg.Enabled || cfg.Listen != "127.0.0.1:9910" {
		t.Fatalf("remote config not persisted: %+v", cfg)
	}
}

// validateRemoteListen refuses only what the listener could never bind — a
// malformed host:port. It deliberately does NOT refuse a non-loopback
// address (ADR-010): binding every interface is a choice the operator gets
// to make, only a UI warning away.
func TestEnrolmentRoutes_RemoteConfigRejectsMalformedListen(t *testing.T) {
	srv, store := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()

	for _, addr := range []string{"not-an-address", "127.0.0.1", "127.0.0.1:not-a-port"} {
		resp, body := doJSON(t, "PUT", srv.URL+"/api/remote", map[string]interface{}{
			"enabled": true,
			"listen":  addr,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("listen %q: status %d, body %s", addr, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), addr) {
			t.Errorf("refusal must quote the address, got: %s", body)
		}
		if got := store.Get().Remote; got != nil {
			t.Fatalf("a malformed address must not persist: %+v", got)
		}
	}
}

func TestEnrolmentRoutes_PutRemoteMalformedJSON(t *testing.T) {
	srv, _ := newEnrolmentRoutesServer(t, nil)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/remote", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestEnrolmentRoutes_OnChangeFires(t *testing.T) {
	var changed int
	srv, _ := newEnrolmentRoutesServer(t, func() { changed++ })
	defer srv.Close()

	doJSON(t, "POST", srv.URL+"/api/enrolments", map[string]interface{}{"client_id": "hermes-mail"})
	if changed != 1 {
		t.Fatalf("expected onChange after create, got %d", changed)
	}
	doJSON(t, "DELETE", srv.URL+"/api/enrolments/hermes-mail", nil)
	if changed != 2 {
		t.Fatalf("expected onChange after revoke, got %d", changed)
	}
	doJSON(t, "PUT", srv.URL+"/api/remote", map[string]interface{}{"enabled": true, "listen": "127.0.0.1:9910"})
	if changed != 3 {
		t.Fatalf("expected onChange after remote config update, got %d", changed)
	}
}
