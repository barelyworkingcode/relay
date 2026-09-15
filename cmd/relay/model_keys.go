package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
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
}

// ModelKeyTable is relay's in-memory table of minted model keys
// (spec-model-broker.md §3.4). Only the SHA-256 of the plaintext is held;
// the plaintext itself is returned once, by Mint, and never stored. Nothing
// mints a key yet except tests — the bridge op to do so (MintModelKey) is a
// later unit (plan-broker-and-sessions.md F5); this table exists now so the
// model endpoint's auth resolver has a real table to consult for an `rmk_`
// bearer.
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

// Lookup reports the project a presented rmk_ bearer is scoped to, and its
// label for audit. ok is false for a key that was never minted or was
// revoked. A direct map lookup by hash, not a scan: the hash itself is not
// a secret Lookup is trying to keep a scan-timing side channel away from
// (that concern applies to comparing a caller-controlled value against a
// stored secret, e.g. the admin token check elsewhere in this codebase —
// here the caller already had to know the plaintext to produce this exact
// hash, so map-bucket timing reveals nothing a successful Mint didn't
// already hand them).
func (t *ModelKeyTable) Lookup(bearer string) (projectID, label string, ok bool) {
	hash := hashModelKey(bearer)
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.byID[hash]
	return rec.projectID, rec.label, ok
}

// HasPrefix reports whether bearer has the rmk_ shape, the auth resolver's
// first branch (spec §3.1).
func HasModelKeyPrefix(bearer string) bool {
	return len(bearer) > len(modelKeyPrefix) && bearer[:len(modelKeyPrefix)] == modelKeyPrefix
}
