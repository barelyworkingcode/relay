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
}

// Compile-time interface assertion.
var _ SettingsStore = (*FileSettingsStore)(nil)

// FileSettingsStore implements SettingsStore backed by a JSON file on disk.
// Create with NewSettingsStore and inject into components that need settings access.
type FileSettingsStore struct {
	mu          sync.Mutex
	cache       *Settings
	lastModTime int64
	dir         string // config directory (injected for testability)
}

// NewSettingsStore creates a new file-backed settings store using the default
// platform config directory.
func NewSettingsStore() *FileSettingsStore {
	return &FileSettingsStore{dir: bridge.ConfigDir()}
}

// NewSettingsStoreAt creates a file-backed settings store rooted at dir.
// Useful for testing without touching the real config directory.
func NewSettingsStoreAt(dir string) *FileSettingsStore {
	return &FileSettingsStore{dir: dir}
}

// path returns the full path to settings.json.
func (ss *FileSettingsStore) path() string {
	return filepath.Join(ss.dir, "settings.json")
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

// load reads settings from disk. Caller must hold the mutex.
// Returns default settings on missing file (expected on first launch) but
// logs a warning on any other I/O or parse error for observability.
func (ss *FileSettingsStore) load() *Settings {
	data, err := os.ReadFile(ss.path())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read settings file", "error", err)
		}
		return defaultSettings()
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		slog.Warn("failed to parse settings file, using defaults", "error", err)
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
		if s.ExternalMcps[i].Env == nil {
			s.ExternalMcps[i].Env = map[string]string{}
		}
		if s.ExternalMcps[i].DiscoveredTools == nil {
			s.ExternalMcps[i].DiscoveredTools = []ToolInfo{}
		}
	}
	for i := range s.Services {
		if s.Services[i].Args == nil {
			s.Services[i].Args = []string{}
		}
		if s.Services[i].Env == nil {
			s.Services[i].Env = map[string]string{}
		}
	}
}

// save writes settings to disk atomically via temp file + rename.
// Caller must hold the mutex.
func (ss *FileSettingsStore) save(s *Settings) error {
	if err := os.MkdirAll(ss.dir, 0700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize settings: %w", err)
	}

	p := ss.path()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		_ = os.Remove(tmp) // clean up partial temp file
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
// Panics on marshal/unmarshal failure — Settings is a known JSON-safe struct,
// so failure indicates a programming error (e.g., adding an unmarshalable field).
// Panicking is safer than the previous fallback to a shallow copy, which shared
// underlying slices and maps and could silently corrupt state.
func deepCopySettings(s *Settings) *Settings {
	data, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("deepCopySettings: marshal failed (programming error): %v", err))
	}
	var cp Settings
	if err := json.Unmarshal(data, &cp); err != nil {
		panic(fmt.Sprintf("deepCopySettings: unmarshal failed (programming error): %v", err))
	}
	return &cp
}

// EnsureInitialized loads settings from disk, generates an admin secret if
// missing, and saves. Call once at startup before using the store.
func (ss *FileSettingsStore) EnsureInitialized() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.load()
	if err := ensureAdminSecret(s); err != nil {
		return err
	}
	if err := ss.save(s); err != nil {
		return err
	}
	ss.cache = s
	// Seed modtime so the first ReloadIfChanged doesn't needlessly reload.
	if info, err := os.Stat(ss.path()); err == nil {
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
		ss.cache = ss.load()
	}
	return deepCopySettings(ss.cache)
}

// Reload always reads from disk and updates the cache.
func (ss *FileSettingsStore) Reload() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.load()
	ss.cache = s
	return deepCopySettings(s)
}

// ReloadIfChanged checks the settings file modtime and reloads only if it changed.
// Returns the new settings if reloaded, or nil if unchanged.
// Both stat and reload happen under the lock to eliminate any TOCTOU window
// between checking the modtime and updating the cache. The stat targets a
// local file so holding the lock during I/O is negligible.
func (ss *FileSettingsStore) ReloadIfChanged() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	info, err := os.Stat(ss.path())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("settings file stat failed", "error", err)
		}
		return nil
	}
	mt := info.ModTime().UnixNano()

	if mt == ss.lastModTime {
		return nil
	}
	ss.lastModTime = mt
	s := ss.load()
	ss.cache = s
	return deepCopySettings(s)
}

// With atomically mutates the cached settings and saves to disk.
// Uses the in-memory cache (deep-copied) rather than re-reading from disk,
// since the cache is authoritative under the mutex. Updates modtime on
// success so ReloadIfChanged() won't redundantly re-read what we just wrote.
// The admin secret must already exist (call EnsureInitialized at startup).
func (ss *FileSettingsStore) With(fn func(s *Settings)) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.cache == nil {
		ss.cache = ss.load()
	}
	s := deepCopySettings(ss.cache)
	fn(s)
	if err := ss.save(s); err != nil {
		slog.Error("failed to save settings", "error", err)
		return err
	}
	ss.cache = s
	if info, err := os.Stat(ss.path()); err == nil {
		ss.lastModTime = info.ModTime().UnixNano()
	}
	return nil
}


