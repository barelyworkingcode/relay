package login

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/ceremonylimit"
)

func newTestVerifier(t *testing.T) *WebAuthnVerifier {
	t.Helper()
	v, err := NewWebAuthnVerifier(testOrigin, testRPID)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func mustRegister(t *testing.T, v *WebAuthnVerifier, a *softAuthenticator) *WebAuthnRegistrationResult {
	t.Helper()
	result, err := v.VerifyRegistration(newRegistration(t, v, a).input())
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	return result
}

func credentialOf(r *WebAuthnRegistrationResult, userHandle []byte) WebAuthnCredential {
	return WebAuthnCredential{
		ID:               r.CredentialID,
		PublicKeyX:       r.PublicKeyX,
		PublicKeyY:       r.PublicKeyY,
		SignCount:        r.SignCount,
		CounterSupported: r.CounterSupported,
		UserHandle:       userHandle,
	}
}

func wantErr(t *testing.T, err error, target error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want %v", what, target)
	}
	if !errors.Is(err, target) {
		t.Fatalf("%s: got %v, want %v", what, err, target)
	}
}

func TestWebAuthnRegistrationHappyPath(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	result, err := v.VerifyRegistration(newRegistration(t, v, a).input())
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	if !bytes.Equal(result.CredentialID, a.credID) {
		t.Errorf("credential id = %x, want %x", result.CredentialID, a.credID)
	}
	if !bytes.Equal(result.PublicKeyX, a.x()) || !bytes.Equal(result.PublicKeyY, a.y()) {
		t.Errorf("public key coordinates do not match the authenticator's")
	}
	if result.SignCount != 1 || !result.CounterSupported {
		t.Errorf("counter = %d supported=%v, want 1/true", result.SignCount, result.CounterSupported)
	}
	if len(result.AAGUID) != aaguidLength {
		t.Errorf("aaguid = %d bytes, want %d", len(result.AAGUID), aaguidLength)
	}
	if n := v.challenges.outstanding(); n != 0 {
		t.Errorf("outstanding challenges = %d, want 0", n)
	}
}

func TestWebAuthnAssertionHappyPath(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), []byte("owner"))

	c := newAssertion(t, v, a)
	c.UserHandle = []byte("owner")
	result, err := v.VerifyAssertion(c.input(t, cred))
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}
	if !bytes.Equal(result.CredentialID, a.credID) {
		t.Errorf("credential id = %x, want %x", result.CredentialID, a.credID)
	}
	if result.SignCount != 2 || !result.UpdateSignCount {
		t.Errorf("sign count = %d update=%v, want 2/true", result.SignCount, result.UpdateSignCount)
	}
	if n := v.challenges.outstanding(); n != 0 {
		t.Errorf("outstanding challenges = %d, want 0", n)
	}
}

// Check 1: challenge.

func TestWebAuthnChallengeUnknown(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	unknown := make([]byte, challengeLength)
	if _, err := rand.Read(unknown); err != nil {
		t.Fatal(err)
	}
	c.ClientData.Challenge = unknown
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnChallenge, "unknown challenge")
}

func TestWebAuthnChallengeUsedTwice(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	if _, err := v.VerifyAssertion(c.input(t, cred)); err != nil {
		t.Fatalf("first assertion: %v", err)
	}
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnChallenge, "replayed challenge")
}

func TestWebAuthnChallengeExpired(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	now := time.Now()
	v.challenges.now = func() time.Time { return now }
	c := newAssertion(t, v, a)
	now = now.Add(ChallengeTTL + time.Second)

	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnChallenge, "expired challenge")
}

func TestWebAuthnChallengeStillLiveJustInsideTTL(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	now := time.Now()
	v.challenges.now = func() time.Time { return now }
	c := newAssertion(t, v, a)
	now = now.Add(ChallengeTTL - time.Millisecond)

	if _, err := v.VerifyAssertion(c.input(t, cred)); err != nil {
		t.Fatalf("assertion just inside the TTL: %v", err)
	}
}

func TestWebAuthnChallengeFromTheOtherCeremony(t *testing.T) {
	t.Run("registration challenge presented to an assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)

		registration, err := v.IssueChallenge(WebAuthnCeremonyRegister)
		if err != nil {
			t.Fatal(err)
		}
		c := newAssertion(t, v, a)
		c.ClientData.Challenge = registration
		_, err = v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnChallenge, "registration challenge on an assertion")
	})

	t.Run("assertion challenge presented to a registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		assertion, err := v.IssueChallenge(WebAuthnCeremonyAssert)
		if err != nil {
			t.Fatal(err)
		}
		c := newRegistration(t, v, a)
		c.ClientData.Challenge = assertion
		_, err = v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnChallenge, "assertion challenge on a registration")
	})
}

func TestWebAuthnChallengeRefusalsAreIndistinguishable(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	unknown := make([]byte, challengeLength)
	if _, err := rand.Read(unknown); err != nil {
		t.Fatal(err)
	}
	wrongCeremony, err := v.IssueChallenge(WebAuthnCeremonyRegister)
	if err != nil {
		t.Fatal(err)
	}
	spent := newAssertion(t, v, a)
	if _, err := v.VerifyAssertion(spent.input(t, cred)); err != nil {
		t.Fatalf("priming assertion: %v", err)
	}

	messages := map[string]string{}
	for name, challenge := range map[string][]byte{
		"unknown":        unknown,
		"already used":   spent.ClientData.Challenge,
		"wrong ceremony": wrongCeremony,
	} {
		fresh := newTestVerifier(t)
		fresh.challenges = v.challenges
		c := newAssertion(t, fresh, a)
		c.ClientData.Challenge = challenge
		_, err := fresh.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnChallenge, name)
		messages[name] = err.Error()
	}
	var seen string
	for name, msg := range messages {
		if seen == "" {
			seen = msg
			continue
		}
		if msg != seen {
			t.Fatalf("%s refused with %q, another case with %q: the refusals must be identical", name, msg, seen)
		}
	}
}

func TestWebAuthnChallengeConsumedBeforeTheRestOfVerification(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	good := c.ClientData.Challenge
	c.ClientData.Origin = "http://localhost:9999"
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnOrigin, "wrong origin")

	replay := newAssertion(t, v, a)
	replay.ClientData.Challenge = good
	_, err = v.VerifyAssertion(replay.input(t, cred))
	wantErr(t, err, errWebAuthnChallenge, "challenge reused after a failed ceremony")
}

func TestWebAuthnChallengeTableIsBounded(t *testing.T) {
	store := newWebAuthnChallengeStore()
	now := time.Now()
	store.now = func() time.Time { return now }

	for i := 0; i < maxOutstandingChallenges; i++ {
		if _, err := store.Issue(WebAuthnCeremonyAssert); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	if _, err := store.Issue(WebAuthnCeremonyAssert); !errors.Is(err, ErrChallengeTableFull) {
		t.Fatalf("issue past the bound: got %v, want %v", err, ErrChallengeTableFull)
	}
	if n := store.outstanding(); n != maxOutstandingChallenges {
		t.Fatalf("outstanding = %d, want %d", n, maxOutstandingChallenges)
	}

	now = now.Add(ChallengeTTL + time.Second)
	if _, err := store.Issue(WebAuthnCeremonyAssert); err != nil {
		t.Fatalf("issue after the outstanding entries expired: %v", err)
	}
	if n := store.outstanding(); n != 1 {
		t.Fatalf("outstanding after eviction = %d, want 1", n)
	}
}

func TestWebAuthnChallengeStoreIsSingleUseUnderConcurrency(t *testing.T) {
	store := newWebAuthnChallengeStore()
	const workers = 16
	challenges := make([][]byte, 0, maxOutstandingChallenges/2)
	for i := 0; i < cap(challenges); i++ {
		c, err := store.Issue(WebAuthnCeremonyAssert)
		if err != nil {
			t.Fatal(err)
		}
		challenges = append(challenges, c)
	}

	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for _, c := range challenges {
				if store.consume(WebAuthnCeremonyAssert, c) {
					local++
				}
				if _, err := store.Issue(WebAuthnCeremonyRegister); err != nil && !errors.Is(err, ErrChallengeTableFull) {
					t.Errorf("issue: %v", err)
				}
			}
			mu.Lock()
			wins += int64(local)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if wins != int64(len(challenges)) {
		t.Fatalf("%d successful consumes, want exactly %d", wins, len(challenges))
	}
}

func TestWebAuthnChallengeStoreRefusesAnUnknownCeremony(t *testing.T) {
	store := newWebAuthnChallengeStore()
	if _, err := store.Issue(WebAuthnCeremony(0)); err == nil {
		t.Fatal("issued a challenge for the zero ceremony")
	}
	if _, err := store.Issue(WebAuthnCeremony(99)); err == nil {
		t.Fatal("issued a challenge for an unknown ceremony")
	}
}

// Check 2: clientDataJSON.type.

func TestWebAuthnClientDataTypeMustMatchTheCeremony(t *testing.T) {
	t.Run("assertion carrying webauthn.create", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.ClientData.Type = "webauthn.create"
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnCeremonyType, "webauthn.create on an assertion")
	})

	t.Run("registration carrying webauthn.get", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.ClientData.Type = "webauthn.get"
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnCeremonyType, "webauthn.get on a registration")
	})

	t.Run("empty and near-miss types", func(t *testing.T) {
		for _, typ := range []string{"", "webauthn.GET", "webauthn.get ", " webauthn.get", "webauthn"} {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			c := newAssertion(t, v, a)
			c.ClientData.Type = typ
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, errWebAuthnCeremonyType, fmt.Sprintf("type %q", typ))
		}
	})
}

// Check 3: origin.

func TestWebAuthnOriginMustMatchExactly(t *testing.T) {
	origins := []string{
		"http://localhost:8791",
		"https://localhost:8790",
		"http://localhost:8790/",
		"http://localhost:8790.evil.example",
		"http://evil.example/http://localhost:8790",
		"http://LOCALHOST:8790",
		"http://127.0.0.1:8790",
		"",
	}
	for _, origin := range origins {
		t.Run("assertion from "+origin, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			c := newAssertion(t, v, a)
			c.ClientData.Origin = origin
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, errWebAuthnOrigin, "origin "+origin)
		})
	}
	t.Run("registration from another origin", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.ClientData.Origin = "https://evil.example"
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnOrigin, "registration origin")
	})
}

// Check 4: crossOrigin.

func TestWebAuthnCrossOriginIsRefused(t *testing.T) {
	t.Run("assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.ClientData.CrossOrigin = boolPtr(true)
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnCrossOrigin, "crossOrigin true")
	})

	t.Run("registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.ClientData.CrossOrigin = boolPtr(true)
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnCrossOrigin, "crossOrigin true")
	})

	t.Run("absent crossOrigin is accepted", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.ClientData.CrossOrigin = nil
		if _, err := v.VerifyAssertion(c.input(t, cred)); err != nil {
			t.Fatalf("absent crossOrigin: %v", err)
		}
	})
}

// Check 5: RP ID hash.

func TestWebAuthnRPIDHashMustMatch(t *testing.T) {
	t.Run("assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.AuthData.RPIDHash = rpIDHash("evil.example")
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnRPIDHash, "rp id hash of another origin")
	})

	t.Run("registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.RPIDHash = rpIDHash("localhost.evil.example")
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnRPIDHash, "rp id hash of a suffix domain")
	})

	t.Run("one flipped bit", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		hash := append([]byte{}, rpIDHash(testRPID)...)
		hash[31] ^= 0x01
		c.AuthData.RPIDHash = hash
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnRPIDHash, "flipped bit")
	})
}

// Check 6: user-present flag.

func TestWebAuthnUserPresenceRequired(t *testing.T) {
	t.Run("assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.AuthData.Flags &^= flagUserPresent
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnUserPresent, "UP clear")
	})

	t.Run("registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Flags &^= flagUserPresent
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnUserPresent, "UP clear")
	})
}

// Check 7: user-verified flag, enforced server-side.

func TestWebAuthnUserVerificationRequired(t *testing.T) {
	t.Run("assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.AuthData.Flags &^= flagUserVerified
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnUserVerified, "UV clear on an otherwise valid assertion")
	})

	t.Run("registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Flags &^= flagUserVerified
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnUserVerified, "UV clear on registration")
	})
}

// Check 8: signature.

func TestWebAuthnSignatureOverTheWrongMessage(t *testing.T) {
	cases := map[string]func(c *assertionCeremony, authData, clientData []byte){
		"clientDataJSON omitted": func(c *assertionCeremony, authData, _ []byte) {
			c.SignOver = authData
		},
		"clientDataJSON not hashed": func(c *assertionCeremony, authData, clientData []byte) {
			c.SignOver = append(append([]byte{}, authData...), clientData...)
		},
		"halves swapped": func(c *assertionCeremony, authData, clientData []byte) {
			hash := sha256Of(clientData)
			c.SignOver = append(append([]byte{}, hash...), authData...)
		},
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			c := newAssertion(t, v, a)
			mangle(c, buildAuthData(c.AuthData), buildClientDataJSON(c.ClientData))
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, ErrWebAuthnAssertionRejected, name)
		})
	}
}

func TestWebAuthnSignatureFromAnotherKey(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	other := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.SignWith = other.key
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, ErrWebAuthnAssertionRejected, "signature from another key")
}

func TestWebAuthnSignatureMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"garbage":          []byte("not a signature"),
		"truncated DER":    {0x30, 0x06, 0x02, 0x01, 0x01},
		"oversized":        bytes.Repeat([]byte{0x30}, maxSignatureLength+1),
		"zero-length ints": {0x30, 0x04, 0x02, 0x00, 0x02, 0x00},
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			c := newAssertion(t, v, a)
			c.RawSignature = sig
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, ErrWebAuthnAssertionRejected, name)
		})
	}
}

func TestWebAuthnSignatureWithTrailingDERBytes(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	authData := buildAuthData(c.AuthData)
	clientData := buildClientDataJSON(c.ClientData)
	c.RawSignature = append(c.sign(t, authData, clientData), 0x00)
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, ErrWebAuthnAssertionRejected, "DER with a trailing byte")
}

// Check 9: credential-to-user binding.

func TestWebAuthnUnknownCredentialID(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.CredentialID = bytes.Repeat([]byte{0xAB}, 32)
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, ErrWebAuthnAssertionRejected, "unknown credential id")
}

func TestWebAuthnNoRegisteredCredentials(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	c := newAssertion(t, v, a)
	_, err := v.VerifyAssertion(c.input(t))
	wantErr(t, err, ErrWebAuthnAssertionRejected, "assertion against an empty credential set")
}

func TestWebAuthnUnknownCredentialIsRefusedIdenticallyToABadSignature(t *testing.T) {
	unknown := func() error {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.CredentialID = bytes.Repeat([]byte{0x01}, 32)
		_, err := v.VerifyAssertion(c.input(t, cred))
		return err
	}()
	badSignature := func() error {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		other := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.SignWith = other.key
		_, err := v.VerifyAssertion(c.input(t, cred))
		return err
	}()
	if unknown == nil || badSignature == nil {
		t.Fatalf("both cases must be refused: unknown=%v signature=%v", unknown, badSignature)
	}
	if unknown.Error() != badSignature.Error() {
		t.Fatalf("unknown credential refused with %q, bad signature with %q: these must be indistinguishable",
			unknown, badSignature)
	}
}

func TestWebAuthnUserHandleMustMatch(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), []byte("owner"))

	c := newAssertion(t, v, a)
	c.UserHandle = []byte("someone-else")
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnUserHandle, "user handle of another user")
}

func TestWebAuthnUserHandleAgainstACredentialThatHasNone(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.UserHandle = []byte("owner")
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnUserHandle, "user handle where the credential stores none")
}

func TestWebAuthnAbsentUserHandleIsBoundByCredentialID(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), []byte("owner"))

	c := newAssertion(t, v, a)
	c.UserHandle = nil
	if _, err := v.VerifyAssertion(c.input(t, cred)); err != nil {
		t.Fatalf("absent user handle: %v", err)
	}
}

func TestWebAuthnCredentialIDOfAnotherRegisteredKeyDoesNotVerify(t *testing.T) {
	v := newTestVerifier(t)
	first := newSoftAuthenticator(t)
	second := newSoftAuthenticator(t)
	credOne := credentialOf(mustRegister(t, v, first), nil)
	credTwo := credentialOf(mustRegister(t, v, second), nil)

	c := newAssertion(t, v, first)
	c.CredentialID = second.credID
	_, err := v.VerifyAssertion(c.input(t, credOne, credTwo))
	wantErr(t, err, ErrWebAuthnAssertionRejected, "signature by the wrong registered key")
}

// Check 10: signature counter.

func TestWebAuthnCounterMustIncrease(t *testing.T) {
	for name, received := range map[string]uint32{"equal": 7, "lower": 3, "zero": 0} {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			cred.SignCount = 7
			cred.CounterSupported = true

			c := newAssertion(t, v, a)
			c.AuthData.SignCount = received
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, ErrWebAuthnCounter, name+" counter")

			var counterErr *WebAuthnCounterError
			if !errors.As(err, &counterErr) {
				t.Fatalf("error %v does not carry the credential for the audit record", err)
			}
			if !bytes.Equal(counterErr.CredentialID, a.credID) {
				t.Errorf("audit names credential %x, want %x", counterErr.CredentialID, a.credID)
			}
			if counterErr.Stored != 7 || counterErr.Received != received {
				t.Errorf("audit says stored=%d received=%d, want 7/%d", counterErr.Stored, counterErr.Received, received)
			}
		})
	}
}

func TestWebAuthnCounterIncreaseIsAccepted(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.AuthData.SignCount = cred.SignCount + 41
	result, err := v.VerifyAssertion(c.input(t, cred))
	if err != nil {
		t.Fatalf("increasing counter: %v", err)
	}
	if result.SignCount != cred.SignCount+41 || !result.UpdateSignCount {
		t.Fatalf("result = %d update=%v, want %d/true", result.SignCount, result.UpdateSignCount, cred.SignCount+41)
	}
}

// A replayed stale assertion must cost the owner one retry and nothing more
// (ADR-016 decision 7, point 10).
func TestWebAuthnCounterRegressionDoesNotDisableTheCredential(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)
	cred.SignCount = 9

	stale := newAssertion(t, v, a)
	stale.AuthData.SignCount = 4
	if _, err := v.VerifyAssertion(stale.input(t, cred)); !errors.Is(err, ErrWebAuthnCounter) {
		t.Fatalf("stale assertion: got %v, want %v", err, ErrWebAuthnCounter)
	}

	fresh := newAssertion(t, v, a)
	fresh.AuthData.SignCount = 10
	if _, err := v.VerifyAssertion(fresh.input(t, cred)); err != nil {
		t.Fatalf("the owner's next assertion was refused after a stale replay: %v", err)
	}
}

func TestWebAuthnCounterlessCredentialIsNotCheckedAgainstZero(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	c := newRegistration(t, v, a)
	c.AuthData.SignCount = 0
	result, err := v.VerifyRegistration(c.input())
	if err != nil {
		t.Fatalf("registration with counter 0: %v", err)
	}
	if result.CounterSupported {
		t.Fatal("a registration reporting counter 0 must record the credential as counterless")
	}

	cred := credentialOf(result, nil)
	for i := 0; i < 3; i++ {
		assertion := newAssertion(t, v, a)
		assertion.AuthData.SignCount = 0
		got, err := v.VerifyAssertion(assertion.input(t, cred))
		if err != nil {
			t.Fatalf("assertion %d from a counterless authenticator: %v", i, err)
		}
		if got.UpdateSignCount {
			t.Fatal("a counterless credential must not have its stored counter rewritten")
		}
	}
}

// Check 11: authData shape.

func TestWebAuthnAssertionAuthDataShape(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)
	base := buildAuthData(authDataOpts{
		RPIDHash:  rpIDHash(testRPID),
		Flags:     flagUserPresent | flagUserVerified,
		SignCount: 5,
	})
	if len(base) != authDataBaseLength {
		t.Fatalf("test instrument produced %d bytes, want %d", len(base), authDataBaseLength)
	}

	cases := map[string][]byte{
		"empty":               {},
		"truncated":           base[:len(base)-1],
		"one trailing byte":   append(append([]byte{}, base...), 0x00),
		"many trailing bytes": append(append([]byte{}, base...), bytes.Repeat([]byte{0xFF}, 64)...),
	}
	for name, authData := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAuthenticatorData(authData); !errors.Is(err, errWebAuthnAuthDataShape) {
				t.Fatalf("ParseAuthenticatorData: got %v, want %v", err, errWebAuthnAuthDataShape)
			}
			fresh := newTestVerifier(t)
			fresh.challenges = v.challenges
			c := newAssertion(t, fresh, a)
			c.AuthData.Trailing = nil
			in := c.input(t, cred)
			in.AuthenticatorData = authData
			_, err := fresh.VerifyAssertion(in)
			wantErr(t, err, errWebAuthnAuthDataShape, name)
		})
	}
}

func TestWebAuthnAssertionRefusesAttestedCredentialData(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.AuthData.Flags |= flagAttestedCredentialData
	c.AuthData.Attested = true
	c.AuthData.AAGUID = a.aaguid
	c.AuthData.CredID = a.credID
	c.AuthData.COSEKey = a.coseKey()
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnAuthDataShape, "AT flag set on an assertion")
}

func TestWebAuthnExtensionDataIsRefused(t *testing.T) {
	t.Run("assertion", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		cred := credentialOf(mustRegister(t, v, a), nil)
		c := newAssertion(t, v, a)
		c.AuthData.Flags |= flagExtensionData
		c.AuthData.Trailing = cborEncMap(cborEntry(cborEncText("credProtect"), cborEncUint(2)))
		_, err := v.VerifyAssertion(c.input(t, cred))
		wantErr(t, err, errWebAuthnExtensions, "ED flag on an assertion")
	})

	t.Run("registration", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Flags |= flagExtensionData
		c.AuthData.Trailing = cborEncMap(cborEntry(cborEncText("credProtect"), cborEncUint(2)))
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnExtensions, "ED flag on a registration")
	})
}

func TestWebAuthnRegistrationAuthDataShape(t *testing.T) {
	t.Run("trailing bytes after the public key", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Trailing = []byte{0x00}
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errCBORTrailing, "trailing bytes")
	})

	t.Run("credential id length of zero", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.CredID = nil
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnAttestedData, "zero-length credential id")
	})

	t.Run("credential id length past the end", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.CredIDLen = 1000
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnAttestedData, "credential id length past the end")
	})

	t.Run("credential id longer than the specification allows", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.CredID = bytes.Repeat([]byte{0x01}, maxCredentialIDLength+1)
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnAttestedData, "1024-byte credential id")
	})

	t.Run("attested credential data flag clear", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Flags &^= flagAttestedCredentialData
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnAuthDataShape, "AT flag clear on a registration")
	})

	t.Run("header only", func(t *testing.T) {
		v := newTestVerifier(t)
		a := newSoftAuthenticator(t)
		c := newRegistration(t, v, a)
		c.AuthData.Attested = false
		_, err := v.VerifyRegistration(c.input())
		wantErr(t, err, errWebAuthnAttestedData, "AT flag set with no attested credential data")
	})
}

// Check 12: ceremony rate and the passkey cap.

func TestWebAuthnCeremonyRateLimit(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	now := time.Now()
	v.limiter.SetClock(func() time.Time { return now })

	fail := func() error {
		c := newAssertion(t, v, a)
		c.ClientData.Origin = "http://evil.example"
		_, err := v.VerifyAssertion(c.input(t, cred))
		return err
	}
	for i := 0; i < ceremonylimit.FailureGrace; i++ {
		if err := fail(); !errors.Is(err, errWebAuthnOrigin) {
			t.Fatalf("failure %d: got %v, want %v", i, err, errWebAuthnOrigin)
		}
	}
	if err := fail(); !errors.Is(err, errWebAuthnOrigin) {
		t.Fatalf("the failure that trips the limiter: got %v, want %v", err, errWebAuthnOrigin)
	}

	good := newAssertion(t, v, a)
	_, err := v.VerifyAssertion(good.input(t, cred))
	wantErr(t, err, ErrWebAuthnRateLimited, "a ceremony inside the penalty window")
	var limited *WebAuthnRateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("error %v does not name a retry delay", err)
	}

	now = now.Add(limited.RetryAfter)
	after := newAssertion(t, v, a)
	if _, err := v.VerifyAssertion(after.input(t, cred)); err != nil {
		t.Fatalf("assertion after the penalty window: %v", err)
	}

	for i := 0; i < ceremonylimit.FailureGrace; i++ {
		if err := fail(); !errors.Is(err, errWebAuthnOrigin) {
			t.Fatalf("a success must reset the failure count, failure %d gave %v", i, err)
		}
	}
}

func TestWebAuthnRegistrationRateLimit(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	now := time.Now()
	v.limiter.SetClock(func() time.Time { return now })

	for i := 0; i <= ceremonylimit.FailureGrace; i++ {
		c := newRegistration(t, v, a)
		c.Format = "packed"
		if _, err := v.VerifyRegistration(c.input()); !errors.Is(err, errWebAuthnAttestationFormat) {
			t.Fatalf("failure %d: got %v", i, err)
		}
	}
	c := newRegistration(t, v, a)
	_, err := v.VerifyRegistration(c.input())
	wantErr(t, err, ErrWebAuthnRateLimited, "registration inside the penalty window")
}

func TestWebAuthnPasskeyCap(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	existing := make([]WebAuthnCredential, 0, MaxRegisteredPasskeys)
	for i := 0; i < MaxRegisteredPasskeys; i++ {
		existing = append(existing, WebAuthnCredential{ID: bytes.Repeat([]byte{byte(i)}, 32)})
	}
	c := newRegistration(t, v, a)
	_, err := v.VerifyRegistration(c.input(existing...))
	wantErr(t, err, ErrWebAuthnPasskeyLimit, "a sixth passkey")

	fresh := newTestVerifier(t)
	room := newRegistration(t, fresh, a)
	if _, err := fresh.VerifyRegistration(room.input(existing[:MaxRegisteredPasskeys-1]...)); err != nil {
		t.Fatalf("a fifth passkey must be accepted: %v", err)
	}
}

func TestWebAuthnDuplicateCredentialIsRefused(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newRegistration(t, v, a)
	_, err := v.VerifyRegistration(c.input(cred))
	wantErr(t, err, ErrWebAuthnDuplicateCred, "re-registering a credential id")
}

// Attestation policy.

func TestWebAuthnAttestationFormatMustBeNone(t *testing.T) {
	for _, format := range []string{"packed", "tpm", "android-key", "android-safetynet", "apple", "fido-u2f", "", "None"} {
		t.Run("fmt "+format, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			c := newRegistration(t, v, a)
			c.Format = format
			_, err := v.VerifyRegistration(c.input())
			wantErr(t, err, errWebAuthnAttestationFormat, "fmt "+format)
		})
	}
}

func TestWebAuthnAttestationStatementMustBeEmpty(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	c := newRegistration(t, v, a)
	c.AttStmt = cborEncMap(cborEntry(cborEncText("alg"), cborEncNegInt(-7)))
	_, err := v.VerifyRegistration(c.input())
	wantErr(t, err, errWebAuthnAttestationStmt, "non-empty attStmt")
}

func TestWebAuthnAttestationObjectShape(t *testing.T) {
	a := newSoftAuthenticator(t)
	authData := cborEncBytes(buildAuthData(authDataOpts{
		RPIDHash: rpIDHash(testRPID),
		Flags:    flagUserPresent | flagUserVerified | flagAttestedCredentialData,
		Attested: true,
		AAGUID:   a.aaguid,
		CredID:   a.credID,
		COSEKey:  a.coseKey(),
	}))
	fmtEntry := cborEntry(cborEncText("fmt"), cborEncText("none"))
	stmtEntry := cborEntry(cborEncText("attStmt"), cborEncMap())
	dataEntry := cborEntry(cborEncText("authData"), authData)

	cases := map[string][]byte{
		"two entries":         cborEncMap(fmtEntry, stmtEntry),
		"a fourth entry":      cborEncMap(fmtEntry, stmtEntry, dataEntry, cborEntry(cborEncText("epAtt"), cborEncUint(1))),
		"fmt is an integer":   cborEncMap(cborEntry(cborEncText("fmt"), cborEncUint(0)), stmtEntry, dataEntry),
		"attStmt is bytes":    cborEncMap(fmtEntry, cborEntry(cborEncText("attStmt"), cborEncBytes(nil)), dataEntry),
		"authData is text":    cborEncMap(fmtEntry, stmtEntry, cborEntry(cborEncText("authData"), cborEncText("nope"))),
		"top level is a text": cborEncText("none"),
		"top level is bytes":  cborEncBytes([]byte{1, 2, 3}),
	}
	for name, object := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAttestationObject(object); !errors.Is(err, errWebAuthnAttestationShape) {
				t.Fatalf("got %v, want %v", err, errWebAuthnAttestationShape)
			}
		})
	}
}

func TestWebAuthnRegistrationMalformedCBOR(t *testing.T) {
	a := newSoftAuthenticator(t)
	base := func() []byte {
		v := newTestVerifier(t)
		return newRegistration(t, v, a).attestationObject()
	}()

	cases := map[string]struct {
		object []byte
		want   error
	}{
		"truncated":             {object: base[:len(base)-4], want: errCBORTruncated},
		"header only":           {object: base[:1], want: errCBORTruncated},
		"trailing bytes":        {object: append(append([]byte{}, base...), 0x00), want: errCBORTrailing},
		"indefinite-length map": {object: append([]byte{0xBF}, append(base[1:], 0xFF)...), want: errCBORIndefinite},
		"empty":                 {object: []byte{}, want: errCBORTruncated},
		"duplicate keys": {object: cborEncMap(
			cborEntry(cborEncText("fmt"), cborEncText("none")),
			cborEntry(cborEncText("fmt"), cborEncText("packed")),
			cborEntry(cborEncText("attStmt"), cborEncMap()),
		), want: errCBORDuplicateKey},
		"keys out of canonical order": {object: cborEncMap(
			cborEntry(cborEncText("attStmt"), cborEncMap()),
			cborEntry(cborEncText("fmt"), cborEncText("none")),
			cborEntry(cborEncText("authData"), cborEncBytes(nil)),
		), want: errCBORNotCanonical},
		"nesting too deep": {object: cborEncMap(
			cborEntry(cborEncText("attStmt"), cborEncMap(
				cborEntry(cborEncText("x"), cborEncMap(
					cborEntry(cborEncText("y"), cborEncUint(1)))))),
		), want: errCBORDepth},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			c := newRegistration(t, v, a)
			c.RawObject = tc.object
			_, err := v.VerifyRegistration(c.input())
			wantErr(t, err, tc.want, name)
		})
	}
}

// COSE key policy.

func TestWebAuthnCOSEAlgorithmMustBeES256(t *testing.T) {
	for name, alg := range map[string][]byte{
		"RS256 (-257)":   cborEncNegInt(-257),
		"EdDSA (-8)":     cborEncNegInt(-8),
		"ES384 (-35)":    cborEncNegInt(-35),
		"a positive alg": cborEncUint(7),
	} {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			c := newRegistration(t, v, a)
			c.AuthData.COSEKey = coseKeyWith(coseKeyOpts{
				kty: cborEncUint(coseKeyTypeEC2),
				alg: alg,
				crv: cborEncUint(coseCurveP256),
				x:   cborEncBytes(a.x()),
				y:   cborEncBytes(a.y()),
			})
			_, err := v.VerifyRegistration(c.input())
			wantErr(t, err, errWebAuthnAlgorithm, name)
		})
	}
}

func TestWebAuthnCOSEKeyPolicy(t *testing.T) {
	a := newSoftAuthenticator(t)
	cases := map[string][]byte{
		"kty is not EC2": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(3), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(1),
			x: cborEncBytes(a.x()), y: cborEncBytes(a.y()),
		}),
		"crv is not P-256": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(2), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(2),
			x: cborEncBytes(a.x()), y: cborEncBytes(a.y()),
		}),
		"x is short": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(2), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(1),
			x: cborEncBytes(a.x()[:31]), y: cborEncBytes(a.y()),
		}),
		"y is text": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(2), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(1),
			x: cborEncBytes(a.x()), y: cborEncText("not a coordinate"),
		}),
		"a point that is not on the curve": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(2), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(1),
			x: cborEncBytes(bytes.Repeat([]byte{0x02}, 32)), y: cborEncBytes(bytes.Repeat([]byte{0x03}, 32)),
		}),
		"the point at infinity": coseKeyWith(coseKeyOpts{
			kty: cborEncUint(2), alg: cborEncNegInt(coseAlgES256), crv: cborEncUint(1),
			x: cborEncBytes(make([]byte, 32)), y: cborEncBytes(make([]byte, 32)),
		}),
		"four labels": cborEncMap(
			cborEntry(cborEncUint(1), cborEncUint(2)),
			cborEntry(cborEncUint(3), cborEncNegInt(coseAlgES256)),
			cborEntry(cborEncNegInt(-1), cborEncUint(1)),
			cborEntry(cborEncNegInt(-2), cborEncBytes(a.x())),
		),
		"an extra label": cborEncMap(
			cborEntry(cborEncUint(1), cborEncUint(2)),
			cborEntry(cborEncUint(3), cborEncNegInt(coseAlgES256)),
			cborEntry(cborEncUint(4), cborEncUint(1)),
			cborEntry(cborEncNegInt(-1), cborEncUint(1)),
			cborEntry(cborEncNegInt(-2), cborEncBytes(a.x())),
			cborEntry(cborEncNegInt(-3), cborEncBytes(a.y())),
		),
		"not a map": cborEncBytes(a.x()),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			c := newRegistration(t, v, a)
			c.AuthData.COSEKey = key
			_, err := v.VerifyRegistration(c.input())
			wantErr(t, err, errWebAuthnCOSEKey, name)
		})
	}
}

func TestWebAuthnAssertionRefusesACredentialWithAnInvalidStoredKey(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)
	cred.PublicKeyY = bytes.Repeat([]byte{0x07}, coseCoordinateLength)

	c := newAssertion(t, v, a)
	_, err := v.VerifyAssertion(c.input(t, cred))
	wantErr(t, err, errWebAuthnCOSEKey, "a stored point that is not on the curve")
}

// clientDataJSON parsing.

func TestWebAuthnClientDataMalformed(t *testing.T) {
	valid := func(t *testing.T, v *WebAuthnVerifier) []byte {
		challenge, err := v.IssueChallenge(WebAuthnCeremonyAssert)
		if err != nil {
			t.Fatal(err)
		}
		return challenge
	}
	cases := map[string]func(t *testing.T, v *WebAuthnVerifier) []byte{
		"not JSON": func(*testing.T, *WebAuthnVerifier) []byte {
			return []byte("not json at all")
		},
		"empty": func(*testing.T, *WebAuthnVerifier) []byte { return []byte{} },
		"a JSON array": func(*testing.T, *WebAuthnVerifier) []byte {
			return []byte(`["webauthn.get"]`)
		},
		"two documents": func(t *testing.T, v *WebAuthnVerifier) []byte {
			one := buildClientDataJSON(clientDataOpts{Type: "webauthn.get", Origin: testOrigin, Challenge: valid(t, v)})
			return append(one, one...)
		},
		"challenge is not base64url": func(t *testing.T, v *WebAuthnVerifier) []byte {
			return []byte(`{"type":"webauthn.get","challenge":"!!!!","origin":"` + testOrigin + `"}`)
		},
		"challenge is padded base64": func(t *testing.T, v *WebAuthnVerifier) []byte {
			padded := base64.URLEncoding.EncodeToString(make([]byte, 30))
			return []byte(`{"type":"webauthn.get","challenge":"` + padded + `","origin":"` + testOrigin + `"}`)
		},
		"challenge is the wrong length": func(t *testing.T, v *WebAuthnVerifier) []byte {
			short := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
			return []byte(`{"type":"webauthn.get","challenge":"` + short + `","origin":"` + testOrigin + `"}`)
		},
		"oversized": func(t *testing.T, v *WebAuthnVerifier) []byte {
			return buildClientDataJSON(clientDataOpts{
				Type: "webauthn.get", Origin: testOrigin, Challenge: valid(t, v),
				ExtraMember: `"pad":"` + strings.Repeat("A", maxClientDataLength) + `"`,
			})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifier(t)
			a := newSoftAuthenticator(t)
			cred := credentialOf(mustRegister(t, v, a), nil)
			c := newAssertion(t, v, a)
			c.ClientData.Raw = build(t, v)
			_, err := v.VerifyAssertion(c.input(t, cred))
			wantErr(t, err, errWebAuthnClientData, name)
		})
	}
}

// A real user agent may add members relay has never heard of; the
// specification says so, and refusing them would refuse the browser.
func TestWebAuthnClientDataUnknownMembersAreAccepted(t *testing.T) {
	v := newTestVerifier(t)
	a := newSoftAuthenticator(t)
	cred := credentialOf(mustRegister(t, v, a), nil)

	c := newAssertion(t, v, a)
	c.ClientData.ExtraMember = `"other_keys_can_be_added_here":"do not compare clientDataJSON against a template. See https://goo.gl/yabPex"`
	if _, err := v.VerifyAssertion(c.input(t, cred)); err != nil {
		t.Fatalf("clientDataJSON with an unknown member: %v", err)
	}
}

func TestParseCredentialID(t *testing.T) {
	raw := bytes.Repeat([]byte{0x5A}, 32)
	got, err := ParseCredentialID(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("decoded %x, want %x", got, raw)
	}
	for name, encoded := range map[string]string{
		"empty":             "",
		"padded":            base64.URLEncoding.EncodeToString(raw),
		"standard alphabet": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xFF}, 32)),
		"longer than 1023":  base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, maxCredentialIDLength+1)),
		"not base64 at all": "**",
	} {
		if _, err := ParseCredentialID(encoded); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The CBOR reader itself.

func TestCBORReaderRefuses(t *testing.T) {
	deep := cborEncMap(cborEntry(cborEncUint(1),
		cborEncMap(cborEntry(cborEncUint(1),
			cborEncMap(cborEntry(cborEncUint(1), cborEncUint(1)))))))

	cases := map[string]struct {
		in   []byte
		want error
	}{
		"truncated head":           {in: []byte{0x58}, want: errCBORTruncated},
		"truncated byte string":    {in: []byte{0x43, 0x01}, want: errCBORTruncated},
		"trailing bytes":           {in: []byte{0x01, 0x01}, want: errCBORTrailing},
		"array":                    {in: []byte{0x81, 0x01}, want: errCBORMajorType},
		"tag":                      {in: []byte{0xC1, 0x01}, want: errCBORMajorType},
		"float":                    {in: []byte{0xF9, 0x00, 0x00}, want: errCBORMajorType},
		"true":                     {in: []byte{0xF5}, want: errCBORMajorType},
		"null":                     {in: []byte{0xF6}, want: errCBORMajorType},
		"indefinite-length map":    {in: []byte{0xBF, 0xFF}, want: errCBORIndefinite},
		"indefinite-length bytes":  {in: []byte{0x5F, 0xFF}, want: errCBORIndefinite},
		"reserved additional info": {in: []byte{0x1C}, want: errCBORSyntax},
		"non-minimal one-byte int": {in: []byte{0x18, 0x01}, want: errCBORNotCanonical},
		"non-minimal two-byte int": {in: []byte{0x19, 0x00, 0x01}, want: errCBORNotCanonical},
		"nesting too deep":         {in: deep, want: errCBORDepth},
		"duplicate map key": {in: cborEncMap(
			cborEntry(cborEncUint(1), cborEncUint(1)),
			cborEntry(cborEncUint(1), cborEncUint(2)),
		), want: errCBORDuplicateKey},
		"map keys out of order": {in: cborEncMap(
			cborEntry(cborEncUint(2), cborEncUint(1)),
			cborEntry(cborEncUint(1), cborEncUint(2)),
		), want: errCBORNotCanonical},
		"text keys out of order": {in: cborEncMap(
			cborEntry(cborEncText("authData"), cborEncUint(1)),
			cborEntry(cborEncText("fmt"), cborEncUint(2)),
		), want: errCBORNotCanonical},
		"a byte string as a map key": {in: cborEncMap(
			cborEntry(cborEncBytes([]byte{1}), cborEncUint(1)),
		), want: errCBORKeyType},
		"too many map entries": {in: func() []byte {
			entries := make([][]byte, 0, cborMaxCollection+1)
			for i := 0; i <= cborMaxCollection; i++ {
				entries = append(entries, cborEntry(cborEncUint(uint64(i)), cborEncUint(0)))
			}
			return cborEncMap(entries...)
		}(), want: errCBORTooManyPairs},
		"input too large": {in: make([]byte, cborMaxInput+1), want: errCBORTooLarge},
		"invalid UTF-8":   {in: []byte{0x62, 0xFF, 0xFE}, want: errCBORNotUTF8},
		"empty input":     {in: nil, want: errCBORTruncated},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := cborParse(tc.in)
			wantErr(t, err, tc.want, name)
		})
	}
}

func TestCBORReaderAcceptsTheShapesTheVerifierNeeds(t *testing.T) {
	item, err := cborParse(cborEncMap(
		cborEntry(cborEncText("fmt"), cborEncText("none")),
		cborEntry(cborEncText("attStmt"), cborEncMap()),
		cborEntry(cborEncText("authData"), cborEncBytes([]byte{1, 2, 3})),
	))
	if err != nil {
		t.Fatalf("attestation shape: %v", err)
	}
	if err := item.requireMap(); err != nil {
		t.Fatal(err)
	}
	format, ok := item.lookupText("fmt")
	if !ok || format.s != "none" {
		t.Fatalf("fmt = %v, %v", format, ok)
	}
	data, ok := item.lookupText("authData")
	if !ok || !bytes.Equal(data.b, []byte{1, 2, 3}) {
		t.Fatalf("authData = %v, %v", data, ok)
	}

	key, err := cborParse(cborEncMap(
		cborEntry(cborEncUint(1), cborEncUint(2)),
		cborEntry(cborEncNegInt(-1), cborEncUint(1)),
	))
	if err != nil {
		t.Fatalf("cose shape: %v", err)
	}
	kty, ok := key.lookupInt(1)
	if !ok || kty.n != 2 {
		t.Fatalf("kty = %v, %v", kty, ok)
	}
	crv, ok := key.lookupInt(-1)
	if !ok || crv.n != 1 {
		t.Fatalf("crv = %v, %v", crv, ok)
	}
	if _, ok := key.lookupInt(-2); ok {
		t.Fatal("lookupInt found a label that is not there")
	}
}

func TestNewWebAuthnVerifierRefusesAnEmptyIdentity(t *testing.T) {
	if _, err := NewWebAuthnVerifier("", testRPID); err == nil {
		t.Error("accepted an empty origin")
	}
	if _, err := NewWebAuthnVerifier(testOrigin, ""); err == nil {
		t.Error("accepted an empty relying party id")
	}
}

func FuzzCBORParse(f *testing.F) {
	f.Add(cborEncMap(
		cborEntry(cborEncText("fmt"), cborEncText("none")),
		cborEntry(cborEncText("attStmt"), cborEncMap()),
		cborEntry(cborEncText("authData"), cborEncBytes([]byte{1, 2, 3})),
	))
	f.Add(coseKeyWith(coseKeyOpts{
		kty: cborEncUint(coseKeyTypeEC2),
		alg: cborEncNegInt(coseAlgES256),
		crv: cborEncUint(coseCurveP256),
		x:   cborEncBytes(bytes.Repeat([]byte{1}, 32)),
		y:   cborEncBytes(bytes.Repeat([]byte{2}, 32)),
	}))
	for _, seed := range [][]byte{
		nil, {0x00}, {0x1C}, {0x18, 0x01}, {0xBF, 0xFF}, {0xC1, 0x01}, {0xF9, 0x00, 0x00},
		{0xA1, 0x41, 0x01, 0x01}, {0x62, 0xFF, 0xFE}, {0xA2, 0x02, 0x01, 0x01, 0x02},
	} {
		f.Add(seed)
	}

	var check func(t *testing.T, item cborItem, depth int) int
	check = func(t *testing.T, item cborItem, depth int) int {
		t.Helper()
		if depth > cborMaxDepth {
			t.Fatalf("accepted an item at depth %d", depth)
		}
		switch item.kind {
		case cborUnsigned, cborNegative, cborBytes, cborText:
			return 1
		case cborMap:
		default:
			t.Fatalf("accepted kind %d", item.kind)
		}
		if len(item.pairs) > cborMaxCollection {
			t.Fatalf("accepted a map of %d pairs", len(item.pairs))
		}
		count := 1
		for _, p := range item.pairs {
			switch p.key.kind {
			case cborUnsigned, cborNegative, cborText:
			default:
				t.Fatalf("accepted a %s map key", p.key.kind)
			}
			count += check(t, p.key, depth+1) + check(t, p.val, depth+1)
		}
		return count
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		item, err := cborParse(data)
		if err != nil {
			return
		}
		if len(data) > cborMaxInput {
			t.Fatalf("accepted %d bytes", len(data))
		}
		if n := check(t, item, 0); n > cborMaxItems {
			t.Fatalf("accepted %d items", n)
		}
		again, err := cborParse(data)
		if err != nil {
			t.Fatalf("accepted then refused the same input: %v", err)
		}
		if again.kind != item.kind || len(again.pairs) != len(item.pairs) {
			t.Fatal("two parses of one input disagree")
		}
	})
}
