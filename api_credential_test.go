package main

// ADR-015 decision 3: a credential names its classes, and absent means
// none. The tests here are adversarial toward the fail-closed rules on
// purpose — an empty Classes slice is the case a reviewer will attack
// first, so it is asserted against every class, not just one.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var allClasses = []CapabilityClass{ClassRead, ClassConfigure, ClassGrant, ClassExecute}

// ---------------------------------------------------------------------------
// Grants
// ---------------------------------------------------------------------------

func TestAPICredential_Grants_NilClassesGrantsNothing(t *testing.T) {
	cred := APICredential{ID: "c1", Hash: "irrelevant"}
	for _, class := range allClasses {
		if cred.Grants(class) {
			t.Errorf("nil Classes granted %q, want refused", class)
		}
	}
}

func TestAPICredential_Grants_EmptyClassesGrantsNothing(t *testing.T) {
	cred := APICredential{ID: "c1", Hash: "irrelevant", Classes: []CapabilityClass{}}
	for _, class := range allClasses {
		if cred.Grants(class) {
			t.Errorf("empty Classes granted %q, want refused", class)
		}
	}
}

func TestAPICredential_Grants_UnknownClassGrantsNothingAndDoesNotPoisonCredential(t *testing.T) {
	cred := APICredential{ID: "c1", Hash: "irrelevant", Classes: []CapabilityClass{"made_up_class", "another_bogus_one"}}
	for _, class := range allClasses {
		if cred.Grants(class) {
			t.Errorf("credential holding only unknown class strings granted %q, want refused", class)
		}
	}

	// An unknown entry sitting beside a real one must not poison the whole
	// credential -- the real class must still work.
	cred.Classes = append(cred.Classes, ClassRead)
	if !cred.Grants(ClassRead) {
		t.Fatal("a real class beside an unknown one was refused; an unknown entry must not poison the whole credential")
	}
	if cred.Grants(ClassConfigure) {
		t.Fatal("ClassConfigure was granted by a credential that never named it")
	}
}

func TestAPICredential_Grants_TrueFalsePerClass(t *testing.T) {
	cred := APICredential{ID: "c1", Hash: "irrelevant", Classes: []CapabilityClass{ClassRead, ClassExecute}}
	cases := map[CapabilityClass]bool{
		ClassRead:      true,
		ClassConfigure: false,
		ClassGrant:     false,
		ClassExecute:   true,
	}
	for class, want := range cases {
		if got := cred.Grants(class); got != want {
			t.Errorf("Grants(%q) = %v, want %v", class, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Mint
// ---------------------------------------------------------------------------

func TestMint_ReturnsPlaintextOnceAndStoresOnlyTheHash(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	var cred APICredential
	var plaintext string
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		cred, plaintext, err = s.Mint("ci-bot", []CapabilityClass{ClassRead})
		assertNoErr(t, err, "Mint")
	}), "store.With")

	if plaintext == "" {
		t.Fatal("Mint returned an empty plaintext token")
	}
	if cred.Hash == "" || cred.Hash == plaintext {
		t.Fatalf("stored Hash is empty or equals the plaintext: %q", cred.Hash)
	}
	if cred.Hash != hashToken(plaintext) {
		t.Fatalf("stored Hash does not match hashToken(plaintext): %q vs %q", cred.Hash, hashToken(plaintext))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	if !strings.Contains(string(raw), cred.Hash) {
		t.Fatal("settings.json does not contain the credential's hash")
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("settings.json contains the PLAINTEXT token — it must never be stored")
	}

	// The in-memory record handed back by Mint carries only the hash too;
	// there is no second field anywhere holding the plaintext.
	credJSON, err := json.Marshal(cred)
	assertNoErr(t, err, "marshal credential")
	if strings.Contains(string(credJSON), plaintext) {
		t.Fatal("the minted APICredential record itself contains the plaintext token")
	}
}

// ---------------------------------------------------------------------------
// Authorize
// ---------------------------------------------------------------------------

func TestCredentialAuthorizer_Authorize_MissingHeader(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	req, _ := http.NewRequest("GET", "http://unix/api/x", nil)
	if err := auth.Authorize(req, ClassRead); err != errNoCredential {
		t.Fatalf("missing header: got %v, want errNoCredential", err)
	}
}

func TestCredentialAuthorizer_Authorize_MalformedHeader(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	for _, header := range []string{"Basic dXNlcjpwYXNz", "Bearer", "Token abc123", "Bearer "} {
		req, _ := http.NewRequest("GET", "http://unix/api/x", nil)
		req.Header.Set("Authorization", header)
		if err := auth.Authorize(req, ClassRead); err != errNoCredential {
			t.Errorf("header %q: got %v, want errNoCredential", header, err)
		}
	}
}

func TestCredentialAuthorizer_Authorize_UnknownTokenIsNoCredential(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	req, _ := http.NewRequest("GET", "http://unix/api/x", nil)
	req.Header.Set("Authorization", "Bearer this-token-was-never-minted")
	if err := auth.Authorize(req, ClassRead); err != errNoCredential {
		t.Fatalf("unknown token: got %v, want errNoCredential", err)
	}
}

func TestCredentialAuthorizer_Authorize_KnownTokenWithoutClassIsRefused(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	var plaintext string
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		_, plaintext, err = s.Mint("read-only", []CapabilityClass{ClassRead})
		assertNoErr(t, err, "Mint")
	}), "store.With")

	req, _ := http.NewRequest("POST", "http://unix/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	if err := auth.Authorize(req, ClassConfigure); err != errClassNotGranted {
		t.Fatalf("credential without the class: got %v, want errClassNotGranted", err)
	}
}

func TestCredentialAuthorizer_Authorize_KnownTokenWithClassIsAllowedAndExposesCredID(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	var cred APICredential
	var plaintext string
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		cred, plaintext, err = s.Mint("configurer", []CapabilityClass{ClassConfigure})
		assertNoErr(t, err, "Mint")
	}), "store.With")

	req, _ := http.NewRequest("PUT", "http://unix/api/services/x", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	if err := auth.Authorize(req, ClassConfigure); err != nil {
		t.Fatalf("credential with the class: got %v, want nil", err)
	}

	gotID, ok := APICredentialIDFromContext(req.Context())
	if !ok {
		t.Fatal("Authorize did not attach a resolved credential id to the request context")
	}
	if gotID != cred.ID {
		t.Fatalf("context credential id = %q, want %q", gotID, cred.ID)
	}
}

// A credential minted by something that predates the class model (nil
// Classes) must be inert end-to-end, not merely inert at the Grants level.
func TestCredentialAuthorizer_Authorize_EmptyClassesRefusedForEveryClass(t *testing.T) {
	store := newCLISandboxStore(t)
	auth := NewCredentialAuthorizer(store)

	var plaintext string
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		_, plaintext, err = s.Mint("legacy-tool", nil)
		assertNoErr(t, err, "Mint")
	}), "store.With")

	for _, class := range allClasses {
		req, _ := http.NewRequest("GET", "http://unix/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		if err := auth.Authorize(req, class); err != errClassNotGranted {
			t.Errorf("class %q: got %v, want errClassNotGranted", class, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Migration
// ---------------------------------------------------------------------------

func TestMigrateFrontendTokenToCredential_GrantsExactlyReadAndConfigure(t *testing.T) {
	s := &Settings{}
	if changed := migrateFrontendTokenToCredential(s, "legacy-plaintext-token"); !changed {
		t.Fatal("first migration call reported no change")
	}
	if len(s.APICredentials) != 1 {
		t.Fatalf("want exactly 1 migrated credential, got %d", len(s.APICredentials))
	}
	cred := s.APICredentials[0]
	if cred.Grants(ClassGrant) || cred.Grants(ClassExecute) {
		t.Fatalf("migrated credential must not hold grant or execute: %+v", cred.Classes)
	}
	if !cred.Grants(ClassRead) || !cred.Grants(ClassConfigure) {
		t.Fatalf("migrated credential must hold read and configure: %+v", cred.Classes)
	}
	if len(cred.Classes) != 2 {
		t.Fatalf("migrated credential holds extra classes: %+v", cred.Classes)
	}
}

func TestMigrateFrontendTokenToCredential_IdempotentOnRepeatedCall(t *testing.T) {
	s := &Settings{}
	migrateFrontendTokenToCredential(s, "legacy-plaintext-token")
	if len(s.APICredentials) != 1 {
		t.Fatalf("after first call want 1 credential, got %d", len(s.APICredentials))
	}
	first := s.APICredentials[0]

	if changed := migrateFrontendTokenToCredential(s, "legacy-plaintext-token"); changed {
		t.Fatal("second call with the same token reported a change")
	}
	if len(s.APICredentials) != 1 {
		t.Fatalf("second call minted a second credential: %d total", len(s.APICredentials))
	}
	if s.APICredentials[0].ID != first.ID {
		t.Fatal("second call replaced the credential's identity instead of leaving it alone")
	}
}

// FrontendChannel.Ensure mints a brand new random token on every process
// start, so a realistic multi-restart sequence calls this function with a
// DIFFERENT token each time. The migration must still converge on one
// credential rather than accumulating a stale one per restart.
func TestMigrateFrontendTokenToCredential_UpdatesInPlaceAcrossTokenRotation(t *testing.T) {
	s := &Settings{}
	migrateFrontendTokenToCredential(s, "token-from-boot-one")
	firstID := s.APICredentials[0].ID

	changed := migrateFrontendTokenToCredential(s, "token-from-boot-two")
	if !changed {
		t.Fatal("migrating a rotated token reported no change")
	}
	if len(s.APICredentials) != 1 {
		t.Fatalf("token rotation across restarts must update in place, not accumulate: %d credentials", len(s.APICredentials))
	}
	if s.APICredentials[0].ID != firstID {
		t.Fatal("token rotation replaced the credential's identity rather than updating its hash")
	}
	if s.AuthenticateAPICredential("token-from-boot-one") != nil {
		t.Fatal("the old rotated-out token still authenticates")
	}
	if s.AuthenticateAPICredential("token-from-boot-two") == nil {
		t.Fatal("the new token does not authenticate after migration")
	}
}

func TestMigrateFrontendTokenToCredential_PreservesTokenValueForExistingConsumer(t *testing.T) {
	s := &Settings{}
	const frontendToken = "eve-and-scheduler-hold-this-value"
	migrateFrontendTokenToCredential(s, frontendToken)

	cred := s.AuthenticateAPICredential(frontendToken)
	if cred == nil {
		t.Fatal("the exact frontend token value does not resolve to the migrated credential")
	}
	if !cred.Grants(ClassRead) || !cred.Grants(ClassConfigure) {
		t.Fatal("the resolved credential does not carry read+configure")
	}
}

func TestMigrateFrontendTokenToCredential_EmptyTokenIsNoOp(t *testing.T) {
	s := &Settings{}
	if changed := migrateFrontendTokenToCredential(s, ""); changed {
		t.Fatal("migrating an empty token reported a change")
	}
	if len(s.APICredentials) != 0 {
		t.Fatalf("migrating an empty token minted a credential: %+v", s.APICredentials)
	}
}

// ---------------------------------------------------------------------------
// Settings round trip
// ---------------------------------------------------------------------------

// The headline compatibility promise: an install that never mints a
// credential must keep a settings.json byte-identical to the one it had
// before this field existed, exactly like Enrolments and Audit.
func TestSettings_APICredentialsOmittedWhenNoneMinted(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	if strings.Contains(string(raw), "api_credentials") {
		t.Fatalf("settings.json carries an api_credentials key with no credential minted:\n%s", raw)
	}

	var onDisk map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(raw, &onDisk), "parse settings.json")
	if _, present := onDisk["api_credentials"]; present {
		t.Fatal("api_credentials key present in the parsed JSON object")
	}
}

func TestSettings_APICredentialsRoundTripsAfterMint(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	assertNoErr(t, store.With(func(s *Settings) {
		_, _, err := s.Mint("hermes", []CapabilityClass{ClassRead})
		assertNoErr(t, err, "Mint")
	}), "store.With")

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	var onDisk Settings
	assertNoErr(t, json.Unmarshal(raw, &onDisk), "parse settings.json")
	if len(onDisk.APICredentials) != 1 || onDisk.APICredentials[0].Name != "hermes" {
		t.Fatalf("minted credential did not round-trip: %+v", onDisk.APICredentials)
	}
}

// ---------------------------------------------------------------------------
// Add/Remove/Find mutators
// ---------------------------------------------------------------------------

func TestAPICredential_AddRemoveFind(t *testing.T) {
	s := &Settings{}
	cred, _, err := s.Mint("tool-a", []CapabilityClass{ClassRead})
	assertNoErr(t, err, "Mint")

	if got := s.FindAPICredential(cred.ID); got == nil || got.ID != cred.ID {
		t.Fatalf("FindAPICredential did not find the minted credential: %+v", got)
	}

	removed, ok := s.RemoveAPICredential(cred.ID)
	if !ok || removed.ID != cred.ID {
		t.Fatalf("RemoveAPICredential did not report the removed credential: %+v", removed)
	}
	if s.FindAPICredential(cred.ID) != nil {
		t.Fatal("credential still resolvable by id after removal")
	}
	if len(s.APICredentials) != 0 {
		t.Fatalf("removal left stray entries: %+v", s.APICredentials)
	}

	if _, ok := s.RemoveAPICredential("does-not-exist"); ok {
		t.Fatal("removing an unknown id reported success")
	}
}

func TestMigrateFrontendTokenToCredential_OverwritesAWidenedClassSet(t *testing.T) {
	s := &Settings{APICredentials: []APICredential{{
		ID:      "hand-written",
		Name:    legacyFrontendCredentialName,
		Hash:    hashToken("tok"),
		Classes: []CapabilityClass{ClassRead, ClassConfigure, ClassGrant, ClassExecute},
	}}}

	if !migrateFrontendTokenToCredential(s, "tok") {
		t.Fatal("migration reported no change against a record carrying classes it does not own")
	}

	got := s.APICredentials[0]
	for _, class := range []CapabilityClass{ClassGrant, ClassExecute} {
		if got.Grants(class) {
			t.Errorf("legacy credential still grants %q after migration", class)
		}
	}
	for _, class := range []CapabilityClass{ClassRead, ClassConfigure} {
		if !got.Grants(class) {
			t.Errorf("legacy credential lost %q", class)
		}
	}
	if migrateFrontendTokenToCredential(s, "tok") {
		t.Error("migration is not idempotent once the record is correct")
	}
}
