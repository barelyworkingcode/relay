package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"relaygo/bridge"
)

// SettingsStore manages settings persistence, caching, and atomic mutations.
// Create with NewSettingsStore and inject into components that need settings access.
type SettingsStore struct {
	mu          sync.Mutex
	cache       *Settings
	lastModTime int64
}

// NewSettingsStore creates a new settings store.
func NewSettingsStore() *SettingsStore {
	return &SettingsStore{}
}

// settingsDir returns the platform config directory for relay.
func settingsDir() string {
	return bridge.ConfigDir()
}

// settingsPath returns the full path to settings.json.
func settingsPath() string {
	return filepath.Join(settingsDir(), "settings.json")
}

const currentSettingsVersion = 1

func defaultSettings() *Settings {
	return &Settings{
		Version:      currentSettingsVersion,
		Tokens:       []StoredToken{},
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
	}
}

// loadSettingsInternal reads settings from disk. Caller must hold the mutex.
func loadSettingsInternal() *Settings {
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return defaultSettings()
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultSettings()
	}
	// Default version for pre-versioned settings files.
	if s.Version == 0 {
		s.Version = currentSettingsVersion
	}
	// Ensure slices are non-nil for JSON serialization.
	if s.Tokens == nil {
		s.Tokens = []StoredToken{}
	}
	if s.ExternalMcps == nil {
		s.ExternalMcps = []ExternalMcp{}
	}
	if s.Services == nil {
		s.Services = []ServiceConfig{}
	}
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].DiscoveredTools == nil {
			s.ExternalMcps[i].DiscoveredTools = []ToolInfo{}
		}
	}
	return &s
}

// saveSettingsInternal writes settings to disk atomically via temp file + rename.
// Caller must hold the mutex.
func saveSettingsInternal(s *Settings) error {
	dir := settingsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize settings: %w", err)
	}

	p := settingsPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write temp settings: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename settings: %w", err)
	}
	return nil
}

// ensureAdminSecret generates an AdminSecret if one is not already set.
func ensureAdminSecret(s *Settings) error {
	if s.AdminSecret != "" {
		return nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("generate admin secret: %w", err)
	}
	s.AdminSecret = hex.EncodeToString(b[:])
	return nil
}

// deepCopySettings returns a deep copy of the settings, ensuring slice and map
// mutations in the copy cannot corrupt the original's underlying data.
func deepCopySettings(s *Settings) *Settings {
	cp := *s

	cp.Tokens = make([]StoredToken, len(s.Tokens))
	for i, tok := range s.Tokens {
		cp.Tokens[i] = tok
		if tok.Permissions != nil {
			cp.Tokens[i].Permissions = make(map[string]Permission, len(tok.Permissions))
			for k, v := range tok.Permissions {
				cp.Tokens[i].Permissions[k] = v
			}
		}
		if tok.DisabledTools != nil {
			cp.Tokens[i].DisabledTools = make(map[string][]string, len(tok.DisabledTools))
			for k, v := range tok.DisabledTools {
				names := make([]string, len(v))
				copy(names, v)
				cp.Tokens[i].DisabledTools[k] = names
			}
		}
		if tok.Context != nil {
			cp.Tokens[i].Context = make(map[string]json.RawMessage, len(tok.Context))
			for k, v := range tok.Context {
				raw := make(json.RawMessage, len(v))
				copy(raw, v)
				cp.Tokens[i].Context[k] = raw
			}
		}
	}

	cp.ExternalMcps = make([]ExternalMcp, len(s.ExternalMcps))
	for i, m := range s.ExternalMcps {
		cp.ExternalMcps[i] = m
		if m.Env != nil {
			cp.ExternalMcps[i].Env = make(map[string]string, len(m.Env))
			for k, v := range m.Env {
				cp.ExternalMcps[i].Env[k] = v
			}
		}
		if m.DiscoveredTools != nil {
			cp.ExternalMcps[i].DiscoveredTools = make([]ToolInfo, len(m.DiscoveredTools))
			copy(cp.ExternalMcps[i].DiscoveredTools, m.DiscoveredTools)
		}
	}

	cp.Services = make([]ServiceConfig, len(s.Services))
	for i, svc := range s.Services {
		cp.Services[i] = svc
		if svc.Env != nil {
			cp.Services[i].Env = make(map[string]string, len(svc.Env))
			for k, v := range svc.Env {
				cp.Services[i].Env[k] = v
			}
		}
	}

	return &cp
}

// ---------------------------------------------------------------------------
// SettingsStore methods
// ---------------------------------------------------------------------------

// Get returns a deep copy of the cached settings (or reads from disk on first
// call). The returned *Settings is safe for concurrent read and mutation.
func (ss *SettingsStore) Get() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.cache == nil {
		ss.cache = loadSettingsInternal()
	}
	return deepCopySettings(ss.cache)
}

// Reload always reads from disk and updates the cache.
func (ss *SettingsStore) Reload() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := loadSettingsInternal()
	ss.cache = s
	return deepCopySettings(s)
}

// ReloadIfChanged checks the settings file modtime and reloads only if it changed.
// Returns the new settings if reloaded, or nil if unchanged.
func (ss *SettingsStore) ReloadIfChanged() *Settings {
	info, err := os.Stat(settingsPath())
	if err != nil {
		return nil
	}
	mt := info.ModTime().UnixNano()

	ss.mu.Lock()
	defer ss.mu.Unlock()

	if mt == ss.lastModTime {
		return nil
	}
	ss.lastModTime = mt
	s := loadSettingsInternal()
	ss.cache = s
	return deepCopySettings(s)
}

// With atomically loads settings, calls fn for mutation, then saves.
// Updates the in-memory cache on success.
func (ss *SettingsStore) With(fn func(s *Settings)) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := loadSettingsInternal()
	if err := ensureAdminSecret(s); err != nil {
		return err
	}
	fn(s)
	if err := saveSettingsInternal(s); err != nil {
		slog.Error("failed to save settings", "error", err)
		return err
	}
	ss.cache = s
	return nil
}

// WithAndNotify atomically mutates settings, then sends a bridge notification
// in the background using the admin secret.
func (ss *SettingsStore) WithAndNotify(fn func(s *Settings), notify func(secret string) error) error {
	var secret string
	err := ss.With(func(s *Settings) {
		fn(s)
		secret = s.AdminSecret
	})
	if err == nil {
		go func() { _ = notify(secret) }()
	}
	return err
}

// ---------------------------------------------------------------------------
// Global convenience functions (delegate to defaultStore)
// ---------------------------------------------------------------------------

var defaultStore = NewSettingsStore()

// GetSettings returns a deep copy of the cached settings.
func GetSettings() *Settings { return defaultStore.Get() }

// ReloadSettings always reads from disk and updates the cache.
func ReloadSettings() *Settings { return defaultStore.Reload() }

// ReloadIfChanged reloads settings from disk only if the file's modtime changed.
func ReloadIfChanged() *Settings { return defaultStore.ReloadIfChanged() }

// WithSettings atomically loads, mutates, and saves settings.
func WithSettings(fn func(s *Settings)) error { return defaultStore.With(fn) }

// WithSettingsAndNotify atomically mutates settings and sends a bridge notification.
func WithSettingsAndNotify(fn func(s *Settings), notify func(secret string) error) error {
	return defaultStore.WithAndNotify(fn, notify)
}
