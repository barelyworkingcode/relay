//go:build darwin

package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveMcpBridgeSocket_Precedence(t *testing.T) {
	configDir := mkSandboxRelayHome(t)
	flagSock := filepath.Join(configDir, "relay.sock")
	const envSock = "/tmp/acme-env/relay.sock"

	for _, tc := range []struct {
		name       string
		explicit   bool
		env        string
		wantPath   string
		wantSource string
		wantErr    bool
	}{
		{"flag outranks env", true, envSock, flagSock, "--config-dir", false},
		{"flag outranks a relative env value", true, "relay.sock", flagSock, "--config-dir", false},
		{"absolute env", false, envSock, envSock, "RELAY_BRIDGE_SOCKET", false},
		{"relative env refused", false, "relay.sock", "", "", true},
		{"empty env falls to default", false, "", flagSock, "default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "RELAY_BRIDGE_SOCKET" {
					return tc.env
				}
				return ""
			}
			path, source, err := resolveMcpBridgeSocket(tc.explicit, getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q from %q, want an error", path, source)
				}
				return
			}
			if err != nil || path != tc.wantPath || source != tc.wantSource {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, nil)", path, source, err, tc.wantPath, tc.wantSource)
			}
		})
	}
}

func TestApplyConfigDirFlag_ExplicitOnlyWhenNonEmpty(t *testing.T) {
	mkSandboxRelayHome(t)
	dir := mkShortTempDir(t, "relay-flag-")

	for _, tc := range []struct {
		name         string
		args         []string
		wantRest     []string
		wantExplicit bool
	}{
		{"separate value", []string{"--config-dir", dir, "mcp"}, []string{"mcp"}, true},
		{"equals value", []string{"--config-dir=" + dir, "mcp"}, []string{"mcp"}, true},
		{"empty equals value", []string{"--config-dir=", "mcp"}, []string{"mcp"}, false},
		{"no flag", []string{"mcp"}, []string{"mcp"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, explicit := applyConfigDirFlag(tc.args)
			if !slices.Equal(rest, tc.wantRest) || explicit != tc.wantExplicit {
				t.Fatalf("applyConfigDirFlag(%q) = (%q, %v), want (%q, %v)", tc.args, rest, explicit, tc.wantRest, tc.wantExplicit)
			}
		})
	}
}
