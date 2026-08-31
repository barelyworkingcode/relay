package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// sasAlphabet drops every member of the 0/O and 1/I/l confusion classes: 0, 1,
// I and O are absent and lowercase never appears, so l cannot either. L is
// unambiguous once 1 and I are both gone.
const sasAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const sasNonceBytes = 16

// The trailing NUL is part of the hashed bytes, not a Go string terminator.
// It makes each tag fixed-length and prefix-free against the other; with every
// field that follows also fixed-length, the preimage needs no length framing.
const (
	sasDomain       = "relay.sas.v1\x00"
	sasCommitDomain = "relay.sas.commit.v1\x00"
)

// newSASNonce returns a fresh nonce as 32 lowercase hex characters.
func newSASNonce() (string, error) {
	b := make([]byte, sasNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sasCommitment is the hiding commitment the client lodges before relay mints
// its own nonce. It binds the CSR's public key: a commitment captured off the
// wire does not open against any other key, so it cannot be replayed under an
// attacker's CSR.
func sasCommitment(csrSPKISHA256 [32]byte, rc []byte) string {
	buf := make([]byte, 0, len(sasCommitDomain)+len(csrSPKISHA256)+len(rc))
	buf = append(buf, sasCommitDomain...)
	buf = append(buf, csrSPKISHA256[:]...)
	buf = append(buf, rc...)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// computeSAS derives the six-character comparison code from both public keys
// and both nonces. caSPKI and csrSPKI are raw DER SubjectPublicKeyInfo.
func computeSAS(caSPKI, csrSPKI, rc, rr []byte) string {
	caSum := sha256.Sum256(caSPKI)
	csrSum := sha256.Sum256(csrSPKI)
	buf := make([]byte, 0, len(sasDomain)+len(caSum)+len(csrSum)+len(rc)+len(rr))
	buf = append(buf, sasDomain...)
	buf = append(buf, caSum[:]...)
	buf = append(buf, csrSum[:]...)
	buf = append(buf, rc...)
	buf = append(buf, rr...)
	return enc30(sha256.Sum256(buf))
}

// enc30 encodes the first 30 bits of the digest, big-endian from the front, as
// six alphabet symbols, most significant group first.
func enc30(digest [32]byte) string {
	v := uint32(digest[0])<<22 | uint32(digest[1])<<14 | uint32(digest[2])<<6 | uint32(digest[3])>>2
	var out [6]byte
	for i := 5; i >= 0; i-- {
		out[5-i] = sasAlphabet[(v>>(5*uint(i)))&31]
	}
	return string(out[:])
}

// validSASHex reports whether s is exactly wantBytes bytes rendered as
// lowercase hex.
func validSASHex(s string, wantBytes int) bool {
	if len(s) != 2*wantBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
