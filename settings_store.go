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

// SettingsStore abstracts settings persistence for testability.
type SettingsStore interface {
	EnsureInitialized() error
	Get() *Settings
	Reload() *Settings
	ReloadIfChanged() *Settings
	With(fn func(*Settings)) error
	WithAndNotify(fn func(*Settings), notify func(string) error) error
}

// FileSettingsStore implements SettingsStore backed by a JSON file on disk.
// Create with NewSettingsStore and inject into components that need settings access.
type FileSettingsStore struct {
	mu          sync.Mutex
	cache       *Settings
	lastModTime int64
}

// NewSettingsStore creates a new file-backed settings store.
func NewSettingsStore() *FileSettingsStore {
	return &FileSettingsStore{}
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
	s.normalize()
	return &s
}

// normalize ensures all slices are non-nil (for JSON serialization) and
// back-fills default values for fields added in later versions.
func (s *Settings) normalize() {
	if s.Version == 0 {
		s.Version = currentSettingsVersion
	}
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
		if s.ExternalMcps[i].Args == nil {
			s.ExternalMcps[i].Args = []string{}
		}
		if s.ExternalMcps[i].DiscoveredTools == nil {
			s.ExternalMcps[i].DiscoveredTools = []ToolInfo{}
		}
	}
	for i := range s.Services {
		if s.Services[i].Args == nil {
			s.Services[i].Args = []string{}
		}
	}
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

// deepCopySettings returns a deep copy via JSON round-trip. This is correct by
// construction — new fields are automatically included without manual updates.
// Performance is irrelevant here: settings are small and copies are infrequent.
func deepCopySettings(s *Settings) *Settings {
	data, err := json.Marshal(s)
	if err != nil {
		slog.Error("deepCopySettings: marshal failed, returning shallow copy", "error", err)
		cp := *s
		return &cp
	}
	var cp Settings
	if err := json.Unmarshal(data, &cp); err != nil {
		slog.Error("deepCopySettings: unmarshal failed, returning shallow copy", "error", err)
		cp := *s
		return &cp
	}
	return &cp
}

// EnsureInitialized loads settings from disk, generates an admin secret if
// missing, and saves. Call once at startup before using the store.
func (ss *FileSettingsStore) EnsureInitialized() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := loadSettingsInternal()
	if err := ensureAdminSecret(s); err != nil {
		return err
	}
	if err := saveSettingsInternal(s); err != nil {
		return err
	}
	ss.cache = s
	// Seed modtime so the first ReloadIfChanged doesn't needlessly reload.
	if info, err := os.Stat(settingsPath()); err == nil {
		ss.lastModTime = info.ModTime().UnixNano()
	}
	return nil
}

// ---------------------------------------------------------------------------
// FileSettingsStore methods
// ---------------------------------------------------------------------------

// Get returns a deep copy of the cached settings (or reads from disk on first
// call). The returned *Settings is safe for concurrent read and mutation.
func (ss *FileSettingsStore) Get() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.cache == nil {
		ss.cache = loadSettingsInternal()
	}
	return deepCopySettings(ss.cache)
}

// Reload always reads from disk and updates the cache.
func (ss *FileSettingsStore) Reload() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := loadSettingsInternal()
	ss.cache = s
	return deepCopySettings(s)
}

// ReloadIfChanged checks the settings file modtime and reloads only if it changed.
// Returns the new settings if reloaded, or nil if unchanged.
func (ss *FileSettingsStore) ReloadIfChanged() *Settings {
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
// The admin secret must already exist (call EnsureInitialized at startup).
func (ss *FileSettingsStore) With(fn func(s *Settings)) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := loadSettingsInternal()
	fn(s)
	if err := saveSettingsInternal(s); err != nil {
		slog.Error("failed to save settings", "error", err)
		return err
	}
	ss.cache = s
	return nil
}

// WithAndNotify atomically mutates settings, then sends a bridge notification
// using the admin secret. The notification is sent synchronously so callers
// can control concurrency (e.g., via GoFunc) rather than spawning untracked
// goroutines that could be lost during shutdown.
func (ss *FileSettingsStore) WithAndNotify(fn func(s *Settings), notify func(secret string) error) error {
	var secret string
	err := ss.With(func(s *Settings) {
		fn(s)
		secret = s.AdminSecret
	})
	if err != nil {
		return err
	}
	if nerr := notify(secret); nerr != nil {
		slog.Warn("bridge notification failed", "error", nerr)
	}
	return nil
}

