package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// newPtyTestRouter builds a real appRouter backed by a sandboxed settings store
// containing a single project, plus a planted service token. Returns the router,
// the created project, and the plaintext service token.
func newPtyTestRouter(t *testing.T) (*appRouter, Project, string) {
	t.Helper()
	mkSandboxRelayHome(t)

	store := NewSettingsStoreAt(bridge.ConfigDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	var proj Project
	var createErr error
	if err := store.With(func(s *Settings) {
		proj, createErr = s.CreateProjectWithToken("PtyProj", t.TempDir(), nil, nil, nil, nil)
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	if createErr != nil {
		t.Fatalf("CreateProjectWithToken: %v", createErr)
	}

	router := &appRouter{
		store:    store,
		tools:    NewExternalMcpManager(nil),
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
	}
	const svcToken = "svc-token-pty-test"
	router.serviceTokens.Register(hashToken(svcToken))
	return router, proj, svcToken
}

func codeOf(err error) int {
	var ce *jsonrpc.CodedError
	if errors.As(err, &ce) {
		return ce.RPCCode
	}
	return 0
}

func TestResolvePtyEnv_ByProjectID(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)

	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID:   proj.ID,
		Directory:   proj.Path,
		RegenSkills: bridge.RegenSkillsNever,
	}, svcToken)
	if err != nil {
		t.Fatalf("ResolvePtyEnv: %v", err)
	}
	if resp.RelayToken != proj.Token {
		t.Errorf("RelayToken = %q, want the project token", resp.RelayToken)
	}
	if resp.WorkingDir != proj.Path {
		t.Errorf("WorkingDir = %q, want %q", resp.WorkingDir, proj.Path)
	}
}

func TestResolvePtyEnv_ByProjectID_AcceptsSubdir(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)

	sub := filepath.Join(proj.Path, "nested", "pkg")
	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID:   proj.ID,
		Directory:   sub,
		RegenSkills: bridge.RegenSkillsNever,
	}, svcToken)
	if err != nil {
		t.Fatalf("ResolvePtyEnv (subdir): %v", err)
	}
	if resp.RelayToken != proj.Token {
		t.Errorf("RelayToken = %q, want the project token", resp.RelayToken)
	}
}

func TestResolvePtyEnv_DirectoryMismatch_Rejected(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)

	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID:   proj.ID,
		Directory:   t.TempDir(), // a different tree, not within proj.Path
		RegenSkills: bridge.RegenSkillsNever,
	}, svcToken)
	if err == nil {
		t.Fatal("expected rejection for directory outside the project")
	}
	if code := codeOf(err); code != jsonrpc.CodeInvalidParams {
		t.Errorf("error code = %d, want CodeInvalidParams (%d)", code, jsonrpc.CodeInvalidParams)
	}
	// The token must never leak, even on the error path.
	if resp.RelayToken != "" {
		t.Errorf("token leaked on rejection: %q", resp.RelayToken)
	}
}

func TestResolvePtyEnv_UnknownProjectID(t *testing.T) {
	router, _, svcToken := newPtyTestRouter(t)

	_, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID:   "does-not-exist",
		RegenSkills: bridge.RegenSkillsNever,
	}, svcToken)
	if code := codeOf(err); code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want CodeMethodNotFound (%d)", code, jsonrpc.CodeMethodNotFound)
	}
}

func TestResolvePtyEnv_RequiresServiceToken(t *testing.T) {
	router, proj, _ := newPtyTestRouter(t)

	// A project token (not a service token) must be rejected.
	_, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID:   proj.ID,
		Directory:   proj.Path,
		RegenSkills: bridge.RegenSkillsNever,
	}, proj.Token)
	if code := codeOf(err); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
	}
}

func TestResolvePtyEnv_LegacyDirectoryMatchStillWorks(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)

	// No ProjectID: relay falls back to matching Directory against Project.Path.
	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		Directory:   proj.Path,
		RegenSkills: bridge.RegenSkillsNever,
	}, svcToken)
	if err != nil {
		t.Fatalf("legacy directory-match resolve: %v", err)
	}
	if resp.RelayToken != proj.Token {
		t.Errorf("RelayToken = %q, want the project token", resp.RelayToken)
	}
}

// TestDirWithinProject_ResolvesSymlinks guards the macOS /var→/private/var (and
// /tmp) gotcha: a project stored under the symlink form must accept a directory
// expressed in the resolved form of the same location.
func TestDirWithinProject_ResolvesSymlinks(t *testing.T) {
	tmp := t.TempDir() // macOS: under /var/folders (a symlink to /private/var/folders)
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil || real == tmp {
		t.Skip("temp dir has no symlink component on this platform")
	}
	// project = symlink form; dir = resolved-form subdir of the same place.
	if !dirWithinProject(filepath.Join(real, "sub"), tmp) {
		t.Errorf("symlink-equivalent subdir wrongly rejected: dir=%q project=%q", filepath.Join(real, "sub"), tmp)
	}
	// And the reverse orientation.
	if !dirWithinProject(filepath.Join(tmp, "sub"), real) {
		t.Errorf("symlink-equivalent subdir wrongly rejected (reverse): dir=%q project=%q", filepath.Join(tmp, "sub"), real)
	}
}

func TestDirWithinProject(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		project string
		want    bool
	}{
		{"empty dir is allowed (no cwd to validate)", "", "/home/p", true},
		{"exact match", "/home/p", "/home/p", true},
		{"subdir", "/home/p/sub/pkg", "/home/p", true},
		{"trailing-slash exact", "/home/p/", "/home/p", true},
		{"sibling escape", "/home/other", "/home/p", false},
		{"parent escape", "/home", "/home/p", false},
		{"dotdot traversal", "/home/p/../other", "/home/p", false},
		{"prefix-not-subdir", "/home/project2", "/home/project", false},
		{"empty project path", "/home/p", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirWithinProject(tc.dir, tc.project); got != tc.want {
				t.Errorf("dirWithinProject(%q, %q) = %v, want %v", tc.dir, tc.project, got, tc.want)
			}
		})
	}
}
