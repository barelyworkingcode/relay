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
)

// settingsMu serializes all settings load-modify-save cycles and cache access.
var settingsMu sync.Mutex

// settingsCache holds the last-loaded settings to avoid re-reading disk on
// every hot-path request (ListTools, CallTool). Updated by WithSettings and
// ReloadSettings. Guarded by settingsMu.
var settingsCache *Settings

// settingsDir returns the platform config directory for relay.
func settingsDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, "relay")
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

// loadSettingsInternal reads settings from disk. Caller must hold settingsMu.
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
// Caller must hold settingsMu.
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
func ensureAdminSecret(s *Settings) {
	if s.AdminSecret != "" {
		return
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		s.AdminSecret = hex.EncodeToString(b[:])
	}
}

// deepCopySettings returns a deep copy of the settings, ensuring slice mutations
// in the copy cannot corrupt the original's underlying arrays.
func deepCopySettings(s *Settings) *Settings {
	cp := *s

	cp.Tokens = make([]StoredToken, len(s.Tokens))
	copy(cp.Tokens, s.Tokens)

	cp.ExternalMcps = make([]ExternalMcp, len(s.ExternalMcps))
	copy(cp.ExternalMcps, s.ExternalMcps)

	cp.Services = make([]ServiceConfig, len(s.Services))
	copy(cp.Services, s.Services)

	return &cp
}

// GetSettings returns a deep copy of the cached settings (or reads from disk
// on first call). Cheap on the hot path. The returned *Settings is a distinct
// snapshot safe for concurrent read access and slice mutation without holding
// the lock. Does not generate or persist an admin secret.
func GetSettings() *Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if settingsCache == nil {
		settingsCache = loadSettingsInternal()
	}
	return deepCopySettings(settingsCache)
}

// ReloadSettings always reads from disk and updates the cache.
// Returns a deep-copy snapshot, same as Settings.
// Use when external changes are expected (reconcile, reload, status poll).
func ReloadSettings() *Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := loadSettingsInternal()
	settingsCache = s
	return deepCopySettings(s)
}

// lastModTime tracks the settings file's modtime for change detection.
// Guarded by settingsMu.
var lastModTime int64

// ReloadIfChanged checks the settings file modtime and reloads only if it changed.
// Returns the new settings if reloaded, or nil if unchanged.
func ReloadIfChanged() *Settings {
	info, err := os.Stat(settingsPath())
	if err != nil {
		return nil
	}
	mt := info.ModTime().UnixNano()

	settingsMu.Lock()
	defer settingsMu.Unlock()

	if mt == lastModTime {
		return nil
	}
	lastModTime = mt
	s := loadSettingsInternal()
	settingsCache = s
	return deepCopySettings(s)
}

// WithSettings atomically loads settings, calls fn for mutation, then saves.
// Updates the in-memory cache on success.
func WithSettings(fn func(s *Settings)) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := loadSettingsInternal()
	ensureAdminSecret(s)
	fn(s)
	if err := saveSettingsInternal(s); err != nil {
		slog.Error("failed to save settings", "error", err)
		return err
	}
	settingsCache = s
	return nil
}

// WithSettingsAndNotify atomically mutates settings, then sends a bridge
// notification in the background using the admin secret. Use this for the
// common "mutate settings + notify tray app" pattern.
func WithSettingsAndNotify(fn func(s *Settings), notify func(secret string) error) error {
	var secret string
	err := WithSettings(func(s *Settings) {
		fn(s)
		secret = s.AdminSecret
	})
	if err == nil {
		go func() { _ = notify(secret) }()
	}
	return err
}
