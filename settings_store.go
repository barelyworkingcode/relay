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

	"github.com/tidwall/jsonc"

	"relaygo/bridge"
)

type SettingsStore interface {
	EnsureInitialized() error
	Get() *Settings
	Reload() *Settings
	ReloadIfChanged() *Settings
	With(fn func(*Settings)) error
}

var _ SettingsStore = (*FileSettingsStore)(nil)

type FileSettingsStore struct {
	mu          sync.Mutex
	cache       *Settings
	lastModTime int64
	dir         string // injected for testability, rather than calling bridge.ConfigDir() directly
}

func NewSettingsStore() *FileSettingsStore {
	return &FileSettingsStore{dir: bridge.ConfigDir()}
}

func NewSettingsStoreAt(dir string) *FileSettingsStore {
	return &FileSettingsStore{dir: dir}
}

func (ss *FileSettingsStore) path() string {
	return filepath.Join(ss.dir, "settings.json")
}

const currentSettingsVersion = 1

func defaultSettings() *Settings {
	return &Settings{
		Version:      currentSettingsVersion,
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
		Projects:     []Project{},
	}
}

// load: caller must hold the mutex.
//
// JSONC comments (// and /* */) are stripped before parsing so users can
// hand-edit settings.json with comment blocks to toggle sections. Comments
// don't survive writes — save() goes through json.MarshalIndent.
func (ss *FileSettingsStore) load() *Settings {
	data, err := os.ReadFile(ss.path())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read settings file", "error", err)
		}
		return defaultSettings()
	}
	var s Settings
	if err := json.Unmarshal(jsonc.ToJSON(data), &s); err != nil {
		slog.Warn("failed to parse settings file, using defaults", "error", err)
		return defaultSettings()
	}
	s.normalize()
	return &s
}

func ensureSlice[T any](s *[]T) {
	if *s == nil {
		*s = []T{}
	}
}

func ensureMap[K comparable, V any](m *map[K]V) {
	if *m == nil {
		*m = map[K]V{}
	}
}

func (s *Settings) normalize() {
	if s.Version == 0 {
		s.Version = currentSettingsVersion
	}
	ensureSlice(&s.ExternalMcps)
	ensureSlice(&s.Services)
	for i := range s.ExternalMcps {
		ensureSlice(&s.ExternalMcps[i].Args)
		ensureMap(&s.ExternalMcps[i].Env)
	}
	for i := range s.Services {
		ensureSlice(&s.Services[i].Args)
		ensureMap(&s.Services[i].Env)
	}
	ensureSlice(&s.Projects)
	for i := range s.Projects {
		ensureSlice(&s.Projects[i].AllowedMcpIDs)
		ensureSlice(&s.Projects[i].AllowedModels)
	}
}

// atomicWriteFile writes + fsyncs a temp file, renames it over the target,
// then fsyncs the directory so the rename survives a crash: os.Rename is
// atomic for visibility but NOT durable on its own.
//
// Shared by settings persistence and the service-config editor
// (service_config_file.go) so both go through one tested durability path.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	// fsync the directory so the rename is durable across a crash.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// save: caller must hold the mutex.
func (ss *FileSettingsStore) save(s *Settings) error {
	if err := os.MkdirAll(ss.dir, 0700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize settings: %w", err)
	}

	// A crash after a bare rename can leave settings.json zero-length or stale,
	// and load() treats a parse failure as "use defaults" — silently wiping
	// every project and its token hashes. atomicWriteFile's fsyncs close that
	// window.
	if err := atomicWriteFile(ss.path(), data, 0600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

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

// deepCopySettings goes through a JSON round-trip rather than a shallow
// struct copy, deliberately: a shallow copy shares underlying slices and
// maps and can silently corrupt state across callers. It panics on
// marshal/unmarshal failure rather than returning an error — Settings is a
// known JSON-safe struct, so failure here means a programming error (e.g. an
// unmarshalable field was added), not a runtime condition callers should
// handle.
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

// freshSettings returns settings that reflect the FILE, not whatever snapshot
// this process happened to load earlier.
//
// Get() answers from an in-memory cache that only two things refresh: a
// mutation made by this process (With, which writes the cache through), and the
// tray's 2s settings poll (ReloadIfChanged). That is fine for a menu and wrong
// for an authorization decision, because relay is not one process: `relay enrol
// create` runs in a CLI process and writes settings.json, and until the tray's
// next poll its listener resolves certificates against a settings view that
// predates the record — so a brand-new enrolment is refused as "not enrolled"
// for up to a poll interval, which is indistinguishable from a genuine
// misconfiguration (issue #21).
//
// The cost of closing that window is one stat() per decision: ReloadIfChanged
// re-reads only when the modtime moved, so the steady state is a stat and a
// deep copy of a small struct. Anything on the remote path that decides whether
// a caller may act MUST go through here rather than Get().
func freshSettings(store SettingsStore) *Settings {
	if s := store.ReloadIfChanged(); s != nil {
		return s
	}
	return store.Get()
}

// Get returns a deep copy, safe for concurrent read and mutation.
func (ss *FileSettingsStore) Get() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.cache == nil {
		ss.cache = ss.load()
	}
	return deepCopySettings(ss.cache)
}

func (ss *FileSettingsStore) Reload() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.load()
	ss.cache = s
	return deepCopySettings(s)
}

// ReloadIfChanged returns the new settings if the file's modtime moved, or
// nil if unchanged. Both stat and reload happen under the lock, deliberately,
// to eliminate a TOCTOU window between checking the modtime and updating the
// cache — the stat targets a local file, so holding the lock during I/O is
// negligible.
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

// With mutates a deep copy of the in-memory cache rather than re-reading
// from disk, since the cache is authoritative under the mutex, and updates
// lastModTime on success so ReloadIfChanged won't redundantly re-read what
// was just written. Requires EnsureInitialized to have run first.
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
