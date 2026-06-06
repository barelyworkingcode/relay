package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestProjectRoutes_FrontendOmitsToken locks the security boundary: the
// eve-facing project endpoints must never serialize the plaintext token or its
// hash. rotate_token is the sole exception.
func TestProjectRoutes_FrontendOmitsToken(t *testing.T) {
	srv, store := newProjectRoutesServer(t)
	defer srv.Close()

	// Seed a project with a real token directly in the store.
	var id string
	if err := store.With(func(s *Settings) {
		p, err := s.CreateProjectWithToken("Secret", t.TempDir(), nil, nil, nil, nil)
		if err == nil {
			id = p.ID
		}
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if id == "" {
		t.Fatal("seed project: no id")
	}

	assertNoTokenKeys := func(label string, obj map[string]json.RawMessage) {
		if _, ok := obj["token"]; ok {
			t.Errorf("%s: response leaked 'token'", label)
		}
		if _, ok := obj["token_hash"]; ok {
			t.Errorf("%s: response leaked 'token_hash'", label)
		}
	}

	// GET list
	resp, body := doJSON(t, "GET", srv.URL+"/api/projects", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d", resp.StatusCode)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 project, got %d", len(list))
	}
	assertNoTokenKeys("list[0]", list[0])

	// GET single
	resp, body = doJSON(t, "GET", srv.URL+"/api/projects/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d", resp.StatusCode)
	}
	var single map[string]json.RawMessage
	if err := json.Unmarshal(body, &single); err != nil {
		t.Fatalf("decode single: %v", err)
	}
	assertNoTokenKeys("single", single)

	// rotate_token MUST return the new plaintext — the one sanctioned exception.
	resp, body = doJSON(t, "POST", srv.URL+"/api/projects/"+id+"/rotate_token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: status %d", resp.StatusCode)
	}
	var rotated map[string]json.RawMessage
	if err := json.Unmarshal(body, &rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if _, ok := rotated["token"]; !ok {
		t.Error("rotate_token response must include the new token")
	}
}
