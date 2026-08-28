package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

func cwdProject(t *testing.T, path string, allowCwd bool) *Settings {
	t.Helper()
	s := makeSettings(nil, nil, nil)
	s.Projects[0].Path = path
	s.Projects[0].AllowCwdAuth = allowCwd
	return s
}

func TestAuthenticateProjectByPath_OptedIn(t *testing.T) {
	dir := t.TempDir()
	s := cwdProject(t, dir, true)

	for _, cwd := range []string{dir, filepath.Join(dir, "sub", "deeper")} {
		stored := s.AuthenticateProjectByPath(cwd)
		if stored == nil {
			t.Fatalf("cwd %q: expected a StoredToken", cwd)
		}
		if stored.ProjectID != "test-project" {
			t.Errorf("cwd %q: project id = %q, want test-project", cwd, stored.ProjectID)
		}
	}
}

func TestAuthenticateProjectByPath_RequiresOptIn(t *testing.T) {
	dir := t.TempDir()
	s := cwdProject(t, dir, false)

	if stored := s.AuthenticateProjectByPath(dir); stored != nil {
		t.Fatalf("expected nil for a project that did not opt in, got %+v", stored)
	}
}

func TestAuthenticateProjectByPath_NoMatch(t *testing.T) {
	s := cwdProject(t, t.TempDir(), true)

	cases := map[string]string{
		"empty cwd":       "",
		"unrelated dir":   t.TempDir(),
		"parent of proj":  filepath.Dir(s.Projects[0].Path),
		"sibling prefix":  s.Projects[0].Path + "-other",
		"escaping suffix": filepath.Join(s.Projects[0].Path, "..", "elsewhere"),
	}
	for name, cwd := range cases {
		if stored := s.AuthenticateProjectByPath(cwd); stored != nil {
			t.Errorf("%s (%q): expected nil, got project %q", name, cwd, stored.ProjectID)
		}
	}
}

func TestAuthenticateProjectByPath_NestedLongestMatch(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "packages", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := cwdProject(t, outer, true)
	s.Projects = append(s.Projects, Project{
		ID:            "inner-project",
		Name:          "inner",
		Path:          inner,
		AllowedMcpIDs: []string{"*"},
		AllowCwdAuth:  true,
	})

	if got := s.AuthenticateProjectByPath(inner); got == nil || got.ProjectID != "inner-project" {
		t.Errorf("inner dir resolved to %v, want inner-project", got)
	}
	if got := s.AuthenticateProjectByPath(filepath.Join(outer, "docs")); got == nil || got.ProjectID != "test-project" {
		t.Errorf("outer dir resolved to %v, want test-project", got)
	}
}

// The longest match is computed only among opted-in participants, so a
// nested project that did NOT opt in cannot shadow an opted-in parent by
// virtue of its longer path.
func TestAuthenticateProjectByPath_NestedOptOutDoesNotShadow(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := cwdProject(t, outer, true)
	s.Projects = append(s.Projects, Project{
		ID:            "inner-project",
		Name:          "inner",
		Path:          inner,
		AllowedMcpIDs: []string{"*"},
		AllowCwdAuth:  false,
	})

	got := s.AuthenticateProjectByPath(inner)
	if got == nil || got.ProjectID != "test-project" {
		t.Fatalf("resolved to %v, want the opted-in parent test-project", got)
	}
}

func TestAuthenticateProjectByPath_ScopeMatchesTokenAuth(t *testing.T) {
	dir := t.TempDir()
	s := makeSettings(
		map[string]Permission{"fsmcp": PermOn, "macmcp": PermOff},
		map[string][]string{"fsmcp": {"write_file"}},
		map[string]json.RawMessage{"fsmcp": json.RawMessage(`{"allowed_dirs":["/x"]}`)},
	)
	s.Projects[0].Path = dir
	s.Projects[0].AllowCwdAuth = true

	byToken := s.AuthenticateProjectByHash(hashToken(testToken))
	byPath := s.AuthenticateProjectByPath(filepath.Join(dir, "sub"))
	if byToken == nil || byPath == nil {
		t.Fatal("both auth paths must resolve")
	}

	wantJSON, _ := json.Marshal(byToken)
	gotJSON, _ := json.Marshal(byPath)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("scope differs between auth paths:\n token: %s\n  path: %s", wantJSON, gotJSON)
	}
}

func TestResolveAuth_CwdFallback(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir, true), NewExternalMcpManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), filepath.Join(dir, "nested"))
	stored, settings, err := r.resolveAuth(ctx, "")
	if err != nil {
		t.Fatalf("expected cwd auth to succeed, got %v", err)
	}
	if stored.ProjectID != "test-project" {
		t.Errorf("project id = %q, want test-project", stored.ProjectID)
	}
	if settings == nil {
		t.Fatal("expected non-nil Settings")
	}
}

func TestResolveAuth_CwdFallbackDeniedWithoutOptIn(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir, false), NewExternalMcpManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	_, _, err := r.resolveAuth(ctx, "")
	if err == nil {
		t.Fatal("expected denial for a project without allow_cwd_auth")
	}
	var coded *jsonrpc.CodedError
	if !errors.As(err, &coded) || coded.RPCCode != jsonrpc.CodeUnauthorized {
		t.Errorf("expected CodeUnauthorized, got %v", err)
	}
}

func TestResolveAuth_BadTokenNotRescuedByCwd(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir, true), NewExternalMcpManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, _, err := r.resolveAuth(ctx, "not-the-right-token"); err == nil {
		t.Fatal("expected a bad token to fail regardless of cwd")
	}
}

func TestResolveCwdAuth_CannotSatisfyServiceOps(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir, true), NewExternalMcpManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, err := r.ResolvePtyEnv(ctx, bridge.PtyEnvRequest{ProjectID: "test-project"}, ""); err == nil {
		t.Fatal("expected ResolvePtyEnv to reject a tokenless caller")
	}
	if _, err := r.ListProjects(""); err == nil {
		t.Fatal("expected ListProjects to reject a tokenless caller")
	}
}

func TestListTools_CwdAuthMatchesTokenSurface(t *testing.T) {
	dir := t.TempDir()
	mock := newMockConn("fsmcp", simpleTools("read_file", "write_file"), nil)
	r := setupRouter(t,
		map[string]Permission{"fsmcp": PermOn},
		map[string][]string{"fsmcp": {"write_file"}},
		nil,
		map[string]*mockMcpConn{"fsmcp": mock},
	)
	if err := r.store.With(func(s *Settings) {
		s.Projects[0].Path = dir
		s.Projects[0].AllowCwdAuth = true
	}); err != nil {
		t.Fatalf("settings mutation: %v", err)
	}

	byToken, err := r.ListTools(context.Background(), testToken)
	if err != nil {
		t.Fatalf("token ListTools: %v", err)
	}
	byCwd, err := r.ListTools(bridge.WithCallerCwd(context.Background(), dir), "")
	if err != nil {
		t.Fatalf("cwd ListTools: %v", err)
	}
	if string(byToken) != string(byCwd) {
		t.Errorf("tool surface differs:\n token: %s\n   cwd: %s", byToken, byCwd)
	}
	// The disabled tool must be absent from both — a sanity check that the
	// comparison above isn't comparing two empty lists.
	tools := unmarshalTools(t, byCwd)
	if len(tools) != 1 || tools[0].Name != "read_file" {
		t.Errorf("expected only read_file, got %+v", tools)
	}
}

// Skipped on case-sensitive volumes, where a case variant is genuinely a
// different directory rather than an alias for the same one.
func TestDirWithinProject_CaseInsensitiveVolume(t *testing.T) {
	// A named element, not t.TempDir()'s numeric leaf — digits have no case.
	dir := filepath.Join(t.TempDir(), "ProjectDir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	variant := caseVariant(dir)
	if variant == "" {
		t.Skip("case-sensitive volume: no case variant resolves to the same directory")
	}

	if !dirWithinProject(dir, variant) {
		t.Errorf("dir %q not matched against case-variant project path %q", dir, variant)
	}
	if !dirWithinProject(filepath.Join(dir, "sub"), variant) {
		t.Errorf("subdirectory of %q not matched against %q", dir, variant)
	}
}

func TestDirWithinProject_IdentityRejectsOutsiders(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()

	if dirWithinProject(other, proj) {
		t.Errorf("unrelated dir %q matched project %q", other, proj)
	}
	if dirWithinProject(filepath.Dir(proj), proj) {
		t.Errorf("parent of %q matched the project itself", proj)
	}
	// A path that doesn't exist yet still resolves textually.
	if !dirWithinProject(filepath.Join(proj, "not", "created", "yet"), proj) {
		t.Errorf("non-existent nested path should still match textually")
	}
}

// caseVariant returns a case-flipped form of dir's last element that stats to
// the same directory, or "" when the volume is case-sensitive.
func caseVariant(dir string) string {
	base := filepath.Base(dir)
	flipped := strings.ToUpper(base)
	if flipped == base {
		flipped = strings.ToLower(base)
	}
	if flipped == base {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(dir), flipped)
	a, err1 := os.Stat(dir)
	b, err2 := os.Stat(candidate)
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		return ""
	}
	return candidate
}
