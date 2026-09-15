package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tidwall/jsonc"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

type SettingsStore interface {
	EnsureInitialized() error
	Get() *Settings
	Reload() *Settings
	ReloadIfChanged() *Settings
	With(fn func(*Settings)) error
	// Sealer returns the store's sealer, or nil on a CLI-shaped or
	// degraded store (§5.4, §5.6). It exists so a caller that needs to
	// seal something outside settings.json itself — LoadOrCreateCA's
	// ca.key.sealed — shares the same key rather than resolving one of
	// its own.
	Sealer() sealed.Sealer
}

var _ SettingsStore = (*FileSettingsStore)(nil)

// DeclinableSettingsStore is the write a callback can refuse.
//
// This is deliberate: it is a second, narrow interface rather than a method on
// SettingsStore. Refusing a write is a property of a store that has a file to
// leave untouched, and widening SettingsStore would oblige every implementation
// of it to grow a method that means nothing to most of them.
type DeclinableSettingsStore interface {
	WithDeclinable(fn func(s *Settings) error) error
}

// withDeclinable saves fn's mutation only when fn returns nil, whatever store
// it is handed.
//
// This is subtle: a store that cannot decline still RUNS fn and still saves, so
// a refusal that mutated s on its way out has to be rolled back here or the
// refusal would persist half of what it refused. Such a store still writes;
// only DeclinableSettingsStore can promise the file was not touched at all.
func WithDeclinable(store SettingsStore, fn func(s *Settings) error) error {
	if d, ok := store.(DeclinableSettingsStore); ok {
		return d.WithDeclinable(fn)
	}
	var refusal error
	saveErr := store.With(func(s *Settings) {
		before := s.Clone()
		if refusal = fn(s); refusal != nil {
			*s = *before
		}
	})
	if refusal != nil {
		return refusal
	}
	return saveErr
}

type FileSettingsStore struct {
	mu          sync.Mutex
	cache       *Settings
	lastModTime int64
	// fileSeen records whether settings.json existed the last time this store
	// looked. It is what separates "not created yet" — a first start, where
	// the tray has legitimate settings in hand and has not written them out —
	// from "existed and is now gone", which must invalidate the cache rather
	// than keep serving it. Nothing else can tell those two apart, since both
	// present as a stat that fails with IsNotExist.
	fileSeen bool
	// readErr holds the reason the last look at an EXISTING settings.json
	// produced no usable settings: a read that failed, or bytes that would
	// not parse.
	//
	// This is subtle: it is the difference between *empty* settings and
	// *unknown* settings, which the returned *Settings cannot express because
	// both resolve to defaultSettings() so that reads fail closed. Reads may
	// treat unknown as empty; a write may not, because the records it would
	// serialize over are records this process never saw.
	readErr error
	dir     string // injected for testability, rather than calling bridge.ConfigDir() directly

	// sealer, when non-nil, is what load() opens sealed fields with and
	// save() reseals them with. nil is two different things depending on
	// sealUnavailable (§5.4, §5.6):
	//
	//   - sealer == nil, sealUnavailable == nil: the CLI's read-only-half
	//     store (NewSettingsStore / NewSettingsStoreAt). It never had a
	//     sealer to begin with, and every write refuses with
	//     errSealerRequired — a second, structural guarantee behind
	//     brokering that a CLI process cannot write settings.json even if
	//     some future change reintroduced a direct call.
	//   - sealer == nil, sealUnavailable != nil: the tray, degraded — a
	//     keyring that could not produce the key settings.json names.
	//     Every write refuses with errSealUnavailable wrapping the named
	//     reason, but reads still work in full (§5.6).
	sealer          sealed.Sealer
	sealUnavailable error
	// sealErrors holds one entry per Secret that a present, matching
	// sealer still could not open — a corrupt envelope, or (via
	// verifyProjectTokenHashes) a project token that opened but does not
	// hash to its own token_hash. Populated by load(); read by SealStatus().
	sealErrors map[string]error
}

// errSettingsUnreadable is what a caller matches with errors.Is to tell a
// refusal to write from a write that was attempted and failed.
var ErrSettingsUnreadable = errors.New("settings file exists but could not be read")

// errSealerRequired is what every write on a sealer-less, non-degraded
// store refuses with — the CLI's shape (§5.4). Never wraps a reason: there
// is nothing to name beyond "this process never had a sealer," which is
// what distinguishes it from errSealUnavailable.
var ErrSealerRequired = errors.New("this process holds no sealing key and cannot write settings.json")

// errSealUnavailable is what every write on a degraded tray store refuses
// with (§5.6 clause 3). Always wraps the named reason a caller matches with
// errors.Is and reads with Error().
var ErrSealUnavailable = errors.New("sealed store is unavailable")

func NewSettingsStore() *FileSettingsStore {
	return &FileSettingsStore{dir: bridge.ConfigDir()}
}

func NewSettingsStoreAt(dir string) *FileSettingsStore {
	return &FileSettingsStore{dir: dir}
}

// NewSettingsStoreSealed is the tray's constructor for the fully-working
// case: sealer already resolved against the key settings.json names
// (§5.5). Constructing one with a nil sealer is a programming error, not a
// degraded state — use NewSettingsStoreDegraded for that.
func NewSettingsStoreSealed(dir string, sealer sealed.Sealer) *FileSettingsStore {
	return &FileSettingsStore{dir: dir, sealer: sealer}
}

// NewSettingsStoreWithCache is the first-start shape held open for tests:
// settings already in hand, nothing written out yet. fileSeen stays false,
// so an absent settings.json under dir reads as "not created yet" rather
// than as a deletion that must invalidate cache.
//
// It panics outside a test binary. A constructor that hands a caller a store
// serving settings no write ever produced is a weakening, and a weakening
// introduced for a test is the one most likely to survive into production
// (ADR-016 decision 8); production builds one through NewSettingsStoreSealed
// and a write.
func NewSettingsStoreWithCache(dir string, sealer sealed.Sealer, cache *Settings) *FileSettingsStore {
	if !testing.Testing() {
		panic("config: NewSettingsStoreWithCache is a test seam and must not be reached in a shipped binary")
	}
	return &FileSettingsStore{dir: dir, sealer: sealer, cache: cache}
}

// NewSettingsStoreDegraded is the tray's constructor for §5.6: a keyring
// that could not produce the key settings.json names, named by reason.
// The store still loads and serves every clear field; every write refuses.
func NewSettingsStoreDegraded(dir string, reason error) *FileSettingsStore {
	if reason == nil {
		panic("sealed: NewSettingsStoreDegraded called with a nil reason")
	}
	return &FileSettingsStore{dir: dir, sealUnavailable: reason}
}

// Sealer returns the store's sealer, or nil on a CLI-shaped or degraded
// store. It exists so a caller that legitimately needs to seal something
// outside settings.json itself — ca.key, in LoadOrCreateCA — can share the
// same key rather than resolving one of its own.
func (ss *FileSettingsStore) Sealer() sealed.Sealer {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.sealer
}

// SealStatus reports why the sealed store is not fully available, or nil
// when every sealed field this process has seen opened cleanly. A
// whole-store reason (sealUnavailable) is reported ahead of any per-field
// one: with no working sealer at all, every field is equally and
// unsurprisingly unopenable, and naming each one separately would bury the
// one fact that actually explains it.
func (ss *FileSettingsStore) SealStatus() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.sealUnavailable != nil {
		return fmt.Errorf("%w: %w", ErrSealUnavailable, ss.sealUnavailable)
	}
	for _, err := range ss.sealErrors {
		return err
	}
	return nil
}

func (ss *FileSettingsStore) path() string {
	return filepath.Join(ss.dir, "settings.json")
}

const CurrentSettingsVersion = 1

// defaultModelEndpointListen is the loopback address R-S9 turns the model
// endpoint's TCP listener on for (plan-broker-and-sessions.md C8): the
// block itself predates this feature and defaults to absent/disabled
// (model.sock is served regardless), but a build that carries the session
// host needs relayLLM and the session host to reach models over the same
// broker, so ensureDefaultModelEndpoint writes this in once, the first
// time such a build starts, exactly as Audit's own default is written
// explicitly below. Loopback only, same reasoning as remote.listen.
const defaultModelEndpointListen = "127.0.0.1:8180"

// EnsureDefaultModelEndpoint sets s.ModelEndpoint to its default iff no
// block exists yet. An install that later clears Listen to "" (an explicit
// choice to disable the TCP listener while keeping model.sock) keeps a
// non-nil block from then on, so this never re-fires and never undoes that
// choice; only a settings.json that has genuinely never decided the block
// — a fresh install, or one written before this feature existed — sees it
// applied. Deliberately NOT wired into EnsureInitialized: that runs from
// several hermetic tests that expect an absent block, via the CLI reset
// path and via the model endpoint's own test setup, none of which are "the
// feature build starts" (spec-session-host.md's own phrase) — only
// runTrayApp is, and it is the only production caller of this function
// (cmd/relay's own startup wiring), which the hermetic suite never reaches.
func EnsureDefaultModelEndpoint(s *Settings) {
	if s.ModelEndpoint != nil {
		return
	}
	s.ModelEndpoint = &ModelEndpointConfig{Listen: defaultModelEndpointListen}
}

func DefaultSettings() *Settings {
	// This is deliberate: the block is redundant with AuditConfig.resolve(),
	// which already reads an absent one as enabled, and reads as noise to
	// delete. A new install must be able to learn what auditing is doing by
	// reading its own settings.json, and this is the only place the operator
	// is shown that `false` is a value the key takes — one whose cost
	// (ADR-010: no remote listener) is in docs/audit-log.md.
	auditEnabled := true
	return &Settings{
		Version:      CurrentSettingsVersion,
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
		Projects:     []Project{},
		Audit:        &AuditConfig{Enabled: &auditEnabled},
	}
}

// load: caller must hold the mutex.
//
// JSONC comments (// and /* */) are stripped before parsing so users can
// hand-edit settings.json with comment blocks to toggle sections. Comments
// don't survive writes — save() goes through json.MarshalIndent.
//
// This is subtle: load also maintains fileSeen and readErr, because the open is
// the one place that learns whether the file is there and whether it could be
// used. A permission error deliberately leaves fileSeen alone — it says nothing
// about existence, and treating it as absence would let a chmod erase the
// store's memory of a file that is still on disk.
//
// Every failure still yields defaultSettings(), so a read resolving through
// here has nothing to authenticate against. readErr is what stops a WRITE from
// treating that emptiness as the file's contents.
func (ss *FileSettingsStore) load() *Settings {
	data, err := os.ReadFile(ss.path())
	if err != nil {
		if os.IsNotExist(err) {
			ss.fileSeen = false
			ss.readErr = nil
		} else {
			slog.Warn("failed to read settings file", "error", err)
			ss.readErr = err
		}
		return DefaultSettings()
	}
	ss.fileSeen = true
	var s Settings
	if err := json.Unmarshal(jsonc.ToJSON(data), &s); err != nil {
		slog.Warn("failed to parse settings file, using defaults", "error", err)
		ss.readErr = err
		return DefaultSettings()
	}
	ss.readErr = nil

	openErrs := openAllSecrets(&s, ss.sealer)
	for path, err := range verifyProjectTokenHashes(&s) {
		openErrs[path] = err
	}
	ss.sealErrors = openErrs

	for i := range s.Services {
		s.Services[i].migrateCapabilities()
		s.Services[i].sanitizeIfBuiltin()
		if err := s.Services[i].validateCapabilities(); err != nil {
			slog.Error("service record refused: relay will not start it", "id", s.Services[i].ID, "error", err)
		}
	}
	s.normalize()
	return &s
}

// unreadableErrLocked reports why this store must not write, or nil if it may.
// Caller must hold the mutex.
func (ss *FileSettingsStore) unreadableErrLocked() error {
	if ss.readErr == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrSettingsUnreadable, ss.path(), ss.readErr)
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
		s.Version = CurrentSettingsVersion
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
		ensureSlice(&s.Services[i].Capabilities)
	}
	ensureSlice(&s.Projects)
	for i := range s.Projects {
		ensureSlice(&s.Projects[i].AllowedMcpIDs)
		ensureSlice(&s.Projects[i].AllowedModels)
	}
	ensureSlice(&s.Hosts)
}

// atomicWriteFile writes + fsyncs a temp file, renames it over the target,
// then fsyncs the directory so the rename survives a crash: os.Rename is
// atomic for visibility but NOT durable on its own.
//
// Shared by settings persistence and the service-config editor
// (service_config_file.go) so both go through one tested durability path.
//
// This is deliberate: the staging file gets a unique name in the target's own
// directory rather than the fixed `<path>.tmp` it used to. relay is not one
// process (docs/tokens.md), and a fixed name with O_TRUNC let two writers open
// the SAME staging file — one truncating the other's half-written bytes, then
// both renaming it over the target, which is how a settings.json ending `}}`
// reaches disk and takes the control plane out of service. Same directory so
// the rename stays within one filesystem, and therefore atomic.
//
// A unique name stops the tearing. It does NOT make a cross-process
// read-modify-write atomic: that stays last-writer-wins, exactly as
// FileSettingsStore.With documents.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	// os.CreateTemp fixes its own 0600 regardless of what the caller asked
	// for, so perm is applied here rather than at open.
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
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
//
// This is subtle: normalize() runs here and not only in load(). A callback that
// appends a record leaves its slice and map fields nil, and json.Marshal spells
// nil as `null` where every record that has been through load() spells it `[]`.
// The next load() repairs it, so nothing in-process notices — but `relay audit`,
// `relay grant` and a hand-edit read the FILE, and a file whose records disagree
// with each other about how "empty" is spelled invites the reader to conclude
// the two mean different things. It mutates s deliberately: both callers make s
// the cache immediately afterwards, so the cache and the file stay identical.
func (ss *FileSettingsStore) save(s *Settings) error {
	// This is deliberate: sealing happens HERE, before json.MarshalIndent,
	// not to the file afterwards. The bytes handed to atomicWriteFile below
	// already contain no plaintext, so the settings.json.*.tmp staging file
	// never contains any either, and a process killed between create and
	// rename leaves a sealed file behind, not a plaintext one (§4.5). Do
	// not "fix" this by adding sweeping or shredding to atomicWriteFile —
	// that would be machinery in the wrong place for a residue that this
	// step already prevents from ever being written.
	if ss.sealer == nil {
		if ss.sealUnavailable != nil {
			return fmt.Errorf("%w: %w", ErrSealUnavailable, ss.sealUnavailable)
		}
		return ErrSealerRequired
	}
	if err := SealAllSecrets(s, ss.sealer); err != nil {
		return err
	}

	if err := os.MkdirAll(ss.dir, 0700); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}

	s.normalize()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize settings: %w", err)
	}

	// A crash after a bare rename can leave settings.json zero-length or stale,
	// which resolves to empty settings and takes every project, credential and
	// enrolment out of service until the file is repaired. atomicWriteFile's
	// fsyncs close that window.
	if err := AtomicWriteFile(ss.path(), data, 0600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	ss.fileSeen = true
	return nil
}

func ensureAdminSecret(s *Settings) error {
	if pt, ok := s.AdminSecret.Reveal(); ok && pt != "" {
		return nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("generate admin secret: %w", err)
	}
	s.AdminSecret = NewSecret(hex.EncodeToString(b[:]))
	return nil
}

// EnsureInitialized creates a settings file that is not there yet, migrates
// one written before sealing existed, or — on a degraded store (§5.6) —
// simply loads what it can and leaves the file untouched. It never writes
// on a degraded or sealer-less store: every write on either refuses inside
// save() with a named reason, and starting over that reason with fresh
// defaults would be worse than deferring to the operator.
//
// A file that exists but cannot be read is refused, loudly, with the file
// untouched, on a sealer-bearing store exactly as it always has been: the
// tray exits on THIS error rather than starting, because an unreadable
// file has unknown contents, and starting over it destroys every project,
// token hash, credential and enrolment in it with no operator surface
// saying so. A degraded store is not this case — its contents are known
// and readable, only its sealed fields are not, which is why §5.6 has
// relay start instead of exit: the recovery UI lives in the tray, and
// exiting would leave the operator with a machine whose only recovery
// surface is the thing that will not start.
func (ss *FileSettingsStore) EnsureInitialized() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	s := ss.load()
	if err := ss.unreadableErrLocked(); err != nil {
		return fmt.Errorf("%w — refusing to initialize over it; repair or move the file, or delete it to start fresh", err)
	}

	if ss.sealer == nil {
		// Degraded (sealUnavailable names why) or CLI-shaped (nothing to
		// name). Either way this process cannot write, so there is
		// nothing left to do beyond serving what load() already produced.
		ss.cache = s
		if info, err := os.Stat(ss.path()); err == nil {
			ss.lastModTime = info.ModTime().UnixNano()
		}
		return nil
	}

	if needsSealedMigration(s) || caKeyAwaitsMigration(ss.dir) {
		if err := ss.migrateLocked(s); err != nil {
			return err
		}
		return nil
	}

	if err := ensureAdminSecret(s); err != nil {
		return err
	}
	if s.SealedKeyID == "" {
		s.SealedKeyID = ss.sealer.KeyID()
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
//
// This is subtle: the Get() fall-through is not a stale-cache escape hatch. It
// runs only where ReloadIfChanged has nothing new to report, and a settings
// file that has been deleted is something to report — it comes back through
// the branch above as absent settings, with nothing left to authenticate
// against.
func FreshSettings(store SettingsStore) *Settings {
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
	return ss.cache.Clone()
}

func (ss *FileSettingsStore) Reload() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.load()
	ss.cache = s
	return s.Clone()
}

// reloadIfChangedLocked brings the cache in line with the file and reports
// whether it now holds something the caller has not seen. Caller must hold the
// mutex; both the stat and the read happen under it, deliberately, so there is
// no TOCTOU window between checking the modtime and replacing the cache — the
// stat targets a local file, so holding the lock during I/O is negligible.
//
// A settings file that existed and is GONE resolves to absent settings, never
// to the last good cache. Deleting settings.json is how an operator locks the
// control plane out, and a cache that outlived the file would keep every
// credential in it authenticating from memory, with nothing left on disk to
// say so.
//
// A file that was never there is a different state and is left alone: a first
// start has settings in hand that nothing has written out yet, and emptying
// the cache would wipe them before they reach disk.
func (ss *FileSettingsStore) reloadIfChangedLocked() bool {
	info, err := os.Stat(ss.path())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("settings file stat failed", "error", err)
			ss.readErr = err
			// This is subtle: readErr alone closes only WRITES, and a stat
			// that failed leaves the file's state unknown for reads too — a
			// read answered from a cache older than the failure is stale
			// rather than closed, which is the one thing every other degraded
			// state of this file refuses to be. load() re-reads and decides
			// readErr itself, so a stat that failed for a reason the open does
			// not share resolves to the file rather than to this branch's
			// guess.
			//
			// A file this store has NEVER seen is left alone for the same
			// reason the deletion branch below leaves it alone: a first start
			// holds settings nothing has written out yet, and emptying the
			// cache would wipe them before they reach disk.
			if !ss.fileSeen {
				return false
			}
			ss.cache = ss.load()
			return true
		}
		if !ss.fileSeen {
			return false
		}
		slog.Warn("settings file has been deleted; settings now resolve to empty")
		ss.fileSeen = false
		ss.readErr = nil
		ss.cache = DefaultSettings()
		// Zeroed so a settings.json restored from a backup still reads as a
		// change: a restored file's modtime can predate the deleted one's.
		ss.lastModTime = 0
		return true
	}
	mt := info.ModTime().UnixNano()

	// This is deliberate: an unreadable file re-reads on every look, modtime or
	// not. The two states that produce one — a chmod and a repair of that chmod
	// — move no timestamp, so a store that trusted the modtime here would stay
	// refusing writes until something unrelated wrote the file. The extra read
	// happens only while degraded.
	if ss.fileSeen && mt == ss.lastModTime && ss.readErr == nil {
		return false
	}
	ss.lastModTime = mt
	ss.cache = ss.load()
	return true
}

// ReloadIfChanged returns the new settings if the file moved or vanished, or
// nil if there is nothing new to report.
func (ss *FileSettingsStore) ReloadIfChanged() *Settings {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if !ss.reloadIfChangedLocked() {
		return nil
	}
	return ss.cache.Clone()
}

// With mutates a deep copy of the settings as they are ON DISK and updates
// lastModTime on success so ReloadIfChanged won't redundantly re-read what was
// just written.
//
// The reload before the callback is what makes this safe across processes.
// `relay credential mint`, `relay enrol create` and `relay service register`
// each run in their own process and write settings.json directly; a tray that
// applied its callback to a cache predating that write would save the record
// away again, and for a minted credential the plaintext is printed once and
// unrecoverable. Within one process the mutex alone would be enough — relay is
// not one process. Cost is one stat per mutation, since the file is only
// re-parsed when it actually moved.
//
// This does NOT make the write atomic against another process writing between
// the reload and the save: that remains last-writer-wins.
//
// A file that exists and could not be read is refused with
// errSettingsUnreadable and the callback never runs. The reload resolves that
// file to empty settings so reads fail closed, and saving the callback's
// mutation on top of that emptiness would rewrite settings.json as defaults
// plus one change — every other record silently gone, with a nil error
// reporting success.
func (ss *FileSettingsStore) With(fn func(s *Settings)) error {
	return ss.WithDeclinable(func(s *Settings) error {
		fn(s)
		return nil
	})
}

// WithDeclinable is With with one addition: a callback that returns an error
// declines the write outright. Nothing is saved, settings.json is left byte for
// byte as it was, and the error is returned unchanged so errors.Is and
// errors.As still reach the callback's own sentinel.
//
// The distinction is not cosmetic. With saves whatever its callback leaves
// behind, including nothing at all, so a callback that decides its change must
// not happen still rewrites the file. On the login routes — the one
// unauthenticated surface relay serves — that handed anything able to reach the
// port a settings writer it holds no credential for, at whatever rate it cared
// to send. Every such write is also a chance to lose a concurrent writer's
// change, because a cross-process read-modify-write here is last-writer-wins
// (docs/tokens.md, "The settings file has more than one writer").
//
// A refusal that MUTATED s before returning still writes nothing: the mutation
// lives on a deep copy this method discards.
func (ss *FileSettingsStore) WithDeclinable(fn func(s *Settings) error) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.reloadIfChangedLocked()
	if ss.cache == nil {
		ss.cache = ss.load()
	}
	if err := ss.unreadableErrLocked(); err != nil {
		slog.Error("refusing to save settings over a file that could not be read", "error", err)
		return err
	}
	s := ss.cache.Clone()
	if err := fn(s); err != nil {
		return err
	}
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
