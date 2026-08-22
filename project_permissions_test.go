package main

// ADR-011 decisions 2, 2b, 4 and 6: the whole permission set is operator-
// settable, and every value is validated on save whichever surface produced
// it. The constraint these tests exist for is the ADR's second: an editor
// whose easiest failure is a confinement that does not confine is operability
// DEFEATING security rather than trading against it. A refused value is a
// value that never becomes a grant somebody trusts.

import (
	"encoding/json"
	"strings"
	"testing"
)

// v2Surfaces is macMCP's worked example plus fsMCP's v1 declaration, which is
// what a host running both looks like today.
func v2Surfaces() McpSurfaces {
	return McpSurfaces{
		"macmcp": macmcpSurface(),
		"fsmcp":  {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
	}
}

func profileWithContext(mcpID, blob string) *Project {
	return &Project{
		ID: "p1", Name: "Profile", Kind: ProjectKindRemote,
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

// A value for a field the MCP does not declare is refused, and the refusal
// says which fields DO exist — a refusal that says a name is wrong without
// saying which are right is one an operator answers by guessing.
func TestValidatePermissions_RefusesUndeclaredField(t *testing.T) {
	proj := profileWithContext("macmcp", `{"mail_folders":["INBOX"]}`)
	err := validateProjectPermissions(proj, v2Surfaces())
	wantRefusal(t, err, "mail_folders", "macmcp", "mail_accounts", "mail_mailboxes")
}

// The declared fragment is a type contract, not decoration: a string where an
// array was declared is refused rather than injected as something the MCP will
// read as one entry or as nothing.
func TestValidatePermissions_RefusesWrongType(t *testing.T) {
	proj := profileWithContext("macmcp", `{"mail_accounts":"Bob"}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts", "array")

	proj = profileWithContext("macmcp", `{"mail_accounts":[7]}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts", "string")
}

// Empty is a refusal on all three sides (decision 4). "No restriction" is not
// expressible as emptiness, so an empty list stored here would be a grant that
// reads on screen as confined and refuses every call at runtime.
func TestValidatePermissions_RefusesEmptyRestrictValue(t *testing.T) {
	for _, blob := range []string{
		`{"mail_accounts":[]}`,
		`{"mail_accounts":null}`,
		`{"mail_accounts":[""]}`,
		`{"mail_accounts":["  "]}`,
	} {
		proj := profileWithContext("macmcp", blob)
		wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_accounts")
	}
}

// A source: "project_path" field is relay's to derive. An operator setting one
// is either confused about what the field is or is widening a bound relay
// controls, and either way SyncProjectToken would overwrite it moments later —
// a value that silently disappears is worse than a refusal.
func TestValidatePermissions_RefusesOperatorSuppliedProjectPathField(t *testing.T) {
	proj := &Project{
		ID: "p1", Name: "Local", Path: "/tmp/x",
		AllowedMcpIDs: []string{"macmcp"},
		Context:       map[string]json.RawMessage{"macmcp": json.RawMessage(`{"write_dirs":["/etc"]}`)},
	}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "write_dirs", "path", "cannot be set")
}

// The mode is a closed set. AccessMode already reads anything that is not
// exactly "write" as read, so a stored typo is fail-closed — but a typo that
// silently means read is a confinement the operator did not choose, and this
// is the only place it can be said out loud.
func TestValidatePermissions_RefusesUnknownAccessMode(t *testing.T) {
	proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		Access: map[string]string{"macmcp": "readwrite"}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "macmcp", "readwrite", "read", "write")

	for _, ok := range []string{AccessRead, AccessWrite} {
		proj.Access["macmcp"] = ok
		if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
			t.Errorf("mode %q should be accepted: %v", ok, err)
		}
	}
}

// A pattern that will not compile matches NO tool (toolAllowedByPatterns fails
// closed), so an allowlist of nothing but a broken pattern grants nothing —
// safe, and a terrible thing to discover from an agent that stopped working.
func TestValidatePermissions_RefusesUncompilablePattern(t *testing.T) {
	proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools: map[string][]string{"macmcp": {"mail_[", "mail_*"}}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "mail_[", "macmcp")

	proj.AllowedTools["macmcp"] = []string{"mail_*", "capture_screenshot"}
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Errorf("a glob and an exact name should both be accepted: %v", err)
	}
}

// A v1 MCP has no operator-settable context at all: SyncProjectToken's v1
// branch REPLACES the whole blob with the derived allowed_dirs, so a value
// stored here is one that vanishes at the next path or MCP edit.
func TestValidatePermissions_RefusesContextForV1Schema(t *testing.T) {
	proj := &Project{ID: "p1", Path: "/tmp/x", AllowedMcpIDs: []string{"fsmcp"},
		Context: map[string]json.RawMessage{"fsmcp": json.RawMessage(`{"allowed_dirs":["/etc"]}`)}}
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "fsmcp", "v1")
}

// An MCP relay has never connected to cannot be checked, and is PERMITTED with
// nothing but an emptiness check. Same stance as ValidateProjectGrants and for
// the same reason: this is a coherence check an operator sees at edit time,
// not the boundary. Refusing on missing information would make an MCP that is
// merely not running unconfigurable, and CallTool's presence re-check still
// denies.
func TestValidatePermissions_PermitsUnknownMcpButNotAnEmptyValue(t *testing.T) {
	proj := profileWithContext("whomcp", `{"anything":["a"]}`)
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Fatalf("an unknown MCP should not be unconfigurable: %v", err)
	}
	proj = profileWithContext("whomcp", `{"anything":[]}`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "anything", "non-empty")
}

// A context blob that is not an object at all is refused rather than read as
// "no fields" — contextValues answers nil for it, which every caller would
// then treat as an absent scope.
func TestValidatePermissions_RefusesNonObjectContext(t *testing.T) {
	proj := profileWithContext("macmcp", `["mail_accounts"]`)
	wantRefusal(t, validateProjectPermissions(proj, v2Surfaces()), "macmcp", "object")
}

// The worked example from the ADR, accepted whole.
func TestValidatePermissions_AcceptsTheWorkedExample(t *testing.T) {
	proj := &Project{
		ID: "prof_hermes_bob_inbox", Name: "Hermes — Bob INBOX (read-only)", Kind: ProjectKindRemote,
		AllowedMcpIDs: []string{"macmcp"},
		AllowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		Access:        map[string]string{"macmcp": AccessRead},
		Context: map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"mail_accounts":["Bob"],"mail_mailboxes":["INBOX"]}`),
		},
	}
	if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
		t.Fatalf("ADR-011's own worked example was refused: %v", err)
	}
	if err := validateProjectShape(proj); err != nil {
		t.Fatalf("ADR-011's own worked example failed the shape check: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The mutators
// ---------------------------------------------------------------------------

// UpdateProjectContext replaces the operator's fields and RE-DERIVES the ones
// relay owns. Without the re-derivation a local project's write_dirs would
// disappear the first time someone edited its mail scope, and the two tools
// that field governs would start refusing with nothing on screen to say why.
func TestUpdateProjectContext_ReDerivesTheProjectPathField(t *testing.T) {
	s := &Settings{Version: 1}
	surfaces := v2Surfaces()
	proj, err := s.CreateProjectWithToken("Local", "/tmp/proj", []string{"macmcp"}, nil, nil, surfaces)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := contextValues(s.Projects[0].Context["macmcp"])
	if !hasScopeValue(before, "write_dirs") {
		t.Fatalf("precondition: write_dirs should have been derived, got %s", s.Projects[0].Context["macmcp"])
	}

	s.UpdateProjectContext(proj.ID, map[string]json.RawMessage{
		"macmcp": json.RawMessage(`{"mail_accounts":["Alice"]}`),
	}, surfaces)

	after := contextValues(s.Projects[0].Context["macmcp"])
	if !hasScopeValue(after, "mail_accounts") {
		t.Errorf("operator value was not stored: %s", s.Projects[0].Context["macmcp"])
	}
	if !hasScopeValue(after, "write_dirs") {
		t.Errorf("the derived field was destroyed by an operator edit: %s", s.Projects[0].Context["macmcp"])
	}
}

// Entries for an MCP the record does not grant are dropped, exactly as
// UpdateProjectAllowedTools drops them: a mode or a scope for an unreachable
// MCP reads as an authority the record does not have.
func TestUpdateProjectAccessAndContext_DropUngrantedMcps(t *testing.T) {
	s := &Settings{Version: 1}
	proj, err := s.CreateProjectWithTokenKind(ProjectKindRemote, "Profile", "", []string{"macmcp"}, nil, nil, v2Surfaces())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.UpdateProjectAccess(proj.ID, map[string]string{"macmcp": AccessWrite, "other": AccessWrite})
	s.UpdateProjectContext(proj.ID, map[string]json.RawMessage{
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
	if got.Access["macmcp"] != AccessWrite {
		t.Errorf("granted MCP lost its mode: %#v", got.Access)
	}
}

// An unrecognised mode is KEPT rather than dropped. Dropping it would fall
// back to the default, which for a local project is write — a mutator silently
// widening a grant on the strength of a typo.
func TestUpdateProjectAccess_KeepsAnUnknownModeRatherThanWidening(t *testing.T) {
	s := &Settings{Version: 1}
	proj, err := s.CreateProjectWithToken("Local", "/tmp/proj", []string{"macmcp"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.UpdateProjectAccess(proj.ID, map[string]string{"macmcp": "wrIte"})
	if got := s.Projects[0].Access["macmcp"]; got != "wrIte" {
		t.Fatalf("mode was rewritten or dropped: %q", got)
	}
	tok := s.storedTokenForProject(&s.Projects[0], "hash")
	if mode := tok.AccessMode("macmcp"); mode != AccessRead {
		t.Errorf("an unrecognised mode must read as %q, got %q", AccessRead, mode)
	}
}
