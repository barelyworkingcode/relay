package main

import (
	"strings"
	"testing"
)

// TestListProjectsAndGetProject_NeverCarryTokenOrHash pins AC-8 / issue
// #64: ListProjects and GetProject are answered to any service
// holding the projects capability, and must never marshal the raw Project — only ResolvePtyEnv may
// hand out a project's plaintext token over the bridge. Checked by string
// search on the raw JSON, not by decoding into a Project (which would
// silently pass by coincidence if the DTO ever gained a differently-named
// but equally revealing field).
func TestListProjectsAndGetProject_NeverCarryTokenOrHash(t *testing.T) {
	router, proj, svcCtx := newPtyTestRouter(t)

	list, err := router.ListProjects(svcCtx, "")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	assertNoTokenFields(t, "ListProjects", list)

	got, err := router.GetProject(svcCtx, proj.ID, "")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	assertNoTokenFields(t, "GetProject", got)

	// Sanity: the fixture project really does carry a live token and
	// hash, and neither response happens to include the project at all
	// (which would make the two checks above vacuous).
	plaintext, ok := proj.Token.Reveal()
	if !ok || plaintext == "" {
		t.Fatal("fixture project has no plaintext token; this test proves nothing")
	}
	if !strings.Contains(string(list), proj.ID) {
		t.Fatalf("ListProjects response does not even mention the fixture project: %s", list)
	}
	if !strings.Contains(string(got), proj.ID) {
		t.Fatalf("GetProject response does not even mention the fixture project: %s", got)
	}
	if strings.Contains(string(list), plaintext) || strings.Contains(string(got), plaintext) {
		t.Fatal("a bridge response contains the project's plaintext token")
	}
}

func assertNoTokenFields(t *testing.T, who string, raw []byte) {
	t.Helper()
	body := string(raw)
	for _, field := range []string{`"token"`, `"token_hash"`} {
		if strings.Contains(body, field) {
			t.Errorf("%s response contains %s: %s", who, field, body)
		}
	}
}
