package main

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
)

const (
	testOrigin = "http://localhost:8790"
	testRPID   = "localhost"
)

// The software authenticator below is a test instrument, not a reference
// client: every field a real authenticator would fix is a knob, because the
// negative table's whole job is to present output no real authenticator
// would produce.

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

func cborEncMap(entries ...[]byte) []byte {
	out := cborHead(5, uint64(len(entries)))
	for _, e := range entries {
		out = append(out, e...)
	}
	return out
}

func cborEntry(key, val []byte) []byte { return append(append([]byte{}, key...), val...) }

type softAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	aaguid []byte
}

func newSoftAuthenticator(t *testing.T) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("credential id: %v", err)
	}
	return &softAuthenticator{key: key, credID: credID, aaguid: make([]byte, aaguidLength)}
}

func (a *softAuthenticator) x() []byte {
	return a.key.PublicKey.X.FillBytes(make([]byte, coseCoordinateLength))
}

func (a *softAuthenticator) y() []byte {
	return a.key.PublicKey.Y.FillBytes(make([]byte, coseCoordinateLength))
}

// coseKey emits CTAP2 canonical order: 1, 3, -1, -2, -3.
func (a *softAuthenticator) coseKey() []byte {
	return coseKeyWith(coseKeyOpts{
		kty: cborEncUint(coseKeyTypeEC2),
		alg: cborEncNegInt(coseAlgES256),
		crv: cborEncUint(coseCurveP256),
		x:   cborEncBytes(a.x()),
		y:   cborEncBytes(a.y()),
	})
}

type coseKeyOpts struct {
	kty, alg, crv, x, y []byte
}

func coseKeyWith(o coseKeyOpts) []byte {
	return cborEncMap(
		cborEntry(cborEncUint(1), o.kty),
		cborEntry(cborEncUint(3), o.alg),
		cborEntry(cborEncNegInt(-1), o.crv),
		cborEntry(cborEncNegInt(-2), o.x),
		cborEntry(cborEncNegInt(-3), o.y),
	)
}

type clientDataOpts struct {
	Type        string
	Origin      string
	Challenge   []byte
	CrossOrigin *bool
	ExtraMember string
	Trailing    string
	Raw         []byte
}

func buildClientDataJSON(o clientDataOpts) []byte {
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

type authDataOpts struct {
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

func buildAuthData(o authDataOpts) []byte {
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

func rpIDHash(rpID string) []byte {
	h := sha256.Sum256([]byte(rpID))
	return h[:]
}

func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

type registrationCeremony struct {
	auth       *softAuthenticator
	ClientData clientDataOpts
	AuthData   authDataOpts
	Format     string
	AttStmt    []byte
	RawObject  []byte
	Entries    []([]byte)
}

func newRegistration(t *testing.T, v *WebAuthnVerifier, a *softAuthenticator) *registrationCeremony {
	t.Helper()
	challenge, err := v.IssueChallenge(WebAuthnCeremonyRegister)
	if err != nil {
		t.Fatalf("issue registration challenge: %v", err)
	}
	return &registrationCeremony{
		auth: a,
		ClientData: clientDataOpts{
			Type:        "webauthn.create",
			Origin:      v.Origin(),
			Challenge:   challenge,
			CrossOrigin: boolPtr(false),
		},
		AuthData: authDataOpts{
			RPIDHash:  rpIDHash(v.RPID()),
			Flags:     flagUserPresent | flagUserVerified | flagAttestedCredentialData,
			SignCount: 1,
			Attested:  true,
			AAGUID:    a.aaguid,
			CredID:    a.credID,
			COSEKey:   a.coseKey(),
		},
		Format:  "none",
		AttStmt: cborEncMap(),
	}
}

func (c *registrationCeremony) attestationObject() []byte {
	if c.RawObject != nil {
		return c.RawObject
	}
	entries := c.Entries
	if entries == nil {
		entries = [][]byte{
			cborEntry(cborEncText("fmt"), cborEncText(c.Format)),
			cborEntry(cborEncText("attStmt"), c.AttStmt),
			cborEntry(cborEncText("authData"), cborEncBytes(buildAuthData(c.AuthData))),
		}
	}
	return cborEncMap(entries...)
}

func (c *registrationCeremony) input(existing ...WebAuthnCredential) WebAuthnRegistrationInput {
	return WebAuthnRegistrationInput{
		ClientDataJSON:    buildClientDataJSON(c.ClientData),
		AttestationObject: c.attestationObject(),
		Existing:          existing,
	}
}

type assertionCeremony struct {
	auth         *softAuthenticator
	ClientData   clientDataOpts
	AuthData     authDataOpts
	CredentialID []byte
	UserHandle   []byte
	SignWith     *ecdsa.PrivateKey
	SignOver     []byte
	RawSignature []byte
}

func newAssertion(t *testing.T, v *WebAuthnVerifier, a *softAuthenticator) *assertionCeremony {
	t.Helper()
	challenge, err := v.IssueChallenge(WebAuthnCeremonyAssert)
	if err != nil {
		t.Fatalf("issue assertion challenge: %v", err)
	}
	return &assertionCeremony{
		auth: a,
		ClientData: clientDataOpts{
			Type:        "webauthn.get",
			Origin:      v.Origin(),
			Challenge:   challenge,
			CrossOrigin: boolPtr(false),
		},
		AuthData: authDataOpts{
			RPIDHash:  rpIDHash(v.RPID()),
			Flags:     flagUserPresent | flagUserVerified,
			SignCount: 2,
		},
		CredentialID: a.credID,
		SignWith:     a.key,
	}
}

func (c *assertionCeremony) sign(t *testing.T, authData, clientDataJSON []byte) []byte {
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

func (c *assertionCeremony) input(t *testing.T, creds ...WebAuthnCredential) WebAuthnAssertionInput {
	t.Helper()
	authData := buildAuthData(c.AuthData)
	clientDataJSON := buildClientDataJSON(c.ClientData)
	return WebAuthnAssertionInput{
		CredentialID:      c.CredentialID,
		ClientDataJSON:    clientDataJSON,
		AuthenticatorData: authData,
		Signature:         c.sign(t, authData, clientDataJSON),
		UserHandle:        c.UserHandle,
		Credentials:       creds,
	}
}
