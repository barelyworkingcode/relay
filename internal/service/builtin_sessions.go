package service

import (
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/config"
)

// RelaySessionsHelperPath resolves the built-in relay-sessions helper's
// path from relay's own bundle (spec-session-host.md §2.1):
// Contents/MacOS/relay -> Contents/Helpers/relay-sessions. relayBin is
// relay's own resolved executable path, the same value startLocked already
// computes for EnvMcpCommand.
func RelaySessionsHelperPath(relayBin string) string {
	return filepath.Join(filepath.Dir(relayBin), "..", "Helpers", "relay-sessions")
}

// BuiltinRelaySessionsService returns the synthesized service record for
// relay-sessions (SH §2.1): built in, never user-registered. Command, Args,
// DisplayName and Capabilities are always exactly these values, resolved
// fresh from relay's own bundle and config dir -- never read from
// settings.json (internal/config's sanitizeIfBuiltin strips a stored
// Command on load, defense in depth for a caller that reads s.Services
// directly instead of going through this function). autostart is the one
// value SH §2.1 leaves to the stored record.
func BuiltinRelaySessionsService(relayBin, configDir string, autostart bool) config.ServiceConfig {
	return config.ServiceConfig{
		ID:          config.RelaySessionsServiceID,
		DisplayName: "Session Host",
		Command:     RelaySessionsHelperPath(relayBin),
		Args: []string{
			"service",
			"-internal-socket", filepath.Join(configDir, "relaysessions-internal.sock"),
			"-hook-socket", filepath.Join(configDir, "relaysessions-hook.sock"),
		},
		Autostart:    autostart,
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions},
	}
}

// EnsureBuiltinRelaySessionsService returns services with the built-in
// relaysessions record synthesized in place of whatever settings.json held
// for that id. Autostart is the only value a stored record contributes; a
// settings.json with no relaysessions record yet gets one with autostart
// true, the same "an install that has never decided turns the feature on"
// default ensureDefaultModelEndpoint applies to the model broker's TCP
// listener -- both ride in on the same build. Does not mutate services;
// safe to call on a clone of the live settings before StartAllAutostart /
// ReclaimOrphans see it.
func EnsureBuiltinRelaySessionsService(services []config.ServiceConfig, relayBin, configDir string) []config.ServiceConfig {
	autostart := true
	out := make([]config.ServiceConfig, 0, len(services)+1)
	for _, svc := range services {
		if svc.ID == config.RelaySessionsServiceID {
			autostart = svc.Autostart
			continue
		}
		out = append(out, svc)
	}
	return append(out, BuiltinRelaySessionsService(relayBin, configDir, autostart))
}
