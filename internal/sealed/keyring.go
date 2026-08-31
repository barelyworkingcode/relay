package sealed

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// Keyring is where the 32-byte key lives between runs.
//
// Load never creates: a caller that wants a key to exist and finds none
// must treat that as ErrKeyMissing, never as a reason to call Create
// itself. §5.5.1 depends on this being true of every implementation.
type Keyring interface {
	// Load returns the stored key, or ErrKeyMissing if there is none.
	Load() (keyID string, key []byte, err error)
	// Create generates a new key and stores it, refusing if one already
	// exists rather than overwriting it.
	Create() (keyID string, key []byte, err error)
	// Destroy removes the stored key. Break-glass only (§5.6 clause 5).
	Destroy() error
}

// generateKey produces a fresh 32-byte AES-256 key and a 16-hex-character
// id for it, both from crypto/rand. Shared by every Keyring implementation
// so "what a newly created key looks like" has exactly one definition.
func generateKey() (keyID string, key []byte, err error) {
	key = make([]byte, aesKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", nil, fmt.Errorf("sealed: generating key: %w", err)
	}
	idBytes := make([]byte, keyIDHexLen/2)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, fmt.Errorf("sealed: generating key id: %w", err)
	}
	return hex.EncodeToString(idBytes), key, nil
}

type memoryKeyring struct {
	mu    sync.Mutex
	keyID string
	key   []byte
}

// NewMemoryKeyring returns a Keyring backed by nothing but the two values
// given. It is used by tests and by nothing in production, and it is not a
// seam that weakens anything: a caller must supply the key, so there is no
// switch that silently produces or substitutes one. Pass "", nil for a
// keyring that starts out empty, to exercise Load's ErrKeyMissing path and
// Create's generation path in tests.
func NewMemoryKeyring(keyID string, key []byte) Keyring {
	return &memoryKeyring{keyID: keyID, key: append([]byte(nil), key...)}
}

func (m *memoryKeyring) Load() (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyID == "" || len(m.key) == 0 {
		return "", nil, ErrKeyMissing
	}
	return m.keyID, append([]byte(nil), m.key...), nil
}

func (m *memoryKeyring) Create() (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyID != "" || len(m.key) != 0 {
		return "", nil, errors.New("sealed: a key already exists; refusing to overwrite")
	}
	keyID, key, err := generateKey()
	if err != nil {
		return "", nil, err
	}
	m.keyID, m.key = keyID, key
	return keyID, append([]byte(nil), key...), nil
}

func (m *memoryKeyring) Destroy() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyID, m.key = "", nil
	return nil
}
