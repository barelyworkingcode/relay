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
	"time"
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

func TestMigrateFrontendTokenToCredential_GrantsExactlyReadConfigureAndProxy(t *testing.T) {
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
	if !cred.Grants(ClassRead) || !cred.Grants(ClassConfigure) || !cred.Grants(ClassProxy) {
		t.Fatalf("migrated credential must hold read, configure and proxy: %+v", cred.Classes)
	}
	if len(cred.Classes) != 3 {
		t.Fatalf("migrated credential holds extra classes: %+v", cred.Classes)
	}
}

// TestMigrateFrontendTokenToCredential_UpgradesAnExistingLegacyRecordInPlace
// is the upgrade path a running install takes on its next start after
// ADR-016 decision 4: settings.json already holds the record ADR-015 wrote,
// carrying read+configure, and nothing rewrites it except this function
// noticing the class set moved. Without the upgrade Eve keeps its token and
// loses the proxied surface.
func TestMigrateFrontendTokenToCredential_UpgradesAnExistingLegacyRecordInPlace(t *testing.T) {
	const token = "legacy-token-written-before-adr-016"
	const id = "legacy-id-from-disk"
	const created = "2026-01-01T00:00:00Z"
	s := &Settings{APICredentials: []APICredential{{
		ID:      id,
		Name:    legacyFrontendCredentialName,
		Hash:    hashToken(token),
		Classes: []CapabilityClass{ClassRead, ClassConfigure},
		Created: created,
	}}}

	if !migrateFrontendTokenToCredential(s, token) {
		t.Fatal("a stored legacy record holding only read+configure was left alone; Eve would 403 on every proxied route")
	}
	if len(s.APICredentials) != 1 {
		t.Fatalf("the upgrade minted a second record instead of upgrading in place: %+v", s.APICredentials)
	}
	got := s.APICredentials[0]
	if got.ID != id || got.Created != created {
		t.Fatalf("the upgrade replaced the record's identity rather than its class set: %+v", got)
	}
	if !got.Grants(ClassProxy) {
		t.Fatalf("the upgrade did not add proxy: %+v", got.Classes)
	}
	if s.AuthenticateAPICredential(token) == nil {
		t.Fatal("the token in the upgraded record stopped authenticating")
	}

	if migrateFrontendTokenToCredential(s, token) {
		t.Error("the upgrade is not idempotent, so every start rewrites settings.json")
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

// ---------------------------------------------------------------------------
// Expiry (ADR-016 decision 3)
// ---------------------------------------------------------------------------

func TestAPICredential_Expired_AbsentExpiresMeansNever(t *testing.T) {
	cred := APICredential{ID: "c1", Hash: "irrelevant", Classes: []CapabilityClass{ClassRead}}
	for _, when := range []time.Time{
		time.Unix(0, 0),
		time.Now(),
		time.Now().Add(100 * 365 * 24 * time.Hour),
	} {
		if cred.Expired(when) {
			t.Fatalf("a credential with no Expires read as expired at %s; absent must mean never", when.UTC().Format(time.RFC3339))
		}
	}
}

func TestAPICredential_Expired_BoundaryAndBothSides(t *testing.T) {
	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	cred := APICredential{ID: "c1", Expires: at.Format(time.RFC3339)}

	if cred.Expired(at.Add(-time.Second)) {
		t.Fatal("a credential expired one second before its own expiry")
	}
	if !cred.Expired(at) {
		t.Fatal("the instant named by Expires must already be past the lifetime")
	}
	if !cred.Expired(at.Add(time.Second)) {
		t.Fatal("a credential outlived its expiry")
	}
}

// An Expires relay cannot parse is not the same case as an absent one:
// absent is a value relay writes deliberately, unparseable is a lifetime
// relay cannot evaluate. Reading it as "never" would make a corrupt or
// hand-edited timestamp the way to mint an immortal credential.
func TestAPICredential_Expired_UnparseableFailsClosed(t *testing.T) {
	for _, bad := range []string{
		"not-a-timestamp",
		"2026-08-28",
		"28/08/2026 12:00:00",
		"1756382400",
		" ",
		"2026-13-45T99:99:99Z",
	} {
		cred := APICredential{ID: "c1", Expires: bad}
		if !cred.Expired(time.Now()) {
			t.Fatalf("Expires = %q read as still live; an unreadable lifetime must fail closed", bad)
		}
	}
}

func TestAPICredential_ExpiresRoundTripsThroughSettingsJSON(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	var minted APICredential
	assertNoErr(t, store.With(func(s *Settings) {
		var err error
		minted, _, err = s.MintFor("hermes-login", []CapabilityClass{ClassRead}, 12*time.Hour)
		assertNoErr(t, err, "MintFor")
	}), "store.With")

	if minted.Expires == "" {
		t.Fatal("MintFor with a ttl produced a credential with no expiry")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	var onDisk Settings
	assertNoErr(t, json.Unmarshal(raw, &onDisk), "parse settings.json")
	if len(onDisk.APICredentials) != 1 {
		t.Fatalf("want 1 credential on disk, got %+v", onDisk.APICredentials)
	}
	if got := onDisk.APICredentials[0].Expires; got != minted.Expires {
		t.Fatalf("Expires round-tripped as %q, want %q", got, minted.Expires)
	}
	if onDisk.APICredentials[0].Expired(time.Now()) {
		t.Fatal("a credential minted with a 12h ttl is already expired after a round trip")
	}
}

// The compatibility half of "absent means never": a record written before
// this field existed must come back with no expiry key at all, not with a
// zero timestamp that Expired would then have to interpret.
func TestAPICredential_ExpiresOmittedWhenNoTTL(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	assertNoErr(t, store.With(func(s *Settings) {
		_, _, err := s.Mint("no-ttl", []CapabilityClass{ClassRead})
		assertNoErr(t, err, "Mint")
	}), "store.With")

	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	assertNoErr(t, err, "read settings.json")
	if strings.Contains(string(raw), "expires") {
		t.Fatalf("settings.json carries an expires key for a credential minted with no ttl:\n%s", raw)
	}

	var onDisk Settings
	assertNoErr(t, json.Unmarshal(raw, &onDisk), "parse settings.json")
	if onDisk.APICredentials[0].Expired(time.Now()) {
		t.Fatal("a credential minted with no ttl came back expired")
	}
}

// TestAPICredential_ExpiredIsRefusedExactlyLikeAnUnknownOne is the oracle
// check. Every observable an authenticating caller has must be identical
// between "this token expired" and "this token never existed": the returned
// credential, and the absence of any other signal. The live credential
// beside them is the control -- it proves the store is answering at all.
func TestAPICredential_ExpiredIsRefusedExactlyLikeAnUnknownOne(t *testing.T) {
	s := &Settings{}

	live, livePlain, err := s.MintFor("live", []CapabilityClass{ClassRead}, time.Hour)
	assertNoErr(t, err, "MintFor live")
	expired, expiredPlain, err := s.MintFor("expired", []CapabilityClass{ClassRead}, time.Hour)
	assertNoErr(t, err, "MintFor expired")

	// Backdate rather than sleep: the record is what authentication reads.
	for i := range s.APICredentials {
		if s.APICredentials[i].ID == expired.ID {
			s.APICredentials[i].Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		}
	}

	if got := s.AuthenticateAPICredential(livePlain); got == nil || got.ID != live.ID {
		t.Fatalf("the live credential does not authenticate: %+v", got)
	}

	fromExpired := s.AuthenticateAPICredential(expiredPlain)
	fromUnknown := s.AuthenticateAPICredential("never-minted-anywhere")
	fromEmpty := s.AuthenticateAPICredential("")
	if fromExpired != nil {
		t.Fatalf("an expired credential authenticated: %+v", fromExpired)
	}
	if fromExpired != fromUnknown || fromExpired != fromEmpty {
		t.Fatalf("expired=%v unknown=%v empty=%v; the three must be indistinguishable", fromExpired, fromUnknown, fromEmpty)
	}

	// It is still ON DISK -- refusal is enforcement, not a side effect of
	// reaping -- and FindAPICredential still resolves it, so an operator
	// surface can show what is about to be swept.
	if s.FindAPICredential(expired.ID) == nil {
		t.Fatal("authentication deleted the expired record; reaping is lazy and separate")
	}
}

func TestAPICredential_UnparseableExpiresIsRefusedAtAuthentication(t *testing.T) {
	s := &Settings{}
	_, plaintext, err := s.MintFor("corrupt", []CapabilityClass{ClassRead}, time.Hour)
	assertNoErr(t, err, "MintFor")
	s.APICredentials[0].Expires = "whenever"

	if got := s.AuthenticateAPICredential(plaintext); got != nil {
		t.Fatalf("a credential with an unreadable expiry authenticated: %+v", got)
	}
}

func TestReapExpiredAPICredentials_RemovesOnlyTheExpired(t *testing.T) {
	s := &Settings{}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	s.AddAPICredential(APICredential{ID: "never", Name: "never", Hash: hashToken("a")})
	s.AddAPICredential(APICredential{ID: "live", Name: "live", Hash: hashToken("b"), Expires: future})
	s.AddAPICredential(APICredential{ID: "dead", Name: "dead", Hash: hashToken("c"), Expires: past})
	s.AddAPICredential(APICredential{ID: "corrupt", Name: "corrupt", Hash: hashToken("d"), Expires: "not-a-time"})

	if !reapExpiredAPICredentials(s) {
		t.Fatal("reaping reported nothing removed with two expired records present")
	}
	for _, id := range []string{"never", "live"} {
		if s.FindAPICredential(id) == nil {
			t.Fatalf("reaping removed %q, which has not expired", id)
		}
	}
	for _, id := range []string{"dead", "corrupt"} {
		if s.FindAPICredential(id) != nil {
			t.Fatalf("reaping left %q behind", id)
		}
	}

	if reapExpiredAPICredentials(s) {
		t.Fatal("a second reap over a clean set reported a change, which would rewrite settings.json for nothing")
	}
}

// The legacy credential carries no expiry, so reaping must never touch it --
// sweeping it would 401 Eve and relayScheduler until the next relay start.
func TestReapExpiredAPICredentials_LeavesTheLegacyCredentialAlone(t *testing.T) {
	s := &Settings{}
	const frontendToken = "reap-legacy-token"
	migrateFrontendTokenToCredential(s, frontendToken)
	s.AddAPICredential(APICredential{ID: "dead", Hash: hashToken("x"), Expires: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})

	reapExpiredAPICredentials(s)

	if s.AuthenticateAPICredential(frontendToken) == nil {
		t.Fatal("reaping swept the legacy frontend credential")
	}
	if len(s.APICredentials) != 1 {
		t.Fatalf("want only the legacy credential left, got %+v", s.APICredentials)
	}
}
