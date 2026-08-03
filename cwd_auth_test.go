package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// Directory auth (Project.AllowCwdAuth): a tokenless bridge caller is resolved
// to a project by its working directory, but only for projects that opted in.
// Every failure mode must land on "no access" — the danger in this feature is a
// mismatch that silently widens scope, so the tests below lean on that.

// cwdProject builds a Settings with one opted-in project rooted at path.
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

// A project nested inside another wins for its own subtree; the outer project
// still owns everything above it.
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

// A nested project that did NOT opt in must not shadow an opted-in parent:
// the longest match is only computed among participants, so the parent's grant
// still applies inside the nested path.
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

// Directory auth must grant exactly what the token grants — same permissions,
// same disabled tools, same context. Only the identification differs.
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

// ---------------------------------------------------------------------------
// Router integration
// ---------------------------------------------------------------------------

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

// A wrong token must fail even from a directory that would have authenticated:
// the fallback covers the absence of a credential, never a bad one.
func TestResolveAuth_BadTokenNotRescuedByCwd(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir, true), NewExternalMcpManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, _, err := r.resolveAuth(ctx, "not-the-right-token"); err == nil {
		t.Fatal("expected a bad token to fail regardless of cwd")
	}
}

// Service-token operations must be unreachable through directory auth, which
// only ever yields a project-scoped token.
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

// ListTools over the full router: the tool surface a tokenless caller sees from
// inside the project must equal what the project's token sees.
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
