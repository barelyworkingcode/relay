package main

import (
	"context"
	"strings"
	"testing"

	"relaygo/bridge"
)

// remoteProjectRouter returns a router whose settings hold one remote project.
// The project is built directly rather than through the create path so these
// guards are proven independently of validation — the whole point of a second
// line of defence is that it holds when the first one didn't run.
func remoteProjectRouter(t *testing.T) (*appRouter, string) {
	t.Helper()
	s := makeSettings(nil, nil, nil)
	s.Projects = append(s.Projects, Project{
		ID:            "remote-proj",
		Name:          "remote",
		Kind:          ProjectKindRemote,
		AllowedMcpIDs: []string{},
		Token:         "remote-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TokenHash:     hashToken("remote-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		// Carried from a former life as a local project. Validation would
		// refuse this today; the guard must not depend on that.
		ShellTemplates: []ShellTemplate{{ID: "tpl-1", Name: "ssh", Command: "/usr/bin/ssh"}},
	})
	r := newTestRouter(t, s, NewExternalMcpManager(nil))
	svcToken := "service-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	r.serviceTokens.Register(hashToken(svcToken))
	return r, svcToken
}

// The hole this closes: dirWithinProject("", "") returns true, because the
// empty-dir branch is evaluated before the empty-project-path branch and
// short-circuits it. Without an explicit refusal the call SUCCEEDS and hands
// back the remote project's plaintext token with WorkingDir: "", which Go's
// exec.Cmd resolves to relay's own working directory.
func TestResolvePtyEnv_RefusesRemoteProject(t *testing.T) {
	r, svcToken := remoteProjectRouter(t)

	resp, err := r.ResolvePtyEnv(context.Background(),
		bridge.PtyEnvRequest{ProjectID: "remote-proj", Directory: ""}, svcToken)
	if err == nil {
		t.Fatalf("remote project PTY launch was permitted, returned working_dir=%q token_len=%d",
			resp.WorkingDir, len(resp.RelayToken))
	}
	if resp.RelayToken != "" {
		t.Error("a refused PTY launch still returned a project token")
	}
	if !strings.Contains(err.Error(), "remote project") {
		t.Errorf("error should name the reason, got: %v", err)
	}
}

// The legacy resolution branch (no ProjectID, matched by name) must refuse too,
// or the guard is bypassable by using the older request shape.
func TestResolvePtyEnv_RefusesRemoteProjectViaLegacyPath(t *testing.T) {
	r, svcToken := remoteProjectRouter(t)

	if _, err := r.ResolvePtyEnv(context.Background(),
		bridge.PtyEnvRequest{Project: "remote"}, svcToken); err == nil {
		t.Fatal("legacy project-name resolution let a remote project through")
	}
}

// A local project must still resolve normally — the guard must not have
// tightened the everyday path.
func TestResolvePtyEnv_LocalProjectStillResolves(t *testing.T) {
	dir := t.TempDir()
	s := makeSettings(nil, nil, nil)
	s.Projects[0].Path = dir
	r := newTestRouter(t, s, NewExternalMcpManager(nil))
	svcToken := "service-token-cccccccccccccccccccccccccccccccc"
	r.serviceTokens.Register(hashToken(svcToken))

	resp, err := r.ResolvePtyEnv(context.Background(),
		bridge.PtyEnvRequest{ProjectID: "test-project", Directory: dir}, svcToken)
	if err != nil {
		t.Fatalf("local project PTY launch was refused: %v", err)
	}
	if resp.WorkingDir != dir || resp.RelayToken == "" {
		t.Errorf("local resolve returned working_dir=%q token_empty=%v", resp.WorkingDir, resp.RelayToken == "")
	}
}

func TestResolveProjectTemplate_RefusesRemoteProject(t *testing.T) {
	r, svcToken := remoteProjectRouter(t)

	// The fixture deliberately carries a shell template, so a missing guard
	// would resolve it rather than falling through to "not found".
	resp, err := r.ResolveProjectTemplate(context.Background(),
		bridge.ShellTemplateRequest{ProjectID: "remote-proj", TemplateID: "tpl-1"}, svcToken)
	if err == nil {
		t.Fatalf("remote project resolved a host shell template: command=%q", resp.Command)
	}
	if resp.Command != "" {
		t.Errorf("a refused template resolve still returned a command: %q", resp.Command)
	}
}
