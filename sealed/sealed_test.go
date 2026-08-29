package sealed

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, aesKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return key
}

func mustSealer(t *testing.T, keyID string, key []byte) Sealer {
	t.Helper()
	s, err := NewAESSealer(keyID, key)
	if err != nil {
		t.Fatalf("NewAESSealer(%q): %v", keyID, err)
	}
	return s
}

// TestSeal_RoundTrip is the baseline: sealing a plaintext and unsealing the
// resulting envelope under the same key and the same AAD returns exactly
// the plaintext given, and marshalling the envelope through JSON and back
// changes nothing about that.
func TestSeal_RoundTrip(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	plaintext := []byte("hunter2-the-real-token")
	aad := []byte("projects/p1/token")

	env, err := sealer.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !env.Valid() {
		t.Fatal("Seal produced an invalid envelope")
	}
	if env.KeyID() != "0123456789abcdef" {
		t.Fatalf("KeyID() = %q, want the sealer's key id", env.KeyID())
	}

	// Round-trip through JSON, as settings.json would.
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var env2 Envelope
	if err := json.Unmarshal(wire, &env2); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, err := sealer.Unseal(env2, aad)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Unseal returned %q, want %q", got, plaintext)
	}
}

// TestSeal_EnvelopeIsSelfDescribing pins the exact §4.6 wire shape: an
// operator hand-reading settings.json must see "sealed": "v1" and the three
// other named keys, not an implementation-detail encoding.
func TestSeal_EnvelopeIsSelfDescribing(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	env, err := sealer.Seal([]byte("x"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	for _, key := range []string{"sealed", "key", "n", "ct"} {
		if _, ok := m[key]; !ok {
			t.Errorf("envelope JSON is missing %q: %s", key, wire)
		}
	}
	if got := m["sealed"]; got != "v1" {
		t.Errorf(`m["sealed"] = %v, want "v1"`, got)
	}
	if got := m["key"]; got != "0123456789abcdef" {
		t.Errorf(`m["key"] = %v, want the sealer's key id`, got)
	}
}

// TestSeal_NonceIsFreshEveryCall is the property §4.5 rests its "no
// nonce-reuse branch is needed" argument on: two Seal calls for the same
// plaintext under the same key never share a nonce, and therefore never
// share ciphertext bytes either.
func TestSeal_NonceIsFreshEveryCall(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	env1, err := sealer.Seal([]byte("same plaintext"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	env2, err := sealer.Seal([]byte("same plaintext"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if bytes.Equal(env1.nonce, env2.nonce) {
		t.Fatal("two Seal calls produced the same nonce")
	}
	if bytes.Equal(env1.ciphertext, env2.ciphertext) {
		t.Fatal("two Seal calls produced the same ciphertext")
	}
}

// TestSeal_WrongAADRejected proves the AAD is actually authenticated: an
// envelope sealed under one field path does not open under another, which
// is what stops a sealed admin_secret from being moved into a project's
// token slot in settings.json (§4.6).
func TestSeal_WrongAADRejected(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	env, err := sealer.Seal([]byte("secret"), []byte("admin_secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := sealer.Unseal(env, []byte("projects/p1/token")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Unseal with wrong AAD: got %v, want ErrCorrupt", err)
	}
}

// TestSeal_TamperedCiphertextRejected flips one bit of the ciphertext, the
// way a hand-edit or a corrupted disk would, and checks it is reported as
// corrupt rather than opened.
func TestSeal_TamperedCiphertextRejected(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	env, err := sealer.Seal([]byte("secret"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env.ciphertext[0] ^= 0xff

	if _, err := sealer.Unseal(env, []byte("field")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Unseal of tampered ciphertext: got %v, want ErrCorrupt", err)
	}
}

// TestSeal_TamperedNonceRejected does the same for the nonce.
func TestSeal_TamperedNonceRejected(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	env, err := sealer.Seal([]byte("secret"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env.nonce[0] ^= 0xff

	if _, err := sealer.Unseal(env, []byte("field")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Unseal of tampered nonce: got %v, want ErrCorrupt", err)
	}
}

// TestSeal_DifferentKeyRejected is AC-7's "a valid envelope under a
// different key id": a Sealer only ever holds one key, so an envelope
// sealed by a different one is refused by name rather than by a generic
// decrypt failure -- this is what lets a caller tell "wrong key" apart
// from "corrupt file" (§4.6).
func TestSeal_DifferentKeyRejected(t *testing.T) {
	sealerA := mustSealer(t, "aaaaaaaaaaaaaaaa", mustKey(t))
	sealerB := mustSealer(t, "bbbbbbbbbbbbbbbb", mustKey(t))

	env, err := sealerA.Seal([]byte("secret"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := sealerB.Unseal(env, []byte("field")); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Unseal under a different sealer: got %v, want ErrKeyMismatch", err)
	}
}

// TestSeal_KeyIDIsBoundToAAD is the acceptance test for §5.5.1's second
// rule: the key id is part of what GCM authenticates, not merely a label
// Unseal compares by string equality. It plants a scenario a bare equality
// check alone would let through: an envelope whose `key` field has been
// edited to name a *different* id, opened by a Sealer that reports that
// same (edited) id but holds the identical underlying key bytes as the
// original sealer. If the id were only ever compared as a string, this
// would look consistent and Unseal would succeed. Because the id is folded
// into the authenticated data at the byte level, the edit still invalidates
// the GCM tag.
func TestSeal_KeyIDIsBoundToAAD(t *testing.T) {
	key := mustKey(t)
	sealerOriginal := mustSealer(t, "1111111111111111", key)

	env, err := sealerOriginal.Seal([]byte("secret"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Edit the envelope's key field via the same JSON path an attacker
	// with write access to settings.json would use.
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	m["key"] = "2222222222222222"
	tamperedWire, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal tampered: %v", err)
	}
	var tampered Envelope
	if err := json.Unmarshal(tamperedWire, &tampered); err != nil {
		t.Fatalf("Unmarshal tampered: %v", err)
	}

	// A sealer that happens to hold the identical key bytes but reports
	// the tampered id: a bare id-equality check would accept this pair.
	sealerSameKeyNewID := mustSealer(t, "2222222222222222", key)

	if _, err := sealerSameKeyNewID.Unseal(tampered, []byte("field")); err == nil {
		t.Fatal("Unseal succeeded after the envelope's key field was edited to a different id sharing the same key bytes")
	}
}

func TestEnvelope_ClassifiesFormats(t *testing.T) {
	sealer := mustSealer(t, "0123456789abcdef", mustKey(t))
	valid, err := sealer.Seal([]byte("secret"), []byte("field"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	validWire, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	cases := []struct {
		name    string
		wire    string
		wantErr error
	}{
		{"valid v1 envelope", string(validWire), nil},
		{"unsupported version", `{"sealed":"v2","key":"0123456789abcdef","n":"AAA=","ct":"AAA="}`, ErrUnsupported},
		{"missing ct", `{"sealed":"v1","key":"0123456789abcdef","n":"AAA="}`, ErrCorrupt},
		{"missing sealed discriminator", `{"key":"0123456789abcdef","n":"AAA=","ct":"AAA="}`, ErrCorrupt},
		{"sealed is not a string", `{"sealed":1,"key":"0123456789abcdef","n":"AAA=","ct":"AAA="}`, ErrCorrupt},
		{"nonce is not valid base64", `{"sealed":"v1","key":"0123456789abcdef","n":"not-base64!","ct":"AAA="}`, ErrCorrupt},
		{"nonce is the wrong length", `{"sealed":"v1","key":"0123456789abcdef","n":"AAAAAAAAAAA=","ct":"AAA="}`, ErrCorrupt},
		{"not a JSON object: a bare string", `"a bare string"`, ErrCorrupt},
		{"not a JSON object: a number", `42`, ErrCorrupt},
		{"not a JSON object: null", `null`, ErrCorrupt},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var env Envelope
			err := json.Unmarshal([]byte(c.wire), &env)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("Unmarshal(%s): unexpected error %v", c.wire, err)
				}
				if !env.Valid() {
					t.Fatal("valid envelope reported itself as invalid")
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Unmarshal(%s): got %v, want %v", c.wire, err, c.wantErr)
			}
		})
	}
}

// TestEnvelope_ClassificationsAreDistinguishable is AC-7's other half:
// "unsupported" and "corrupt" must never collapse into the same sentinel,
// or an operator reading the two failures could not tell "this is a newer
// format relay doesn't understand yet" from "this file is broken".
func TestEnvelope_ClassificationsAreDistinguishable(t *testing.T) {
	var unsupported Envelope
	errUnsupported := json.Unmarshal([]byte(`{"sealed":"v2"}`), &unsupported)

	var corrupt Envelope
	errCorrupt := json.Unmarshal([]byte(`{"sealed":"v1"}`), &corrupt)

	if errors.Is(errUnsupported, ErrCorrupt) {
		t.Error("an unsupported format also matched ErrCorrupt")
	}
	if errors.Is(errCorrupt, ErrUnsupported) {
		t.Error("a corrupt object also matched ErrUnsupported")
	}
}

func TestEnvelope_MarshalOfUnsealedEnvelopeFails(t *testing.T) {
	var env Envelope
	if _, err := json.Marshal(env); err == nil {
		t.Fatal("marshalling the zero-value Envelope succeeded")
	}
}

func TestNewAESSealer_RejectsWrongKeyLength(t *testing.T) {
	if _, err := NewAESSealer("0123456789abcdef", make([]byte, 16)); err == nil {
		t.Fatal("NewAESSealer accepted a 16-byte key")
	}
}

func TestNewAESSealer_RejectsMalformedKeyID(t *testing.T) {
	cases := []string{"", "short", "0123456789abcdeg", "0123456789ABCDEF0123456789ABCDEF"}
	for _, id := range cases {
		if _, err := NewAESSealer(id, mustKey(t)); err == nil {
			t.Errorf("NewAESSealer accepted key id %q", id)
		}
	}
}

func TestMemoryKeyring_LoadOnEmptyKeyringIsMissingAndCreatesNothing(t *testing.T) {
	kr := NewMemoryKeyring("", nil)
	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("Load on empty keyring: got %v, want ErrKeyMissing", err)
	}
	// Loading twice must not have manufactured a key as a side effect --
	// this is §5.5.1's rule at the keyring's own boundary: Load never
	// creates, full stop, regardless of how many times it's called.
	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("second Load on empty keyring: got %v, want ErrKeyMissing", err)
	}
}

func TestMemoryKeyring_CreateThenLoadRoundTrips(t *testing.T) {
	kr := NewMemoryKeyring("", nil)
	keyID, key, err := kr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !validKeyID(keyID) {
		t.Fatalf("Create produced key id %q, not %d hex characters", keyID, keyIDHexLen)
	}
	if len(key) != aesKeySize {
		t.Fatalf("Create produced a %d-byte key, want %d", len(key), aesKeySize)
	}

	gotID, gotKey, err := kr.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotID != keyID || !bytes.Equal(gotKey, key) {
		t.Fatal("Load did not return what Create stored")
	}
}

func TestMemoryKeyring_CreateRefusesToOverwrite(t *testing.T) {
	kr := NewMemoryKeyring("", nil)
	firstID, _, err := kr.Create()
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, _, err := kr.Create(); err == nil {
		t.Fatal("second Create succeeded instead of refusing to overwrite")
	}
	// And the original key is still what Load reports -- a refused Create
	// must not have partially applied.
	gotID, _, err := kr.Load()
	if err != nil {
		t.Fatalf("Load after refused overwrite: %v", err)
	}
	if gotID != firstID {
		t.Fatalf("Load returned key id %q after a refused overwrite, want the original %q", gotID, firstID)
	}
}

func TestMemoryKeyring_DestroyThenLoadIsMissing(t *testing.T) {
	kr := NewMemoryKeyring("", nil)
	if _, _, err := kr.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := kr.Destroy(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("Load after Destroy: got %v, want ErrKeyMissing", err)
	}
}

// TestMemoryKeyring_ConstructorRequiresAnExplicitKey guards the seam
// description in keyring.go: NewMemoryKeyring is not itself a source of
// keys, only a place to put one a caller already generated. There is no
// variant that manufactures a key at construction time.
func TestMemoryKeyring_ConstructorRequiresAnExplicitKey(t *testing.T) {
	kr := NewMemoryKeyring("", nil)
	if _, _, err := kr.Load(); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("a keyring constructed with no key reported one present: err=%v", err)
	}
}
