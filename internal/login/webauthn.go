package login

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/barelyworkingcode/relay/internal/ceremonylimit"
)

const (
	flagUserPresent            byte = 1 << 0
	flagUserVerified           byte = 1 << 2
	flagAttestedCredentialData byte = 1 << 6
	flagExtensionData          byte = 1 << 7

	authDataBaseLength    = 37
	aaguidLength          = 16
	maxCredentialIDLength = 1023
	maxClientDataLength   = 4096
	maxSignatureLength    = 256
	coseCoordinateLength  = 32

	// MaxRegisteredPasskeys caps what an owner may register (ADR-016
	// decision 7, point 12).
	MaxRegisteredPasskeys = 5

	coseKeyTypeEC2 = 2
	coseAlgES256   = -7
	coseCurveP256  = 1
)

var (
	errWebAuthnAuthDataShape     = errors.New("webauthn: malformed authenticator data")
	errWebAuthnExtensions        = errors.New("webauthn: extension data is not supported")
	errWebAuthnAttestedData      = errors.New("webauthn: attested credential data")
	errWebAuthnAttestationFormat = errors.New("webauthn: unsupported attestation format")
	errWebAuthnAttestationStmt   = errors.New("webauthn: attestation statement must be empty")
	errWebAuthnAttestationShape  = errors.New("webauthn: malformed attestation object")
	errWebAuthnCOSEKey           = errors.New("webauthn: unsupported COSE key")
	errWebAuthnAlgorithm         = errors.New("webauthn: unsupported COSE algorithm")
	errWebAuthnClientData        = errors.New("webauthn: malformed client data")
	errWebAuthnCeremonyType      = errors.New("webauthn: wrong ceremony type")
	errWebAuthnOrigin            = errors.New("webauthn: origin mismatch")
	errWebAuthnCrossOrigin       = errors.New("webauthn: cross-origin ceremony")
	errWebAuthnChallenge         = errors.New("webauthn: unknown, expired or already used challenge")
	errWebAuthnRPIDHash          = errors.New("webauthn: relying party id hash mismatch")
	errWebAuthnUserPresent       = errors.New("webauthn: user presence flag not set")
	errWebAuthnUserVerified      = errors.New("webauthn: user verification flag not set")
	errWebAuthnUserHandle        = errors.New("webauthn: user handle does not match the credential")
	ErrWebAuthnPasskeyLimit      = fmt.Errorf("webauthn: at most %d passkeys may be registered", MaxRegisteredPasskeys)
	ErrWebAuthnDuplicateCred     = errors.New("webauthn: credential is already registered")
	ErrWebAuthnCounter           = errors.New("webauthn: signature counter did not increase")

	// ErrWebAuthnAssertionRejected is the single answer to a credential id
	// that resolves to nothing AND to a signature that does not verify
	// (ADR-016 decision 7, point 9). Splitting it would tell an
	// unauthenticated caller which credential ids exist.
	ErrWebAuthnAssertionRejected = errors.New("webauthn: assertion rejected")
)

// WebAuthnCounterError names the credential so the call site can audit the
// cloned-authenticator signal. It carries no instruction to disable that
// credential, because one replayed stale assertion must not lock the owner
// out (ADR-016 decision 7, point 10).
type WebAuthnCounterError struct {
	CredentialID []byte
	Stored       uint32
	Received     uint32
}

func (e *WebAuthnCounterError) Error() string {
	return fmt.Sprintf("%s: credential %s stored %d, received %d",
		ErrWebAuthnCounter, base64.RawURLEncoding.EncodeToString(e.CredentialID), e.Stored, e.Received)
}

func (e *WebAuthnCounterError) Unwrap() error { return ErrWebAuthnCounter }

type AuthenticatorData struct {
	Raw       []byte
	RPIDHash  []byte
	Flags     byte
	SignCount uint32

	AAGUID       []byte
	CredentialID []byte
	PublicKeyX   []byte
	PublicKeyY   []byte
}

func (a *AuthenticatorData) UserPresent() bool  { return a.Flags&flagUserPresent != 0 }
func (a *AuthenticatorData) UserVerified() bool { return a.Flags&flagUserVerified != 0 }
func (a *AuthenticatorData) HasAttestedCredentialData() bool {
	return a.Flags&flagAttestedCredentialData != 0
}

// ParseAuthenticatorData refuses trailing bytes in both shapes: an assertion
// is exactly 37 bytes, and a registration is exactly the length its flags
// imply (ADR-016 decision 7, point 11).
func ParseAuthenticatorData(data []byte) (*AuthenticatorData, error) {
	if len(data) < authDataBaseLength {
		return nil, fmt.Errorf("%w: %d bytes, want at least %d", errWebAuthnAuthDataShape, len(data), authDataBaseLength)
	}
	a := &AuthenticatorData{
		Raw:       data,
		RPIDHash:  data[0:32],
		Flags:     data[32],
		SignCount: binary.BigEndian.Uint32(data[33:37]),
	}
	if a.Flags&flagExtensionData != 0 {
		return nil, errWebAuthnExtensions
	}
	rest := data[authDataBaseLength:]
	if !a.HasAttestedCredentialData() {
		if len(rest) != 0 {
			return nil, fmt.Errorf("%w: %d trailing bytes with no attested credential data", errWebAuthnAuthDataShape, len(rest))
		}
		return a, nil
	}
	if len(rest) < aaguidLength+2 {
		return nil, fmt.Errorf("%w: %d bytes after the header", errWebAuthnAttestedData, len(rest))
	}
	a.AAGUID = rest[:aaguidLength]
	idLen := int(binary.BigEndian.Uint16(rest[aaguidLength : aaguidLength+2]))
	rest = rest[aaguidLength+2:]
	if idLen == 0 || idLen > maxCredentialIDLength {
		return nil, fmt.Errorf("%w: credential id length %d", errWebAuthnAttestedData, idLen)
	}
	if len(rest) < idLen {
		return nil, fmt.Errorf("%w: credential id length %d exceeds %d remaining bytes", errWebAuthnAttestedData, idLen, len(rest))
	}
	a.CredentialID = rest[:idLen]
	key, err := cborParse(rest[idLen:])
	if err != nil {
		return nil, fmt.Errorf("%w: public key: %w", errWebAuthnAttestedData, err)
	}
	a.PublicKeyX, a.PublicKeyY, err = coseES256Coordinates(key)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func coseES256Coordinates(key cborItem) ([]byte, []byte, error) {
	if err := key.requireMap(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errWebAuthnCOSEKey, err)
	}
	if len(key.pairs) != 5 {
		return nil, nil, fmt.Errorf("%w: %d entries, want 5", errWebAuthnCOSEKey, len(key.pairs))
	}
	intAt := func(label int64) (int64, error) {
		item, ok := key.lookupInt(label)
		if !ok {
			return 0, fmt.Errorf("%w: no label %d", errWebAuthnCOSEKey, label)
		}
		if item.kind != cborUnsigned && item.kind != cborNegative {
			return 0, fmt.Errorf("%w: label %d is a %s", errWebAuthnCOSEKey, label, item.kind)
		}
		return item.n, nil
	}
	coordAt := func(label int64) ([]byte, error) {
		item, ok := key.lookupInt(label)
		if !ok {
			return nil, fmt.Errorf("%w: no label %d", errWebAuthnCOSEKey, label)
		}
		if item.kind != cborBytes {
			return nil, fmt.Errorf("%w: label %d is a %s", errWebAuthnCOSEKey, label, item.kind)
		}
		if len(item.b) != coseCoordinateLength {
			return nil, fmt.Errorf("%w: label %d is %d bytes, want %d", errWebAuthnCOSEKey, label, len(item.b), coseCoordinateLength)
		}
		return item.b, nil
	}
	alg, err := intAt(3)
	if err != nil {
		return nil, nil, err
	}
	if alg != coseAlgES256 {
		return nil, nil, fmt.Errorf("%w: alg %d, only ES256 (%d) is accepted", errWebAuthnAlgorithm, alg, coseAlgES256)
	}
	kty, err := intAt(1)
	if err != nil {
		return nil, nil, err
	}
	if kty != coseKeyTypeEC2 {
		return nil, nil, fmt.Errorf("%w: kty %d, want %d", errWebAuthnCOSEKey, kty, coseKeyTypeEC2)
	}
	crv, err := intAt(-1)
	if err != nil {
		return nil, nil, err
	}
	if crv != coseCurveP256 {
		return nil, nil, fmt.Errorf("%w: crv %d, want %d", errWebAuthnCOSEKey, crv, coseCurveP256)
	}
	x, err := coordAt(-2)
	if err != nil {
		return nil, nil, err
	}
	y, err := coordAt(-3)
	if err != nil {
		return nil, nil, err
	}
	if _, err := webauthnPublicKey(x, y); err != nil {
		return nil, nil, err
	}
	return x, y, nil
}

func webauthnPublicKey(x, y []byte) (*ecdsa.PublicKey, error) {
	if len(x) != coseCoordinateLength || len(y) != coseCoordinateLength {
		return nil, fmt.Errorf("%w: coordinates are %d and %d bytes", errWebAuthnCOSEKey, len(x), len(y))
	}
	point := make([]byte, 1+2*coseCoordinateLength)
	point[0] = 4
	copy(point[1:], x)
	copy(point[1+coseCoordinateLength:], y)
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return nil, fmt.Errorf("%w: %w", errWebAuthnCOSEKey, err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}

type AttestationObject struct {
	Format   string
	AuthData *AuthenticatorData
}

// ParseAttestationObject accepts exactly the three-key `none` shape: any
// other format, a non-empty statement, or a fourth key is a refusal
// (ADR-016 decision 6).
func ParseAttestationObject(data []byte) (*AttestationObject, error) {
	top, err := cborParse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errWebAuthnAttestationShape, err)
	}
	if err := top.requireMap(); err != nil {
		return nil, fmt.Errorf("%w: %w", errWebAuthnAttestationShape, err)
	}
	if len(top.pairs) != 3 {
		return nil, fmt.Errorf("%w: %d entries, want 3", errWebAuthnAttestationShape, len(top.pairs))
	}
	format, ok := top.lookupText("fmt")
	if !ok || format.kind != cborText {
		return nil, fmt.Errorf("%w: no text fmt", errWebAuthnAttestationShape)
	}
	if format.s != "none" {
		return nil, fmt.Errorf("%w: %q", errWebAuthnAttestationFormat, format.s)
	}
	stmt, ok := top.lookupText("attStmt")
	if !ok || stmt.kind != cborMap {
		return nil, fmt.Errorf("%w: no attStmt map", errWebAuthnAttestationShape)
	}
	if len(stmt.pairs) != 0 {
		return nil, fmt.Errorf("%w: %d entries", errWebAuthnAttestationStmt, len(stmt.pairs))
	}
	raw, ok := top.lookupText("authData")
	if !ok || raw.kind != cborBytes {
		return nil, fmt.Errorf("%w: no authData byte string", errWebAuthnAttestationShape)
	}
	authData, err := ParseAuthenticatorData(raw.b)
	if err != nil {
		return nil, err
	}
	if !authData.HasAttestedCredentialData() {
		return nil, fmt.Errorf("%w: attested credential data flag is clear", errWebAuthnAttestedData)
	}
	return &AttestationObject{Format: format.s, AuthData: authData}, nil
}

type webauthnClientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin *bool  `json:"crossOrigin"`
}

// parseClientData allows unknown JSON members deliberately: the WebAuthn
// specification reserves the right to add them and browsers already ship one
// (`other_keys_can_be_added_here`), so DisallowUnknownFields here would be a
// refusal of real user agents rather than of attacker input.
func parseClientData(data []byte) (webauthnClientData, []byte, error) {
	var cd webauthnClientData
	if len(data) == 0 || len(data) > maxClientDataLength {
		return cd, nil, fmt.Errorf("%w: %d bytes", errWebAuthnClientData, len(data))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cd); err != nil {
		return cd, nil, fmt.Errorf("%w: %w", errWebAuthnClientData, err)
	}
	if dec.More() {
		return cd, nil, fmt.Errorf("%w: trailing content", errWebAuthnClientData)
	}
	challenge, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil {
		return cd, nil, fmt.Errorf("%w: challenge: %w", errWebAuthnClientData, err)
	}
	if len(challenge) != challengeLength {
		return cd, nil, fmt.Errorf("%w: challenge is %d bytes, want %d", errWebAuthnClientData, len(challenge), challengeLength)
	}
	return cd, challenge, nil
}

type WebAuthnCredential struct {
	ID               []byte
	PublicKeyX       []byte
	PublicKeyY       []byte
	SignCount        uint32
	CounterSupported bool
	UserHandle       []byte
}

type WebAuthnRegistrationInput struct {
	ClientDataJSON    []byte
	AttestationObject []byte
	Existing          []WebAuthnCredential
}

type WebAuthnRegistrationResult struct {
	CredentialID     []byte
	PublicKeyX       []byte
	PublicKeyY       []byte
	SignCount        uint32
	CounterSupported bool
	AAGUID           []byte
}

type WebAuthnAssertionInput struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
	UserHandle        []byte
	Credentials       []WebAuthnCredential
}

type WebAuthnAssertionResult struct {
	CredentialID    []byte
	SignCount       uint32
	UpdateSignCount bool
}

type WebAuthnVerifier struct {
	origin     string
	rpID       string
	rpIDHash   [32]byte
	challenges *WebAuthnChallengeStore
	limiter    *ceremonylimit.Limiter
}

func NewWebAuthnVerifier(origin, rpID string) (*WebAuthnVerifier, error) {
	if origin == "" {
		return nil, errors.New("webauthn: no origin")
	}
	if rpID == "" {
		return nil, errors.New("webauthn: no relying party id")
	}
	return &WebAuthnVerifier{
		origin:     origin,
		rpID:       rpID,
		rpIDHash:   sha256.Sum256([]byte(rpID)),
		challenges: newWebAuthnChallengeStore(),
		limiter:    ceremonylimit.New(),
	}, nil
}

func (v *WebAuthnVerifier) Origin() string { return v.origin }

func (v *WebAuthnVerifier) RPID() string { return v.rpID }

func (v *WebAuthnVerifier) IssueChallenge(ceremony WebAuthnCeremony) ([]byte, error) {
	return v.challenges.Issue(ceremony)
}

// CeremonyCompleted is the limiter's success signal for a registration, and
// the caller owns it: it says the ceremony completed end to end, including
// everything the verifier is deliberately blind to.
//
// CeremonyRefused is the other half — a ceremony this verifier accepted and
// the caller then refused, which is the only way a bootstrap-code guess is
// counted at all.
func (v *WebAuthnVerifier) CeremonyCompleted() { v.limiter.RecordSuccess() }

func (v *WebAuthnVerifier) CeremonyRefused() { v.limiter.RecordFailure() }

// VerifyRegistration is pure: it knows nothing about the bootstrap code that
// anchors registration (ADR-016 decision 2), which is checked by the caller
// afterwards.
//
// This is deliberate: a verified registration therefore does NOT clear the
// limiter, and only CeremonyCompleted does. Clearing here made a registration
// carrying no code at all — which costs an unauthenticated caller nothing but
// a key of its own — a failure to the route and a success to the limiter,
// wiping `failures` and `nextAllowed` for every other ceremony. A caller that
// forgets to confirm leaves the count standing, which is the direction this
// has to fail.
func (v *WebAuthnVerifier) VerifyRegistration(in WebAuthnRegistrationInput) (result *WebAuthnRegistrationResult, err error) {
	if retry, ok := v.limiter.Allow(); !ok {
		return nil, &WebAuthnRateLimitedError{RetryAfter: retry}
	}
	defer func() {
		if err != nil {
			v.limiter.RecordFailure()
		}
	}()

	if len(in.Existing) >= MaxRegisteredPasskeys {
		return nil, fmt.Errorf("%w: %d registered", ErrWebAuthnPasskeyLimit, len(in.Existing))
	}
	clientData, challenge, err := parseClientData(in.ClientDataJSON)
	if err != nil {
		return nil, err
	}
	if !v.challenges.consume(WebAuthnCeremonyRegister, challenge) {
		return nil, errWebAuthnChallenge
	}
	if err := v.checkClientData(clientData, WebAuthnCeremonyRegister); err != nil {
		return nil, err
	}
	att, err := ParseAttestationObject(in.AttestationObject)
	if err != nil {
		return nil, err
	}
	if err := v.checkAuthData(att.AuthData); err != nil {
		return nil, err
	}
	for _, c := range in.Existing {
		if bytes.Equal(c.ID, att.AuthData.CredentialID) {
			return nil, ErrWebAuthnDuplicateCred
		}
	}
	return &WebAuthnRegistrationResult{
		CredentialID: att.AuthData.CredentialID,
		PublicKeyX:   att.AuthData.PublicKeyX,
		PublicKeyY:   att.AuthData.PublicKeyY,
		SignCount:    att.AuthData.SignCount,
		// This is deliberate: a registration reporting counter 0 fixes the
		// per-credential policy as "no counters" for good, so a later
		// assertion of 0 is not read as a clone (decision 7, point 10).
		CounterSupported: att.AuthData.SignCount != 0,
		AAGUID:           att.AuthData.AAGUID,
	}, nil
}

// VerifyAssertion clears the limiter on its own, unlike VerifyRegistration.
// This is deliberate and is not an oversight of the asymmetry: an assertion
// carries no anchor for a caller to check afterwards, so a ceremony this
// function accepts is a ceremony that completed. The caller confirms it too,
// so the signal keeps working if that ever stops being true.
func (v *WebAuthnVerifier) VerifyAssertion(in WebAuthnAssertionInput) (result *WebAuthnAssertionResult, err error) {
	if retry, ok := v.limiter.Allow(); !ok {
		return nil, &WebAuthnRateLimitedError{RetryAfter: retry}
	}
	defer func() {
		if err != nil {
			v.limiter.RecordFailure()
			return
		}
		v.limiter.RecordSuccess()
	}()

	clientData, challenge, err := parseClientData(in.ClientDataJSON)
	if err != nil {
		return nil, err
	}
	if !v.challenges.consume(WebAuthnCeremonyAssert, challenge) {
		return nil, errWebAuthnChallenge
	}
	if err := v.checkClientData(clientData, WebAuthnCeremonyAssert); err != nil {
		return nil, err
	}
	cred := findWebAuthnCredential(in.Credentials, in.CredentialID)
	if cred == nil {
		return nil, ErrWebAuthnAssertionRejected
	}
	if len(in.UserHandle) > 0 &&
		subtle.ConstantTimeCompare(in.UserHandle, cred.UserHandle) != 1 {
		return nil, errWebAuthnUserHandle
	}
	authData, err := ParseAuthenticatorData(in.AuthenticatorData)
	if err != nil {
		return nil, err
	}
	if authData.HasAttestedCredentialData() {
		return nil, fmt.Errorf("%w: attested credential data on an assertion", errWebAuthnAuthDataShape)
	}
	if err := v.checkAuthData(authData); err != nil {
		return nil, err
	}
	if len(in.Signature) == 0 || len(in.Signature) > maxSignatureLength {
		return nil, ErrWebAuthnAssertionRejected
	}
	pub, err := webauthnPublicKey(cred.PublicKeyX, cred.PublicKeyY)
	if err != nil {
		return nil, err
	}
	clientDataHash := sha256.Sum256(in.ClientDataJSON)
	signed := make([]byte, 0, len(authData.Raw)+len(clientDataHash))
	signed = append(signed, authData.Raw...)
	signed = append(signed, clientDataHash[:]...)
	digest := sha256.Sum256(signed)
	if !ecdsa.VerifyASN1(pub, digest[:], in.Signature) {
		return nil, ErrWebAuthnAssertionRejected
	}
	if cred.CounterSupported && authData.SignCount <= cred.SignCount {
		return nil, &WebAuthnCounterError{
			CredentialID: cred.ID,
			Stored:       cred.SignCount,
			Received:     authData.SignCount,
		}
	}
	return &WebAuthnAssertionResult{
		CredentialID:    cred.ID,
		SignCount:       authData.SignCount,
		UpdateSignCount: cred.CounterSupported,
	}, nil
}

func (v *WebAuthnVerifier) checkClientData(cd webauthnClientData, ceremony WebAuthnCeremony) error {
	if cd.Type != ceremony.clientDataType() {
		return fmt.Errorf("%w: %q, want %q", errWebAuthnCeremonyType, cd.Type, ceremony.clientDataType())
	}
	// An origin is not a secret and a variable-time comparison of it leaks
	// nothing, so this is plain equality on purpose.
	if cd.Origin != v.origin {
		return fmt.Errorf("%w: %q, want %q", errWebAuthnOrigin, cd.Origin, v.origin)
	}
	if cd.CrossOrigin != nil && *cd.CrossOrigin {
		return errWebAuthnCrossOrigin
	}
	return nil
}

func (v *WebAuthnVerifier) checkAuthData(a *AuthenticatorData) error {
	if subtle.ConstantTimeCompare(a.RPIDHash, v.rpIDHash[:]) != 1 {
		return fmt.Errorf("%w: want SHA-256(%q)", errWebAuthnRPIDHash, v.rpID)
	}
	if !a.UserPresent() {
		return errWebAuthnUserPresent
	}
	if !a.UserVerified() {
		return errWebAuthnUserVerified
	}
	return nil
}

func findWebAuthnCredential(creds []WebAuthnCredential, id []byte) *WebAuthnCredential {
	if len(id) == 0 || len(id) > maxCredentialIDLength {
		return nil
	}
	var found *WebAuthnCredential
	for i := range creds {
		if subtle.ConstantTimeCompare(creds[i].ID, id) == 1 {
			found = &creds[i]
		}
	}
	return found
}

// ParseCredentialID decodes the unpadded base64url a browser sends as rawId
// and a Passkey record stores.
func ParseCredentialID(encoded string) ([]byte, error) {
	id, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("webauthn: credential id: %w", err)
	}
	if len(id) == 0 || len(id) > maxCredentialIDLength {
		return nil, fmt.Errorf("webauthn: credential id is %d bytes", len(id))
	}
	return id, nil
}
