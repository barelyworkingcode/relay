// Package sealed holds the envelope format and the two crypto/keyring
// primitives that ADR-017 needs to move a secret off disk and into an
// AES-256-GCM envelope keyed from the login keychain.
//
// It is a package of its own rather than files in main for one reason that
// matters and one that is convenience: Envelope's fields are unexported so
// package main cannot construct or edit one by hand, and the cgo keychain
// code (keychain_darwin.go) is easier to build-tag in isolation.
//
// This package is wired into nothing yet. Nothing outside it imports it.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	sealedFormatV1 = "v1"
	aesKeySize     = 32 // AES-256
	gcmNonceSize   = 12 // AES-GCM standard nonce size
	keyIDHexLen    = 16 // §4.6: the id is 16 hex characters
)

// Sentinel errors, each naming one of §4.6's distinguishable refusals.
// A caller matches these with errors.Is; the message text an operator sees
// is built one layer up, where the field path and the settings-level
// context (is this a per-field failure or a whole-store key mismatch?) are
// known.
var (
	ErrKeyMissing  = errors.New("sealing key not found")
	ErrKeyMismatch = errors.New("sealing key id does not match")
	ErrCorrupt     = errors.New("sealed value is corrupt")
	ErrUnsupported = errors.New("unsupported seal format")
)

// Envelope is one sealed value: AES-256-GCM ciphertext plus what is needed
// to open it again, marshalling to and from the §4.6 JSON object. Its
// fields are unexported, so the only way to produce a valid one is a
// Sealer's Seal, and the only way to read one back is json.Unmarshal (which
// classifies the input per §4.6) or a Sealer's Unseal.
type Envelope struct {
	keyID      string
	nonce      []byte
	ciphertext []byte
	valid      bool
}

// KeyID reports the id this envelope claims to be sealed with, read
// straight from its (possibly tampered) `key` field. It exists so a caller
// building the AAD for Unseal (§4.6) can fold in what the envelope itself
// claims, rather than what it hopes is true.
func (e Envelope) KeyID() string { return e.keyID }

// Valid reports whether e is a genuine envelope rather than the Envelope
// zero value or one whose UnmarshalJSON never succeeded.
func (e Envelope) Valid() bool { return e.valid }

// envelopeWire is the exact §4.6 JSON shape.
type envelopeWire struct {
	Sealed string `json:"sealed"`
	Key    string `json:"key"`
	Nonce  string `json:"n"`
	CT     string `json:"ct"`
}

// MarshalJSON emits the §4.6 envelope object.
//
// This is deliberate: marshalling the zero value (or any Envelope that
// never got through UnmarshalJSON or Seal) is refused rather than emitting
// a half-formed object, on the same residue-rule reasoning as
// Secret.MarshalJSON one layer up — a bug that produces an unsealed value
// must fail loudly here, not write four empty strings that look sealed.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if !e.valid {
		return nil, fmt.Errorf("sealed: cannot marshal an unsealed envelope")
	}
	return json.Marshal(envelopeWire{
		Sealed: sealedFormatV1,
		Key:    e.keyID,
		Nonce:  base64.StdEncoding.EncodeToString(e.nonce),
		CT:     base64.StdEncoding.EncodeToString(e.ciphertext),
	})
}

// UnmarshalJSON classifies b per §4.6's three-way split among an
// object that is sealed, one that names an unsupported format, and one
// that is simply corrupt. It never returns a "legacy plaintext" result —
// that classification (bare JSON string vs. object) is Secret's job one
// layer up, since Envelope is only ever reached once something else has
// already decided the value is an object.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return fmt.Errorf("%w: not a JSON object", ErrCorrupt)
	}

	rawSealed, ok := fields["sealed"]
	if !ok {
		return fmt.Errorf("%w: missing \"sealed\" discriminator", ErrCorrupt)
	}
	var sealed string
	if err := json.Unmarshal(rawSealed, &sealed); err != nil {
		return fmt.Errorf("%w: \"sealed\" is not a string", ErrCorrupt)
	}
	// This is subtle: the format check comes before any other field is
	// read, so "sealed": "v2" is reported as unsupported even if the rest
	// of the object is garbage — an operator reading the error should
	// learn "this is a newer format", not "this is corrupt".
	if sealed != sealedFormatV1 {
		return fmt.Errorf("%w: %q", ErrUnsupported, sealed)
	}

	var wire envelopeWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if wire.Key == "" || wire.Nonce == "" || wire.CT == "" {
		return fmt.Errorf("%w: missing key, n, or ct", ErrCorrupt)
	}
	nonce, err := base64.StdEncoding.DecodeString(wire.Nonce)
	if err != nil {
		return fmt.Errorf("%w: n is not valid base64", ErrCorrupt)
	}
	if len(nonce) != gcmNonceSize {
		return fmt.Errorf("%w: n is %d bytes, want %d", ErrCorrupt, len(nonce), gcmNonceSize)
	}
	ct, err := base64.StdEncoding.DecodeString(wire.CT)
	if err != nil {
		return fmt.Errorf("%w: ct is not valid base64", ErrCorrupt)
	}

	e.keyID = wire.Key
	e.nonce = nonce
	e.ciphertext = ct
	e.valid = true
	return nil
}

// Sealer seals and opens envelopes under exactly one key.
type Sealer interface {
	KeyID() string
	Seal(plaintext, aad []byte) (Envelope, error)
	Unseal(e Envelope, aad []byte) ([]byte, error)
}

type aesSealer struct {
	keyID string
	aead  cipher.AEAD
}

// NewAESSealer builds a Sealer over a 32-byte AES-256 key, identified by
// keyID (16 hex characters, per §4.6). Constructed at exactly one place in
// production — runTrayApp, from the keychain key (§5.1) — and directly, with
// a fixed key, by tests.
func NewAESSealer(keyID string, key []byte) (Sealer, error) {
	if len(key) != aesKeySize {
		return nil, fmt.Errorf("sealed: key must be %d bytes, got %d", aesKeySize, len(key))
	}
	if !validKeyID(keyID) {
		return nil, fmt.Errorf("sealed: key id %q is not %d hex characters", keyID, keyIDHexLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sealed: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealed: %w", err)
	}
	return &aesSealer{keyID: keyID, aead: aead}, nil
}

func (s *aesSealer) KeyID() string { return s.keyID }

// Seal generates a fresh random nonce on every call, so a repeated GCM
// nonce under this key is unreachable by construction rather than merely
// avoided by callers re-sealing on every write (§4.5).
func (s *aesSealer) Seal(plaintext, aad []byte) (Envelope, error) {
	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, fmt.Errorf("sealed: generating nonce: %w", err)
	}
	ct := s.aead.Seal(nil, nonce, plaintext, s.bind(aad))
	return Envelope{keyID: s.keyID, nonce: nonce, ciphertext: ct, valid: true}, nil
}

// Unseal refuses before it ever attempts decryption when e claims a key
// this Sealer does not hold — the sealer only ever knows one key, so this
// is a cheap, exact check rather than a guess. It exists beside the AAD
// binding in bind, not instead of it: this catches "this envelope is not
// for me" as a named ErrKeyMismatch, while bind is what makes the id
// itself part of what GCM authenticates (§5.5.1) rather than a label a
// caller could reattach to a different ciphertext.
func (s *aesSealer) Unseal(e Envelope, aad []byte) ([]byte, error) {
	if !e.Valid() {
		return nil, fmt.Errorf("sealed: %w", ErrCorrupt)
	}
	if e.keyID != s.keyID {
		return nil, fmt.Errorf("sealed: %w: envelope key %q, sealer key %q", ErrKeyMismatch, e.keyID, s.keyID)
	}
	pt, err := s.aead.Open(nil, e.nonce, e.ciphertext, s.bind(aad))
	if err != nil {
		return nil, fmt.Errorf("sealed: %w", ErrCorrupt)
	}
	return pt, nil
}

// bind folds this Sealer's key id into the bytes GCM authenticates, ahead
// of whatever the caller passed as aad (a field path, per §4.6). This is
// the AEAD-associated-data half of §5.5.1's key-substitution defence: an
// envelope's `key` field cannot be edited independently of its ciphertext,
// because doing so changes the bytes GCM authenticated at Seal time and the
// tag no longer verifies — even against a Sealer that now (perhaps also
// tampered) reports the same id, unless it also holds the exact same
// underlying key.
func (s *aesSealer) bind(aad []byte) []byte {
	out := make([]byte, 0, len(s.keyID)+1+len(aad))
	out = append(out, s.keyID...)
	out = append(out, 0)
	return append(out, aad...)
}

func validKeyID(id string) bool {
	if len(id) != keyIDHexLen {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
