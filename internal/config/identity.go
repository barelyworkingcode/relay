package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken returns the persisted SHA-256 token identifier used for constant
// time authentication lookups. Plaintext tokens are never persisted.
func HashToken(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

const (
	CAKeyFile       = "ca.key"
	CAKeySealedFile = "ca.key.sealed"
	CAAADPrefix     = "relay-ca-v1\x00"
)
