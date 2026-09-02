package project

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

func v2Surfaces() McpSurfaces {
	return McpSurfaces{
		"macmcp": macmcpSurface(),
		"fsmcp":  {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
	}
}

func profileWithContext(mcpID, blob string) *config.Project {
	return &config.Project{
		ID: "p1", Name: "Profile", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{mcpID},
		Context:       map[string]json.RawMessage{mcpID: json.RawMessage(blob)},
	}
}

func wantRefusal(t *testing.T, err error, substrings ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	for _, want := range substrings {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should say %q; got: %v", want, err)
		}
	}
}

func TestValidatePermissions_RefusesUndeclaredField(t *testing.T) {
	proj := profileWithContext("macmcp", `{"mail_folders":["INBOX"]}`)
	err := validateProjectPermissions(proj, v2Surfaces())
	wantRefusal(t, err, "mail_folders", "macmcp", "mail_accounts", "mail_mailboxes")
}

func TestValidatePermissions_RefusesWrongType(t *testing.T) {
	proj := profileWithContext("macmcp", `{"mail_accounts":"Bob"}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts", "array")

	proj = profileWithContext("macmcp", `{"mail_accounts":[7]}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts", "string")
}

func TestValidatePermissions_RefusesEmptyRestrictValue(t *testing.T) {
	for _, blob := range []string{
		`{"mail_accounts":null}`,
		`{"mail_accounts":[""]}`,
		`{"mail_accounts":["  "]}`,
		`{"mail_accounts":["*","Bob"]}`,
	} {
		proj := profileWithContext("macmcp", blob)
		wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts")
	}
}

// TestValidatePermissions_AcceptsAConfirmedEmptyOrWildcardValue pins the
// ADR-011 addendum ("A star and an empty array"): an explicit `[]` is no
// longer refused at save time -- it is the confirmed-empty grant, distinct
// from the field being absent entirely -- and `["*"]` is an ordinary
// non-empty string as far as relay's own validation is concerned.
func TestValidatePermissions_AcceptsAConfirmedEmptyOrWildcardValue(t *testing.T) {
	for _, blob := range []string{
		`{"mail_accounts":[]}`,
		`{"mail_accounts":["*"]}`,
	} {
		proj := profileWithContext("macmcp", blob)
		if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
			t.Errorf("%s: expected no refusal, got: %v", blob, err)
		}
	}
}

// SyncProjectToken re-derives a source: "project_path" field moments after any
// save, so accepting an operator's value here would look like acceptance and
// then silently discard it; refusing is louder than that.
func TestValidatePermissions_RefusesOperatorSuppliedProjectPathField(t *testing.T) {
	proj := &config.Project{
		ID: "p1", Name: "Local", Path: "/tmp/x",
		AllowedMcpIDs: []string{"macmcp"},
		Context:       map[string]json.RawMessage{"macmcp": json.RawMessage(`{"file_dirs":["/etc"]}`)},
	}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "file_dirs", "path", "cannot be set")
}

// AccessMode already reads anything but exactly "write" as read, so a stored
// typo is fail-closed at call time — but reading silently as read is not what
// the operator chose, and this validation is the only place that says so.
func TestValidatePermissions_RefusesUnknownAccessMode(t *testing.T) {
	proj := &config.Project{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		Access: map[string]string{"macmcp": "readwrite"}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "macmcp", "readwrite", "read", "write")

	for _, ok := range []string{config.AccessRead, config.AccessWrite} {
		proj.Access["macmcp"] = ok
		if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
			t.Errorf("mode %q should be accepted: %v", ok, err)
		}
	}
}

// toolAllowedByPatterns already fails closed on an uncompilable pattern, so
// this refusal exists for usability, not safety: a broken pattern should say
// so at save time rather than silently stop an agent later.
func TestValidatePermissions_RefusesUncompilablePattern(t *testing.T) {
	proj := &config.Project{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools: map[string][]string{"macmcp": {"mail_[", "mail_*"}}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_[", "macmcp")

	proj.AllowedTools["macmcp"] = []string{"mail_*", "capture_screenshot"}
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Errorf("a glob and an exact name should both be accepted: %v", err)
	}
}

// SyncProjectToken's v1 branch REPLACES the whole context blob with the
// derived allowed_dirs, so a value stored here would vanish at the next edit
// rather than take effect.
func TestValidatePermissions_RefusesContextForV1Schema(t *testing.T) {
	proj := &config.Project{ID: "p1", Path: "/tmp/x", AllowedMcpIDs: []string{"fsmcp"},
		Context: map[string]json.RawMessage{"fsmcp": json.RawMessage(`{"allowed_dirs":["/etc"]}`)}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "fsmcp", "v1")
}

// An MCP relay has never connected to cannot be checked, so it is permitted
// with nothing but an emptiness check: refusing on missing information would
// make an MCP that is merely not running unconfigurable, and CallTool's own
// presence re-check still denies it at the boundary.
func TestValidatePermissions_PermitsUnknownMcpButNotAnEmptyValue(t *testing.T) {
	proj := profileWithContext("whomcp", `{"anything":["a"]}`)
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Fatalf("an unknown MCP should not be unconfigurable: %v", err)
	}
	proj = profileWithContext("whomcp", `{"anything":[]}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "anything", "non-empty")
}

// ContextValues answers nil for a non-object blob, which every caller would
// then treat as an absent scope, so it is refused rather than read as "no
// fields".
func TestValidatePermissions_RefusesNonObjectContext(t *testing.T) {
	proj := profileWithContext("macmcp", `["mail_accounts"]`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "macmcp", "object")
}

func TestValidatePermissions_AcceptsTheWorkedExample(t *testing.T) {
	proj := &config.Project{
		ID: "prof_hermes_bob_inbox", Name: "Hermes — Bob INBOX (read-only)", Kind: config.ProjectKindRemote,
		AllowedMcpIDs: []string{"macmcp"},
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		Access:        map[string]string{"macmcp": config.AccessRead},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"],"mail_mailboxes":["INBOX"]}`),
		},
	}
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Fatalf("ADR-011's own worked example was refused: %v", err)
	}
	if err := ValidateShape(proj); err != nil {
		t.Fatalf("ADR-011's own worked example failed the shape check: %v", err)
	}
}

func TestUpdateProjectContext_ReDerivesTheProjectPathField(t *testing.T) {
	s := &config.Settings{Version: 1}
	surfaces := v2Surfaces()
	proj, err := CreateWithToken(s, "Local", "/tmp/proj", []string{"macmcp"}, nil, nil, surfaces)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := ContextValues(s.Projects[0].Context["macmcp"])
	if !HasScopeValue(before, "file_dirs") {
		t.Fatalf("precondition: file_dirs should have been derived, got %s", s.Projects[0].Context["macmcp"])
	}

	updateProjectContext(s, proj.ID, map[string]json.RawMessage{
		"macmcp": json.RawMessage(`{"mail_accounts":["Alice"]}`),
	}, surfaces)

	after := ContextValues(s.Projects[0].Context["macmcp"])
	if !HasScopeValue(after, "mail_accounts") {
		t.Errorf("operator value was not stored: %s", s.Projects[0].Context["macmcp"])
	}
	if !HasScopeValue(after, "file_dirs") {
		t.Errorf("the derived field was destroyed by an operator edit: %s", s.Projects[0].Context["macmcp"])
	}
}

func TestUpdateProjectAccessAndContext_DropUngrantedMcps(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Profile", "", []string{"macmcp"}, nil, nil, v2Surfaces())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.UpdateProjectAccess(proj.ID, map[string]string{"macmcp": config.AccessWrite, "other": config.AccessWrite})
	updateProjectContext(s, proj.ID, map[string]json.RawMessage{
		"macmcp": json.RawMessage(`{"mail_accounts":["Bob"]}`),
		"other":  json.RawMessage(`{"whatever":["x"]}`),
	}, v2Surfaces())

	got := s.Projects[0]
	if _, ok := got.Access["other"]; ok {
		t.Error("a mode was stored for an MCP the profile does not grant")
	}
	if _, ok := got.Context["other"]; ok {
		t.Error("a scope was stored for an MCP the profile does not grant")
	}
	if got.Access["macmcp"] != config.AccessWrite {
		t.Errorf("granted MCP lost its mode: %#v", got.Access)
	}
}

// Dropping an unrecognised mode would fall back to the default, which for a
// local project is write — silently widening a grant on the strength of a
// typo — so it is kept as-is instead.
func TestUpdateProjectAccess_KeepsAnUnknownModeRatherThanWidening(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := CreateWithToken(s, "Local", "/tmp/proj", []string{"macmcp"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.UpdateProjectAccess(proj.ID, map[string]string{"macmcp": "wrIte"})
	if got := s.Projects[0].Access["macmcp"]; got != "wrIte" {
		t.Fatalf("mode was rewritten or dropped: %q", got)
	}
	tok := config.StoredTokenForProject(s, &s.Projects[0], "hash")
	if mode := tok.AccessMode("macmcp"); mode != config.AccessRead {
		t.Errorf("an unrecognised mode must read as %q, got %q", config.AccessRead, mode)
	}
}

// An explicit false is kept, not discarded: for a local project, which
// defaults to allowed, it is the only way to say the opposite, and discarding
// it would make a confined local agent unexpressible.
func TestUpdateProjectAllowExternal_KeepsBothValuesAndDropsUngrantedMcps(t *testing.T) {
	s := &config.Settings{Version: 1}
	proj, err := CreateWithTokenKind(s, config.ProjectKindRemote, "Profile", "", []string{"macmcp"}, nil, nil, v2Surfaces())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.UpdateProjectAllowExternal(proj.ID, map[string]bool{
		"macmcp": true,
		"other":  true,
	})
	got := s.Projects[0]
	if !got.AllowExternal["macmcp"] {
		t.Errorf("the granted MCP lost its outbound grant: %#v", got.AllowExternal)
	}
	if _, ok := got.AllowExternal["other"]; ok {
		t.Error("an outbound grant was stored for an MCP the profile does not grant")
	}
	tok := config.StoredTokenForProject(s, &s.Projects[0], "hash")
	if !tok.ExternalAllowed("macmcp") {
		t.Error("the grant did not reach the token every auth path builds")
	}
	if tok.ExternalAllowed("other") {
		t.Error("a dropped entry still reached the token")
	}

	local, err := CreateWithToken(s, "Local", "/tmp/proj", []string{"macmcp"}, nil, nil, v2Surfaces())
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	s.UpdateProjectAllowExternal(local.ID, map[string]bool{"macmcp": false})
	stored, _ := config.FindProjectByID(s, local.ID)
	if allowed, ok := stored.AllowExternal["macmcp"]; !ok || allowed {
		t.Fatalf("a local project's explicit refusal was discarded: %#v", stored.AllowExternal)
	}
	if config.StoredTokenForProject(s, stored, "hash").ExternalAllowed("macmcp") {
		t.Error("a stored refusal did not reach the token")
	}

	s.UpdateProjectAllowExternal(local.ID, nil)
	stored, _ = config.FindProjectByID(s, local.ID)
	if stored.AllowExternal != nil {
		t.Errorf("a cleared field left a map behind: %#v", stored.AllowExternal)
	}
	// Marshalling a lone Project requires its Secret fields to be sealed
	// first (§4.4) — wrap it in a throwaway Settings just to reach
	// sealAllSecrets, since that is the only place the sealed set is
	// enumerated.
	sealMe := &config.Settings{Projects: []config.Project{*stored}}
	if err := config.SealAllSecrets(sealMe, testSealer()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob, err := json.Marshal(sealMe.Projects[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "allow_external") {
		t.Errorf("a record saying nothing serialized the key anyway: %s", blob)
	}
	if !config.StoredTokenForProject(s, stored, "hash").ExternalAllowed("macmcp") {
		t.Error("clearing the field did not return the local project to its default")
	}

	s.UpdateProjectAllowExternal(proj.ID, map[string]bool{"macmcp": true})
	updateProjectMcps(s, proj.ID, []string{}, v2Surfaces())
	if _, ok := s.Projects[0].AllowExternal["macmcp"]; ok {
		t.Errorf("an outbound grant outlived the MCP grant it belonged to: %#v", s.Projects[0].AllowExternal)
	}
}
