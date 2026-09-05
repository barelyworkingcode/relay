package main

import (
	"context"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/project"
)

// newHostPtyTestRouter builds a router with one host and one project bound
// to it, mirroring newPtyTestRouter's shape for the console case.
func newHostPtyTestRouter(t *testing.T, probe *config.HostProbe) (*appRouter, config.Project, config.Host, string) {
	t.Helper()
	mkSandboxRelayHome(t)

	store := sealedSettingsStoreAt(bridge.ConfigDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	var host config.Host
	var proj config.Project
	if err := store.With(func(s *config.Settings) {
		var err error
		host, err = s.AddHost(config.Host{Name: "devbox", Target: "admin@devbox.local"})
		if err != nil {
			t.Fatalf("AddHost: %v", err)
		}
		if probe != nil {
			s.SetHostProbe(host.ID, *probe)
			if h, _ := config.FindHostByID(s, host.ID); h != nil {
				host = *h
			}
		}
		proj, err = project.ApplyCreate(s, project.CreateFields{
			Name:   "relayfs",
			Path:   "/home/admin/src/relayfs",
			HostID: host.ID,
		}, project.McpSurfaces{})
		if err != nil {
			t.Fatalf("ApplyCreate: %v", err)
		}
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}

	router := &appRouter{
		store:    store,
		tools:    mcpbroker.NewManager(nil),
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
	}
	const svcToken = "svc-token-host-pty-test"
	router.serviceTokens.Register(config.HashToken(svcToken))
	return router, proj, host, svcToken
}

func TestResolvePtyEnv_HostProject_NoTokenAndHostSpec(t *testing.T) {
	probe := &config.HostProbe{
		OK: true, OS: "Darwin", Shell: "/bin/zsh",
		NodePath: "/opt/homebrew/bin/node", ClaudePath: "/opt/homebrew/bin/claude",
	}
	router, proj, host, svcToken := newHostPtyTestRouter(t, probe)

	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID: proj.ID,
		Directory: proj.Path,
	}, svcToken)
	if err != nil {
		t.Fatalf("ResolvePtyEnv: %v", err)
	}
	if resp.RelayToken != "" {
		t.Errorf("RelayToken = %q, want empty for a host project", resp.RelayToken)
	}
	if resp.WorkingDir != proj.Path {
		t.Errorf("WorkingDir = %q, want %q", resp.WorkingDir, proj.Path)
	}
	if resp.Host == nil {
		t.Fatal("expected a HostSpec")
	}
	if resp.Host.ID != host.ID || resp.Host.Name != host.Name {
		t.Errorf("HostSpec id/name = %q/%q, want %q/%q", resp.Host.ID, resp.Host.Name, host.ID, host.Name)
	}
	if resp.Host.ClaudePath != "/opt/homebrew/bin/claude" || resp.Host.NodePath != "/opt/homebrew/bin/node" {
		t.Errorf("unexpected HostSpec paths: %+v", resp.Host)
	}
	if len(resp.Host.SSHArgv) == 0 || resp.Host.SSHArgv[0] != "ssh" {
		t.Errorf("expected a real ssh argv prefix, got %v", resp.Host.SSHArgv)
	}
}

func TestResolvePtyEnv_HostProject_SubdirAccepted(t *testing.T) {
	probe := &config.HostProbe{OK: true, ClaudePath: "/opt/homebrew/bin/claude"}
	router, proj, _, svcToken := newHostPtyTestRouter(t, probe)

	resp, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID: proj.ID,
		Directory: proj.Path + "/nested/pkg",
	}, svcToken)
	if err != nil {
		t.Fatalf("ResolvePtyEnv: %v", err)
	}
	if resp.Host == nil {
		t.Fatal("expected a HostSpec")
	}
}

func TestResolvePtyEnv_HostProject_DirectoryOutsideRejected(t *testing.T) {
	probe := &config.HostProbe{OK: true, ClaudePath: "/opt/homebrew/bin/claude"}
	router, proj, _, svcToken := newHostPtyTestRouter(t, probe)

	_, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID: proj.ID,
		Directory: "/some/other/place",
	}, svcToken)
	if err == nil {
		t.Fatal("expected an error for a directory outside the host project's path")
	}
	if code := codeOf(err); code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want CodeInvalidParams", code)
	}
}

func TestResolvePtyEnv_HostProject_NoClaudeRefused(t *testing.T) {
	router, proj, host, svcToken := newHostPtyTestRouter(t, nil) // never probed
	_ = host

	_, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID: proj.ID,
		Directory: proj.Path,
	}, svcToken)
	if err == nil {
		t.Fatal("expected an error: host has never been probed")
	}
	if code := codeOf(err); code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want CodeInvalidParams", code)
	}
}

func TestResolvePtyEnv_HostProject_ProbeFailedRefused(t *testing.T) {
	probe := &config.HostProbe{OK: false, Error: "connection refused"}
	router, proj, _, svcToken := newHostPtyTestRouter(t, probe)

	_, err := router.ResolvePtyEnv(context.Background(), bridge.PtyEnvRequest{
		ProjectID: proj.ID,
		Directory: proj.Path,
	}, svcToken)
	if err == nil {
		t.Fatal("expected an error: last probe failed, so claude_path is empty")
	}
}
