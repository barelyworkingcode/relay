package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// TestSecrets_ForEachVisitsEverySecretField walks the Settings type by
// reflection and asserts forEachSecret visits exactly that set of fields on
// a populated value. A Secret field added later and not enumerated in
// forEachSecret would reach disk in the clear — this is AC-4's whole test
// surface.
func TestSecrets_ForEachVisitsEverySecretField(t *testing.T) {
	// The reflect walk over the TYPE finds every field declaration of type
	// Secret reachable from Settings, independent of forEachSecret's own
	// hand-written enumeration below — a Secret field added to the type
	// but never added to this set means the type walk itself needs
	// updating, which is the point: it is the mechanical audit a reviewer
	// runs by hand when Settings' shape changes.
	typePaths := map[string]bool{}
	walkForSecretFields(reflect.TypeOf(Settings{}), "Settings", typePaths)
	wantTypePaths := map[string]bool{
		"Settings.AdminSecret":                          true,
		"Settings.Projects.Token":                       true,
		"Settings.ExternalMcps.Env":                     true,
		"Settings.ExternalMcps.OAuthState.ClientSecret": true,
		"Settings.ExternalMcps.OAuthState.AccessToken":  true,
		"Settings.ExternalMcps.OAuthState.RefreshToken": true,
		"Settings.Services.Env":                         true,
	}
	if !reflect.DeepEqual(typePaths, wantTypePaths) {
		t.Fatalf("Secret-typed fields reachable from Settings = %v, want %v", typePaths, wantTypePaths)
	}

	// forEachSecret's own runtime enumeration must visit exactly the
	// concrete paths a populated value produces at each of those field
	// declarations.
	s := &Settings{
		AdminSecret: NewSecret("admin"),
		Projects:    []Project{{ID: "p1", Token: NewSecret("tok1")}},
		ExternalMcps: []ExternalMcp{{
			ID:  "mcp1",
			Env: map[string]Secret{"K": NewSecret("v")},
			OAuthState: &OAuthState{
				ClientSecret: NewSecret("cs"),
				AccessToken:  NewSecret("at"),
				RefreshToken: NewSecret("rt"),
			},
		}},
		Services: []ServiceConfig{{ID: "svc1", Env: map[string]Secret{"E": NewSecret("v2")}}},
	}

	got := map[string]bool{}
	if err := forEachSecret(s, func(path string, sec *Secret) error {
		if _, dup := got[path]; dup {
			t.Errorf("forEachSecret visited %s twice", path)
		}
		got[path] = true
		return nil
	}); err != nil {
		t.Fatalf("forEachSecret: %v", err)
	}

	wantPaths := map[string]bool{
		"admin_secret":                                 true,
		"projects/p1/token":                            true,
		"external_mcps/mcp1/env/K":                     true,
		"external_mcps/mcp1/oauth_state/access_token":  true,
		"external_mcps/mcp1/oauth_state/refresh_token": true,
		"external_mcps/mcp1/oauth_state/client_secret": true,
		"services/svc1/env/E":                          true,
	}
	if !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("forEachSecret visited %v, want %v", got, wantPaths)
	}
}

// walkForSecretFields records, for documentation of intent alongside the
// concrete assertion above, every struct field of type Secret reachable
// from t — including through slices, maps and pointers — so a reviewer
// changing Settings' shape has a mechanical check to run by hand:
// forEachSecret's own hand-written enumeration must grow in step with any
// hit this records that TestSecrets_ForEachVisitsEverySecretField's fixture
// does not already exercise.
func walkForSecretFields(t reflect.Type, path string, out map[string]bool) {
	switch t.Kind() {
	case reflect.Ptr, reflect.Slice:
		walkForSecretFields(t.Elem(), path, out)
	case reflect.Map:
		walkForSecretFields(t.Elem(), path, out)
	case reflect.Struct:
		if t == reflect.TypeOf(Secret{}) {
			out[path] = true
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			walkForSecretFields(f.Type, path+"."+f.Name, out)
		}
	}
}

// TestSealAllSecrets_RoundTripsThroughOpen is the store-independent
// round-trip: seal a populated Settings, marshal it, unmarshal a fresh
// value, open it under the same sealer, and confirm every plaintext
// reappears exactly and every sealed field marshals as an envelope object
// rather than a bare string (AC-1, AC-2).
func TestSealAllSecrets_RoundTripsThroughOpen(t *testing.T) {
	sealer := testSealer()
	s := &Settings{
		AdminSecret: NewSecret("s3cr3t-admin"),
		Projects:    []Project{{ID: "p1", Token: NewSecret("p1-token")}},
		ExternalMcps: []ExternalMcp{{
			ID: "mcp1",
			Env: map[string]Secret{
				"OPENAI_API_KEY": NewSecret("sk-obviously-secret"),
				"LOG_LEVEL":      NewSecret("debug"), // AC-1b: sealed too, uniformly
			},
		}},
	}

	if err := SealAllSecrets(s, sealer); err != nil {
		t.Fatalf("sealAllSecrets: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// AC-1 / AC-2: byte-scan the sealed document for every plaintext value
	// and confirm the clear fields are still ordinary strings.
	for _, plaintext := range []string{"s3cr3t-admin", "p1-token", "sk-obviously-secret", "debug"} {
		if bytesContainsString(data, plaintext) {
			t.Errorf("sealed document contains plaintext %q:\n%s", plaintext, data)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["admin_secret"].(map[string]any); !ok {
		t.Errorf("admin_secret is not an envelope object: %v", raw["admin_secret"])
	}
	if _, ok := raw["projects"].([]any)[0].(map[string]any)["token"].(map[string]any); !ok {
		t.Errorf("projects[0].token is not an envelope object")
	}
	envRaw := raw["external_mcps"].([]any)[0].(map[string]any)["env"].(map[string]any)
	for k, v := range envRaw {
		if _, ok := v.(map[string]any); !ok {
			t.Errorf("env key %q is not sealed (a name-based heuristic would leave LOG_LEVEL clear; it must not): %v", k, v)
		}
	}
	if _, ok := raw["external_mcps"].([]any)[0].(map[string]any)["id"].(string); !ok {
		t.Errorf("env keys / mcp id must stay plain strings")
	}

	var reloaded Settings
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("unmarshal into Settings: %v", err)
	}
	if errs := openAllSecrets(&reloaded, sealer); len(errs) != 0 {
		t.Fatalf("openAllSecrets: %v", errs)
	}
	if pt, ok := reloaded.AdminSecret.Reveal(); !ok || pt != "s3cr3t-admin" {
		t.Errorf("AdminSecret round-trip = (%q, %v), want (s3cr3t-admin, true)", pt, ok)
	}
	if pt, ok := reloaded.Projects[0].Token.Reveal(); !ok || pt != "p1-token" {
		t.Errorf("Project token round-trip = (%q, %v), want (p1-token, true)", pt, ok)
	}
	if pt, ok := reloaded.ExternalMcps[0].Env["OPENAI_API_KEY"].Reveal(); !ok || pt != "sk-obviously-secret" {
		t.Errorf("env value round-trip = (%q, %v), want (sk-obviously-secret, true)", pt, ok)
	}
}

func bytesContainsString(b []byte, s string) bool {
	return len(s) > 0 && (func() bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	})()
}

// TestVerifyProjectTokenHashes catches both directions of §4.2's
// invariant: a token that hashes correctly stays available, and one that
// does not is closed off rather than served (AC-6b) — the on-load half.
// The on-migration half (refuse and write nothing) is pinned in
// settings_migrate_test.go.
func TestVerifyProjectTokenHashes(t *testing.T) {
	good := NewSecret("good-token")
	s := &Settings{
		Projects: []Project{
			{ID: "ok", Token: good, TokenHash: HashToken("good-token")},
			{ID: "bad", Token: NewSecret("bad-token"), TokenHash: HashToken("something-else")},
		},
	}
	errs := verifyProjectTokenHashes(s)
	if len(errs) != 1 {
		t.Fatalf("verifyProjectTokenHashes returned %d errors, want 1: %v", len(errs), errs)
	}
	if _, ok := errs["projects/bad/token"]; !ok {
		t.Fatalf("expected an error keyed projects/bad/token, got %v", errs)
	}
	if pt, ok := s.Projects[0].Token.Reveal(); !ok || pt != "good-token" {
		t.Errorf("the matching project's token was disturbed: (%q, %v)", pt, ok)
	}
	if _, ok := s.Projects[1].Token.Reveal(); ok {
		t.Errorf("a mismatched token must not remain revealable")
	}

	// The revoked Secret can never be resealed — sealAllSecrets refuses
	// the whole write rather than silently dropping or re-sealing an
	// empty placeholder over it (§4.5).
	if err := SealAllSecrets(s, testSealer()); err == nil {
		t.Fatal("sealAllSecrets succeeded over a project whose token could not be resealed")
	}
}

// TestSecret_MarshalRefusesUnsealed pins AC-5: json.Marshal of a Settings
// (or a bare Secret) that has never been sealed errors rather than
// emitting plaintext or a placeholder.
func TestSecret_MarshalRefusesUnsealed(t *testing.T) {
	sec := NewSecret("never-sealed")
	if _, err := json.Marshal(sec); err == nil {
		t.Fatal("Marshal of an unsealed Secret succeeded")
	}
	s := &Settings{AdminSecret: sec}
	if _, err := json.Marshal(s); err == nil {
		t.Fatal("Marshal of a Settings holding an unsealed Secret succeeded")
	}
}

// TestSecret_NeverRendersPlaintext pins AC-5b: every format verb and a
// direct String() call show the placeholder, never the value.
func TestSecret_NeverRendersPlaintext(t *testing.T) {
	sec := NewSecret("format-verb-canary")
	for _, rendered := range []string{
		fmt.Sprintf("%v", sec),
		fmt.Sprintf("%s", sec),
		fmt.Sprintf("%+v", sec),
		sec.String(),
	} {
		if bytesContainsString([]byte(rendered), "format-verb-canary") {
			t.Errorf("rendering leaked the plaintext: %q", rendered)
		}
		if rendered != "<sealed>" {
			t.Errorf("rendered = %q, want <sealed>", rendered)
		}
	}
}

// TestSecret_UnmarshalClassifiesEnvelopeShapes pins AC-7: a bare string is
// legacy plaintext; a valid envelope opens; an unsupported "sealed" value
// and a corrupt object are each named distinctly rather than collapsing
// into a generic parse error.
func TestSecret_UnmarshalClassifiesEnvelopeShapes(t *testing.T) {
	sealer := testSealer()

	var legacy Secret
	if err := json.Unmarshal([]byte(`"plain-value"`), &legacy); err != nil {
		t.Fatalf("legacy plaintext: %v", err)
	}
	if pt, ok := legacy.Reveal(); !ok || pt != "plain-value" {
		t.Fatalf("legacy Reveal = (%q, %v)", pt, ok)
	}

	sealed, err := sealer.Seal([]byte("x"), []byte("aad"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	validEnvelope, err := json.Marshal(sealed)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var opened Secret
	if err := json.Unmarshal(validEnvelope, &opened); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	if _, ok := opened.Reveal(); ok {
		t.Fatal("a freshly-unmarshalled envelope must be closed until openAllSecrets runs")
	}

	var unsupported Secret
	errUnsupported := json.Unmarshal([]byte(`{"sealed":"v2","key":"x","n":"x","ct":"x"}`), &unsupported)
	if errUnsupported == nil {
		t.Fatal("an unsupported seal version must be refused")
	}

	var corrupt Secret
	errCorrupt := json.Unmarshal([]byte(`{"sealed":"v1"}`), &corrupt)
	if errCorrupt == nil {
		t.Fatal("a sealed object missing key/n/ct must be refused")
	}

	if errUnsupported.Error() == errCorrupt.Error() {
		t.Fatalf("unsupported-format and corrupt collapsed into the same message: %q", errUnsupported)
	}
}
