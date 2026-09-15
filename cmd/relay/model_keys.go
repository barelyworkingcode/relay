package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

// Revoke removes every key minted under label. Safe to call for a label that
// was never minted or already revoked.
func (t *ModelKeyTable) Revoke(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for hash, rec := range t.byID {
		if rec.label == label {
			delete(t.byID, hash)
		}
	}
}

// Lookup reports the project a presented rmk_ bearer is scoped to, and its
// label for audit. ok is false for a key that was never minted, was
// revoked, or does not have the rmk_ shape at all — callers should still
// check the prefix themselves before spending a lookup, since Lookup treats
// an unrecognised shape identically to a revoked key (both are "not this
// key"), which is the same fail-closed non-distinction §2.4 asks of a
// disallowed vs. unknown model.
func (t *ModelKeyTable) Lookup(bearer string) (projectID, label string, ok bool) {
	hash := hashModelKey(bearer)
	t.mu.Lock()
	defer t.mu.Unlock()
	for storedHash, rec := range t.byID {
		if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash)) == 1 {
			return rec.projectID, rec.label, true
		}
	}
	return "", "", false
}

// HasPrefix reports whether bearer has the rmk_ shape, the auth resolver's
// first branch (spec §3.1).
func HasModelKeyPrefix(bearer string) bool {
	return len(bearer) > len(modelKeyPrefix) && bearer[:len(modelKeyPrefix)] == modelKeyPrefix
}
