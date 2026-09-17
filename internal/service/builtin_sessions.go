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

// RelaySessionsInternalSocketPath and RelaySessionsHookSocketPath are the
// two socket paths the built-in record passes on relay-sessions' command
// line. They are functions rather than inline strings because relay's
// session sandbox generator (C7) has to name the hook socket as the one
// socket in relay's own directory a session may connect to: a rename that
// reached only one of the two would leave the sandbox denying the socket the
// host actually serves, which reads as Claude Code's permission hook timing
// out rather than as a path mismatch.
func RelaySessionsInternalSocketPath(configDir string) string {
	return filepath.Join(configDir, "relaysessions-internal.sock")
}

func RelaySessionsHookSocketPath(configDir string) string {
	return filepath.Join(configDir, "relaysessions-hook.sock")
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
			"-internal-socket", RelaySessionsInternalSocketPath(configDir),
			"-hook-socket", RelaySessionsHookSocketPath(configDir),
		},
		Autostart:    autostart,
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions},
	}
}

// EnsureBuiltinRelaySessionsService returns services with the built-in
// relaysessions record synthesized in place of whatever settings.json held
// for that id. Autostart is the only value a stored record contributes; a
// settings.json with no relaysessions record yet gets one with autostart
// true -- an install that has never decided turns the built-in session host
// on by default. Does not mutate services; safe to call on a clone of the
// live settings before StartAllAutostart / ReclaimOrphans see it.
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

// EnsureBuiltinRelaySessionsRecord adds a relaysessions record to s.Services
// iff none exists yet, carrying DisplayName, Autostart and Capabilities but
// never Command or Args -- sanitizeIfBuiltin strips those from a stored
// record on every load regardless, so writing them here would be dead
// weight, not a shortcut. Without a stored record at all, ServiceOps.List
// and SetAutostart (cmd/relay/service_ops.go, which read settings.json
// directly rather than through EnsureBuiltinRelaySessionsService's
// in-memory synthesis) have nothing to act on: `relay service list` never
// shows the session host, and there is no way to turn its autostart back
// off. Idempotent and safe to call every start: an existing record, of any
// Autostart value, is left untouched.
func EnsureBuiltinRelaySessionsRecord(s *config.Settings) {
	if svc, _ := config.FindServiceByID(s, config.RelaySessionsServiceID); svc != nil {
		return
	}
	s.AddService(config.ServiceConfig{
		ID:           config.RelaySessionsServiceID,
		DisplayName:  "Session Host",
		Autostart:    true,
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions},
	})
}
