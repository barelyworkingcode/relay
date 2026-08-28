package main

import (
	"crypto/subtle"
	"errors"
	"time"
)

// bootstrapCodeTTL matches the anchor's whole job: short enough that the
// window between minting and redeeming is a deliberate, momentary act, not
// something an operator forgets was open (ADR-016 decision 2).
const bootstrapCodeTTL = 2 * time.Minute

// errBootstrapCodeInvalid is the one answer consumeBootstrapCode ever gives
// for failure. An absent record, an expired one, and a wrong plaintext are
// refused identically — a distinguishable refusal would let a caller with no
// access to the config dir learn whether registration is currently anchored
// at all, which is exactly the fact the anchor exists to keep from a page in
// the owner's browser.
var errBootstrapCodeInvalid = errors.New("invalid or expired login code")

// mintBootstrapCode generates a single-use registration code and stores only
// its SHA-256 plus a two-minute expiry, replacing any existing record: the
// anchor is one at a time, matching the one ceremony it authorises. Does not
// save; use within store.With, matching Mint.
func mintBootstrapCode(s *Settings) (string, error) {
	plaintext, err := generateRandomHex(16)
	if err != nil {
		return "", err
	}
	s.LoginBootstrap = &LoginBootstrap{
		Hash:    hashToken(plaintext),
		Expires: time.Now().UTC().Add(bootstrapCodeTTL).Format(time.RFC3339),
	}
	return plaintext, nil
}

// consumeBootstrapCode verifies plaintext against the stored anchor and, on
// success, deletes it so it cannot be replayed. It takes *Settings rather
// than a store so a caller can resolve and delete inside one store.With —
// the same TOCTOU reasoning docs/tokens.md gives for resolveAndRemove:
// reading the record in one call and deleting it in another would race a
// second process minting or consuming between the two.
func consumeBootstrapCode(s *Settings, plaintext string) error {
	b := s.LoginBootstrap
	if b == nil {
		return errBootstrapCodeInvalid
	}
	// This is deliberate: an Expires relay cannot parse reads as expired,
	// the same rule APICredential.Expired follows — a lifetime relay cannot
	// evaluate is never treated as still open.
	expired := true
	if at, err := time.Parse(time.RFC3339, b.Expires); err == nil {
		expired = !time.Now().Before(at)
	}
	match := subtle.ConstantTimeCompare([]byte(b.Hash), []byte(hashToken(plaintext))) == 1
	if expired || !match {
		return errBootstrapCodeInvalid
	}
	s.LoginBootstrap = nil
	return nil
}
