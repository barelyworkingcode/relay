package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/project"
)

func cwdProject(t *testing.T, path string) *config.Settings {
	t.Helper()
	s := makeSettings(nil, nil, nil)
	s.Projects[0].Path = path
	return s
}

// allow_cwd_auth is retired (plan-broker-and-sessions.md §2 C3): the field
// is gone from config.Project, and project.AuthenticateByPath is kept only
// as an always-nil stub so router.go's resolveCwdAuth (R-S2a's exclusive
// file this wave) keeps compiling unchanged. These tests pin that a caller
// asserting a matching cwd never resolves a project, regardless of the
// directory relationship that used to grant it.
func TestAuthenticateByPath_AlwaysNil(t *testing.T) {
	dir := t.TempDir()
	s := cwdProject(t, dir)

	for _, cwd := range []string{"", dir, filepath.Join(dir, "sub", "deeper")} {
		if stored := project.AuthenticateByPath(s, cwd); stored != nil {
			t.Errorf("cwd %q: expected nil, got %+v", cwd, stored)
		}
	}
}

func TestResolveAuth_CwdNeverGrantsAccess(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir), mcpbroker.NewManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), filepath.Join(dir, "nested"))
	_, _, err := r.resolveAuth(ctx, "")
	if err == nil {
		t.Fatal("expected a tokenless caller asserting only a cwd to be refused")
	}
	var coded *jsonrpc.CodedError
	if !errors.As(err, &coded) || coded.RPCCode != jsonrpc.CodeUnauthorized {
		t.Errorf("expected CodeUnauthorized, got %v", err)
	}
}

func TestResolveAuth_BadTokenNotRescuedByCwd(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir), mcpbroker.NewManager(nil))

	ctx := bridge.WithCallerCwd(context.Background(), dir)
	if _, _, err := r.resolveAuth(ctx, "not-the-right-token"); err == nil {
		t.Fatal("expected a bad token to fail regardless of cwd")
	}
}

func TestResolveCwdAuth_CannotSatisfyServiceOps(t *testing.T) {
	dir := t.TempDir()
	r := newTestRouter(t, cwdProject(t, dir), mcpbroker.NewManager(nil))

	// A tokenless caller asserting only a cwd must still be refused any
	// operation that requires a service's launch identity.
	ctx := bridge.WithCallerCwd(context.Background(), dir)
	req := bridge.RegisterManifestRequest{ServiceID: "svc", InternalSocket: "/tmp/x.sock", InternalToken: "t", Manifest: bridge.Manifest{Routes: []string{"/api/svc/"}}}
	if err := r.RegisterManifest(ctx, req, ""); err == nil {
		t.Fatal("expected RegisterManifest to reject a tokenless cwd-asserting caller")
	}
}

func TestListTools_CwdAloneIsRefused(t *testing.T) {
	dir := t.TempDir()
	mock := newMockConn("fsmcp", simpleTools("read_file", "write_file"), nil)
	r := setupRouter(t,
		map[string]config.Permission{"fsmcp": config.PermOn},
		map[string][]string{"fsmcp": {"write_file"}},
		nil,
		map[string]*mockMcpConn{"fsmcp": mock},
	)
	if err := r.store.With(func(s *config.Settings) {
		s.Projects[0].Path = dir
	}); err != nil {
		t.Fatalf("settings mutation: %v", err)
	}

	if _, err := r.ListTools(context.Background(), testToken); err != nil {
		t.Fatalf("token ListTools: %v", err)
	}
	if _, err := r.ListTools(bridge.WithCallerCwd(context.Background(), dir), ""); err == nil {
		t.Fatal("expected a tokenless cwd-only ListTools to be refused")
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

	if !project.DirWithin(dir, variant) {
		t.Errorf("dir %q not matched against case-variant project path %q", dir, variant)
	}
	if !project.DirWithin(filepath.Join(dir, "sub"), variant) {
		t.Errorf("subdirectory of %q not matched against %q", dir, variant)
	}
}

func TestDirWithinProject_IdentityRejectsOutsiders(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()

	if project.DirWithin(other, proj) {
		t.Errorf("unrelated dir %q matched project %q", other, proj)
	}
	if project.DirWithin(filepath.Dir(proj), proj) {
		t.Errorf("parent of %q matched the project itself", proj)
	}
	// A path that doesn't exist yet still resolves textually.
	if !project.DirWithin(filepath.Join(proj, "not", "created", "yet"), proj) {
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
