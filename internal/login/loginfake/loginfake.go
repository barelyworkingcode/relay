// Package loginfake is a software WebAuthn authenticator: a test instrument,
// not a reference client. It exists in a separate, non-test package for the
// same reason internal/presence/presencetest does — cmd/relay's login_routes
// tests drive the real HTTP door from outside internal/login, and a Go test
// file's symbols do not cross a package boundary, so a shared fake for a
// black-box HTTP test cannot live inside internal/login's own _test.go files.
//
// It is deliberately NOT a copy of internal/login's unexported encoding: a
// real browser or authenticator does not share relay's Go constants either,
// so this package knows the WebAuthn/COSE/CTAP2 wire format on its own terms,
// independently of how internal/login happens to have implemented it. Every
// field a real authenticator would fix is instead a knob here, because
// exercising relay's negative paths means constructing shapes no real
// authenticator would ever produce.
package loginfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/login"
)

func init() {
	if !testing.Testing() {
		panic("loginfake: linked into a non-test binary; this package must never ship")
	}
}

// WebAuthn/COSE/CTAP2 wire-format constants this package needs to construct
// a ceremony. These are spec values, not relay's own tuning, and are kept
// separate from internal/login's identically-valued unexported constants on
// purpose (see the package doc).
const (
	aaguidLength         = 16
	coseCoordinateLength = 32
	coseKeyTypeEC2       = 2
	coseAlgES256         = -7
	coseCurveP256        = 1
	authDataBaseLength   = 37

	FlagUserPresent            byte = 1 << 0
	FlagUserVerified           byte = 1 << 2
	FlagAttestedCredentialData byte = 1 << 6
)

func cborHead(major byte, arg uint64) []byte {
	switch {
	case arg < 24:
		return []byte{major<<5 | byte(arg)}
	case arg < 1<<8:
		return []byte{major<<5 | 24, byte(arg)}
	case arg < 1<<16:
		b := []byte{major<<5 | 25, 0, 0}
		binary.BigEndian.PutUint16(b[1:], uint16(arg))
		return b
	default:
		b := []byte{major<<5 | 26, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(arg))
		return b
	}
}

func cborEncUint(n uint64) []byte { return cborHead(0, n) }

func cborEncNegInt(n int64) []byte { return cborHead(1, uint64(-1-n)) }

func cborEncBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }

func cborEncText(s string) []byte { return append(cborHead(3, uint64(len(s))), s...) }

// CBOREncMap builds a canonical CBOR map from key/value entries already
// encoded with CBOREntry. Called with no entries it builds an empty map,
// which is what an attestation statement of format "none" carries.
func CBOREncMap(entries ...[]byte) []byte {
	out := cborHead(5, uint64(len(entries)))
	for _, e := range entries {
		out = append(out, e...)
	}
	return out
}

// CBOREntry pairs an already-encoded key and value for CBOREncMap.
func CBOREntry(key, val []byte) []byte { return append(append([]byte{}, key...), val...) }

// SoftAuthenticator is a P-256 keypair plus the identifiers a registration
// binds to it.
type SoftAuthenticator struct {
	Key    *ecdsa.PrivateKey
	CredID []byte
	AAGUID []byte
}

func NewSoftAuthenticator(t *testing.T) *SoftAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("credential id: %v", err)
	}
	return &SoftAuthenticator{Key: key, CredID: credID, AAGUID: make([]byte, aaguidLength)}
}

func (a *SoftAuthenticator) x() []byte {
	return a.Key.X.FillBytes(make([]byte, coseCoordinateLength))
}

func (a *SoftAuthenticator) y() []byte {
	return a.Key.Y.FillBytes(make([]byte, coseCoordinateLength))
}

// CoseKey emits CTAP2 canonical order: 1, 3, -1, -2, -3.
func (a *SoftAuthenticator) CoseKey() []byte {
	return CBOREncMap(
		CBOREntry(cborEncUint(1), cborEncUint(coseKeyTypeEC2)),
		CBOREntry(cborEncUint(3), cborEncNegInt(coseAlgES256)),
		CBOREntry(cborEncNegInt(-1), cborEncUint(coseCurveP256)),
		CBOREntry(cborEncNegInt(-2), cborEncBytes(a.x())),
		CBOREntry(cborEncNegInt(-3), cborEncBytes(a.y())),
	)
}

// ClientDataOpts builds the clientDataJSON a browser would send. Raw
// overrides every other field when set, for the negative cases that need
// bytes no browser would produce.
type ClientDataOpts struct {
	Type        string
	Origin      string
	Challenge   []byte
	CrossOrigin *bool
	ExtraMember string
	Trailing    string
	Raw         []byte
}

func buildClientDataJSON(o ClientDataOpts) []byte {
	if o.Raw != nil {
		return o.Raw
	}
	members := []string{
		fmt.Sprintf("%q:%q", "type", o.Type),
		fmt.Sprintf("%q:%q", "challenge", base64.RawURLEncoding.EncodeToString(o.Challenge)),
		fmt.Sprintf("%q:%q", "origin", o.Origin),
	}
	if o.CrossOrigin != nil {
		members = append(members, fmt.Sprintf("%q:%t", "crossOrigin", *o.CrossOrigin))
	}
	if o.ExtraMember != "" {
		members = append(members, o.ExtraMember)
	}
	return []byte("{" + strings.Join(members, ",") + "}" + o.Trailing)
}

// AuthDataOpts builds the authenticatorData bytes.
type AuthDataOpts struct {
	RPIDHash  []byte
	Flags     byte
	SignCount uint32
	Attested  bool
	AAGUID    []byte
	CredID    []byte
	CredIDLen int
	COSEKey   []byte
	Trailing  []byte
}

func buildAuthData(o AuthDataOpts) []byte {
	out := make([]byte, 0, authDataBaseLength)
	out = append(out, o.RPIDHash...)
	out = append(out, o.Flags)
	out = binary.BigEndian.AppendUint32(out, o.SignCount)
	if o.Attested {
		out = append(out, o.AAGUID...)
		length := o.CredIDLen
		if length == 0 {
			length = len(o.CredID)
		}
		out = binary.BigEndian.AppendUint16(out, uint16(length))
		out = append(out, o.CredID...)
		out = append(out, o.COSEKey...)
	}
	return append(out, o.Trailing...)
}

// RPIDHash is what a real authenticator binds authenticatorData to.
func RPIDHash(rpID string) []byte {
	h := sha256.Sum256([]byte(rpID))
	return h[:]
}

// RegistrationCeremony builds the wire bytes for one registration.
type RegistrationCeremony struct {
	ClientData ClientDataOpts
	AuthData   AuthDataOpts
	Format     string
	AttStmt    []byte
	RawObject  []byte
	Entries    [][]byte
}

func (c *RegistrationCeremony) attestationObject() []byte {
	if c.RawObject != nil {
		return c.RawObject
	}
	entries := c.Entries
	if entries == nil {
		entries = [][]byte{
			CBOREntry(cborEncText("fmt"), cborEncText(c.Format)),
			CBOREntry(cborEncText("attStmt"), c.AttStmt),
			CBOREntry(cborEncText("authData"), cborEncBytes(buildAuthData(c.AuthData))),
		}
	}
	return CBOREncMap(entries...)
}

// Input builds the verifier's wire-level input for this registration.
func (c *RegistrationCeremony) Input(existing ...login.WebAuthnCredential) login.WebAuthnRegistrationInput {
	return login.WebAuthnRegistrationInput{
		ClientDataJSON:    buildClientDataJSON(c.ClientData),
		AttestationObject: c.attestationObject(),
		Existing:          existing,
	}
}

// AssertionCeremony builds the wire bytes for one assertion.
type AssertionCeremony struct {
	ClientData   ClientDataOpts
	AuthData     AuthDataOpts
	CredentialID []byte
	UserHandle   []byte
	SignWith     *ecdsa.PrivateKey
	SignOver     []byte
	RawSignature []byte
}

func (c *AssertionCeremony) sign(t *testing.T, authData, clientDataJSON []byte) []byte {
	t.Helper()
	if c.RawSignature != nil {
		return c.RawSignature
	}
	message := c.SignOver
	if message == nil {
		hash := sha256.Sum256(clientDataJSON)
		message = append(append([]byte{}, authData...), hash[:]...)
	}
	digest := sha256.Sum256(message)
	sig, err := ecdsa.SignASN1(rand.Reader, c.SignWith, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return sig
}

// Input builds the verifier's wire-level input for this assertion.
func (c *AssertionCeremony) Input(t *testing.T, creds ...login.WebAuthnCredential) login.WebAuthnAssertionInput {
	t.Helper()
	authData := buildAuthData(c.AuthData)
	clientDataJSON := buildClientDataJSON(c.ClientData)
	return login.WebAuthnAssertionInput{
		CredentialID:      c.CredentialID,
		ClientDataJSON:    clientDataJSON,
		AuthenticatorData: authData,
		Signature:         c.sign(t, authData, clientDataJSON),
		UserHandle:        c.UserHandle,
		Credentials:       creds,
	}
}
