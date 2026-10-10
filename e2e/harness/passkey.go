package harness

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"sort"
)

// Authenticator is a software WebAuthn authenticator holding one ES256
// credential. Relay's real verifier checks what it produces.
type Authenticator struct {
	credID    []byte
	key       *ecdsa.PrivateKey
	signCount uint32
}

// NewAuthenticator creates an authenticator with a fresh credential. The
// credential is unknown to relay until RegisterPasskey runs, which makes it
// usable as an unregistered authenticator too.
func NewAuthenticator() *Authenticator {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic("e2e harness: generating a P-256 key: " + err.Error())
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		panic("e2e harness: reading random bytes: " + err.Error())
	}
	return &Authenticator{credID: id, key: key, signCount: 1}
}

// CredentialID returns the id of the authenticator's credential.
func (a *Authenticator) CredentialID() []byte { return append([]byte(nil), a.credID...) }

// SetSignCount sets the counter value the credential has reached. The next
// assertion reports n+1. It does nothing for another credential's id.
func (a *Authenticator) SetSignCount(credentialID []byte, n uint32) {
	if bytes.Equal(credentialID, a.credID) {
		a.signCount = n
	}
}

var b64 = base64.RawURLEncoding

type challengeReply struct {
	Challenge   string   `json:"challenge"`
	RPID        string   `json:"rp_id"`
	Origin      string   `json:"origin"`
	Credentials []string `json:"credentials"`
}

func (i *Instance) loginOrigin() string {
	_, port, _ := net.SplitHostPort(i.Ready.Listeners["api"])
	return "http://localhost:" + port
}

func (i *Instance) loginChallenge(c *Client, ceremony string) challengeReply {
	i.t.Helper()
	r := c.Do("POST", "/relay/login/challenge", map[string]string{"ceremony": ceremony})
	if r.Status != 200 {
		i.t.Fatalf("POST /relay/login/challenge (%s) answered %d: %s", ceremony, r.Status, tailString(r.Body, 500))
	}
	var rep challengeReply
	r.JSON(i.t, &rep)
	return rep
}

func clientDataJSON(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

// RegisterPasskey runs the registration ceremony with the bootstrap code and
// returns the new passkey's id and the verify response.
func (i *Instance) RegisterPasskey(a *Authenticator, bootstrapCode string) (passkeyID string, resp Response) {
	t := i.t
	t.Helper()
	c := i.Anonymous()
	ch := i.loginChallenge(c, "register")
	origin := i.loginOrigin()
	rpHash := sha256.Sum256([]byte("localhost"))

	// authData: rpIdHash | flags UP+UV+AT | signCount | aaguid | idLen | id | COSE key
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(0x01 | 0x04 | 0x40)
	_ = binary.Write(&ad, binary.BigEndian, a.signCount)
	ad.Write(make([]byte, 16))
	_ = binary.Write(&ad, binary.BigEndian, uint16(len(a.credID)))
	ad.Write(a.credID)
	ad.Write(a.coseKey())

	att := cborMap(
		cborPair{cborText("fmt"), cborText("none")},
		cborPair{cborText("attStmt"), cborMap()},
		cborPair{cborText("authData"), cborBytes(ad.Bytes())},
	)
	body := map[string]string{
		"ceremony":           "register",
		"code":               bootstrapCode,
		"client_data_json":   b64.EncodeToString(clientDataJSON("webauthn.create", ch.Challenge, origin)),
		"attestation_object": b64.EncodeToString(att),
	}
	resp = c.Do("POST", "/relay/login/verify", body)
	if resp.Status/100 == 2 {
		var out struct {
			CredentialID string `json:"credential_id"`
		}
		resp.JSON(t, &out)
		passkeyID = out.CredentialID
	}
	return passkeyID, resp
}

// SignIn runs the assertion ceremony with the authenticator's credential. On
// success the returned credential holds the session token relay minted.
func (i *Instance) SignIn(a *Authenticator) (Credential, Response) {
	t := i.t
	t.Helper()
	c := i.Anonymous()
	ch := i.loginChallenge(c, "assert")
	origin := i.loginOrigin()
	rpHash := sha256.Sum256([]byte("localhost"))

	a.signCount++
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(0x01 | 0x04) // UP+UV
	_ = binary.Write(&ad, binary.BigEndian, a.signCount)

	cd := clientDataJSON("webauthn.get", ch.Challenge, origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte(nil), ad.Bytes()...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("signing the assertion: %v", err)
	}
	resp := c.Do("POST", "/relay/login/verify", map[string]string{
		"ceremony":           "assert",
		"credential_id":      b64.EncodeToString(a.credID),
		"client_data_json":   b64.EncodeToString(cd),
		"authenticator_data": b64.EncodeToString(ad.Bytes()),
		"signature":          b64.EncodeToString(sig),
		"user_handle":        "",
	})
	var cred Credential
	if resp.Status == 200 {
		var out struct {
			Token   string   `json:"token"`
			Classes []string `json:"classes"`
		}
		resp.JSON(t, &out)
		cred = Credential{Name: "login", Token: out.Token, Classes: out.Classes}
	}
	return cred, resp
}

// coseKey is the credential public key as a canonical CTAP2 COSE_Key:
// {1:2, 3:-7, -1:1, -2:x, -3:y}.
func (a *Authenticator) coseKey() []byte {
	x := a.key.X.FillBytes(make([]byte, 32))
	y := a.key.Y.FillBytes(make([]byte, 32))
	return cborMap(
		cborPair{cborInt(1), cborInt(2)},
		cborPair{cborInt(3), cborInt(-7)},
		cborPair{cborInt(-1), cborInt(1)},
		cborPair{cborInt(-2), cborBytes(x)},
		cborPair{cborInt(-3), cborBytes(y)},
	)
}

// Minimal canonical CBOR (CTAP2): shortest integer forms, map keys sorted by
// encoded length and then bytewise.

type cborPair struct{ k, v []byte }

func cborHead(major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return []byte{m | byte(n)}
	case n <= 0xff:
		return []byte{m | 24, byte(n)}
	case n <= 0xffff:
		return []byte{m | 25, byte(n >> 8), byte(n)}
	case n <= 0xffffffff:
		b := []byte{m | 26, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(n))
		return b
	default:
		b := []byte{m | 27, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(b[1:], n)
		return b
	}
}

func cborInt(v int64) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }

func cborText(s string) []byte { return append(cborHead(3, uint64(len(s))), s...) }

func cborMap(pairs ...cborPair) []byte {
	sorted := append([]cborPair(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].k, sorted[j].k
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return bytes.Compare(a, b) < 0
	})
	out := cborHead(5, uint64(len(sorted)))
	for _, p := range sorted {
		out = append(out, p.k...)
		out = append(out, p.v...)
	}
	return out
}
