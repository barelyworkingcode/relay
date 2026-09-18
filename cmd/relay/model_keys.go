package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/barelyworkingcode/relay/internal/service"
)

// modelKeyPrefix makes a model key recognisable in a leak scan, and is what
// the auth resolver uses to tell a model key from a project token before
// hashing either (spec-model-broker.md §3.1).
const modelKeyPrefix = "rmk_"

// modelKeyHexLen is the random half of the plaintext: 32 bytes, lowercase
// hex, the same shape a launch secret uses (internal/service.LaunchSecretHexLen).
const modelKeyHexLen = 64

type modelKeyRecord struct {
	projectID string
	label     string
	// launch, when set, is the launch this key lives and dies with: Lookup
	// re-derives whether it is still live on every call (BindLaunch). nil
	// means the key is revoked only explicitly (Revoke/RevokeKey), which is
	// every key minted for a session that has no launch to bind.
	launch *service.Launch
}

// ModelKeyTable is relay's in-memory table of minted model keys
// (spec-model-broker.md §3.4). Only the SHA-256 of the plaintext is held;
// the plaintext itself is returned once, by Mint, and never stored.
// AuthorizeLaunch mints one per session under the label "session:<id>", and
// the model endpoint's auth resolver consults this table for an `rmk_`
// credential.
//
// A key dies three ways: an explicit Revoke (sessionAccount.end, on
// SessionExited or project delete), a process restart (the table is in
// memory), and — for a key bound to its session's launch with BindLaunch —
// the launch ending, whatever ends it.
type ModelKeyTable struct {
	mu   sync.Mutex
	byID map[string]modelKeyRecord // sha256 hex of the full plaintext -> record
}

func NewModelKeyTable() *ModelKeyTable {
	return &ModelKeyTable{byID: make(map[string]modelKeyRecord)}
}

func hashModelKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Mint generates a fresh rmk_ key bound to projectID, labelled label (the
// spec's convention is "session:<id>", but this table does not itself know
// about sessions — the caller picks the label). The plaintext is returned
// exactly once; only its hash is kept.
func (t *ModelKeyTable) Mint(projectID, label string) (string, error) {
	var raw [modelKeyHexLen / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("model key: %w", err)
	}
	plaintext := modelKeyPrefix + hex.EncodeToString(raw[:])

	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[hashModelKey(plaintext)] = modelKeyRecord{projectID: projectID, label: label}
	return plaintext, nil
}

// Revoke removes the key minted under (projectID, label). Scoped to both,
// not label alone: a label is the minting caller's own convention (e.g.
// "session:<id>"), not a value relay guarantees unique across projects, so
// scoping by project too is what stops one project's revoke call from
// reaching a same-labelled key that happens to belong to another. Safe to
// call for a (projectID, label) pair that was never minted or already
// revoked.
func (t *ModelKeyTable) Revoke(projectID, label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for hash, rec := range t.byID {
		if rec.projectID == projectID && rec.label == label {
			delete(t.byID, hash)
		}
	}
}

// RevokeKey removes exactly the key minted for plaintext, never any other
// key sharing its (project, label) pair. A launch rollback that only knows
// the exact value it itself minted -- not merely its label -- uses this
// instead of Revoke, so undoing one failed attempt can never reach a
// concurrent, successful one that happens to share the same label. Safe to
// call for a plaintext that was never minted or already revoked.
func (t *ModelKeyTable) RevokeKey(plaintext string) {
	if plaintext == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byID, hashModelKey(plaintext))
}

// BindLaunch ties plaintext's lifetime to launch's: from now on Lookup
// answers "no such key" the moment launch is no longer live, with nothing
// reporting the end. This is the same shape ModelHostRegistry uses for its
// host — liveness is re-derived from the launch table on every check, not
// tracked as a flag, and there is no sweeper — and it exists because
// revocation through sessionAccount.end depends on relay-sessions reporting
// SessionExited, which a crash, a failed report or a root that exits through
// the launch table's own watcher never delivers.
//
// Bind only a launch that says Hello (Launch.WasBound): its root-exit watcher
// is what ends it. A launch that never binds — a provider session with no
// shim — expires unbound after ProjectSessionLaunchTTL and would take its key
// with it seconds after the session started. Those keys keep SessionExited-
// only revocation. A key that was never minted or already revoked is left
// alone.
func (t *ModelKeyTable) BindLaunch(plaintext string, launch *service.Launch) {
	if plaintext == "" || launch == nil {
		return
	}
	hash := hashModelKey(plaintext)
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.byID[hash]; ok {
		rec.launch = launch
		t.byID[hash] = rec
	}
}

// Lookup reports the project a presented rmk_ bearer is scoped to, and its
// label for audit. ok is false for a key that was never minted, was
// revoked, or is bound to a launch that has ended. A direct map lookup by
// hash, not a scan: the hash itself is not
// a secret Lookup is trying to keep a scan-timing side channel away from
// (that concern applies to comparing a caller-controlled value against a
// stored secret, e.g. the admin token check elsewhere in this codebase —
// here the caller already had to know the plaintext to produce this exact
// hash, so map-bucket timing reveals nothing a successful Mint didn't
// already hand them).
func (t *ModelKeyTable) Lookup(bearer string) (projectID, label string, ok bool) {
	hash := hashModelKey(bearer)
	t.mu.Lock()
	rec, ok := t.byID[hash]
	t.mu.Unlock()
	if !ok {
		return "", "", false
	}
	// Asked outside t.mu: Live takes the launch table's lock, and holding
	// this one across it would order the two for anything that ever calls in
	// the other direction.
	if rec.launch != nil && !rec.launch.Live() {
		// The launch is gone for good, so is the key. Dropped here rather than
		// left for a sweeper; deleting by hash is idempotent against a
		// concurrent Revoke.
		t.mu.Lock()
		delete(t.byID, hash)
		t.mu.Unlock()
		return "", "", false
	}
	return rec.projectID, rec.label, true
}

// HasPrefix reports whether bearer has the rmk_ shape, the auth resolver's
// first branch (spec §3.1).
func HasModelKeyPrefix(bearer string) bool {
	return len(bearer) > len(modelKeyPrefix) && bearer[:len(modelKeyPrefix)] == modelKeyPrefix
}
