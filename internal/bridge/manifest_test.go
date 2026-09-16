package bridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Deliberate: the manifest wire shape is duplicated in relayLLM (ADR-001) —
// this golden round-trip makes a field rename break loudly before it drifts
// from the other side.
func TestActionDecl_JSONRoundTrip_PreservesForEach(t *testing.T) {
	original := ActionDecl{
		ID:           "stop-llama",
		Label:        "Stop",
		Method:       "DELETE",
		PathTemplate: "/api/llama/instances/{alias}",
		ForEach:      "instances",
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Field name MUST be camelCase forEach — the JS-side renderer reads
	// `action.forEach`. Anything else silently breaks per-row buttons.
	if !strings.Contains(string(raw), `"forEach":"instances"`) {
		t.Errorf("forEach not present in JSON: %s", raw)
	}

	var got ActionDecl
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != original {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, original)
	}
}

func TestActionDecl_OmitsEmptyForEach(t *testing.T) {
	raw, _ := json.Marshal(ActionDecl{
		ID:           "ping",
		Label:        "Ping",
		Method:       "GET",
		PathTemplate: "/ping",
	})
	if strings.Contains(string(raw), "forEach") {
		t.Errorf("empty forEach should be omitted, got %s", raw)
	}
}

func TestManifestValidate_HappyPath(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/", "/ws"},
		Status: &StatusDecl{Path: "/api/status"},
		Actions: []ActionDecl{{
			ID: "stop", Label: "Stop", Method: "DELETE",
			PathTemplate: "/api/x/{id}", ForEach: "items",
		}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestManifestValidate_RejectsDuplicateActionID(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Actions: []ActionDecl{
			{ID: "x", Label: "X", Method: "GET", PathTemplate: "/x"},
			{ID: "x", Label: "X2", Method: "GET", PathTemplate: "/x2"},
		},
	}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicat") {
		t.Errorf("want duplicate-action error, got %v", err)
	}
}

func TestManifestValidate_RejectsUnsupportedMethod(t *testing.T) {
	m := Manifest{
		Routes:  []string{"/api/"},
		Actions: []ActionDecl{{ID: "x", Label: "X", Method: "OPTIONS", PathTemplate: "/x"}},
	}
	if err := m.Validate(); err == nil {
		t.Error("OPTIONS should not be a supported action method")
	}
}

// Deliberate: normalized in place because downstream HTTP dispatch uses
// action.Method verbatim.
func TestManifestValidate_NormalizesActionMethodCase(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Actions: []ActionDecl{
			{ID: "a", Label: "A", Method: "get", PathTemplate: "/a"},
			{ID: "b", Label: "B", Method: "Post", PathTemplate: "/b"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Actions[0].Method != http.MethodGet {
		t.Errorf("method[0] not normalized: got %q want GET", m.Actions[0].Method)
	}
	if m.Actions[1].Method != http.MethodPost {
		t.Errorf("method[1] not normalized: got %q want POST", m.Actions[1].Method)
	}
}

func TestManifestValidate_EmptyRoutesRejected(t *testing.T) {
	m := Manifest{Routes: nil}
	if err := m.Validate(); err == nil {
		t.Error("empty routes must be rejected — relay can't dispatch to a service with no claims")
	}
}

// TestManifestValidate_SessionHostRoutesRejected is C5's manifest-validation
// rule: /launch and /terminate are relay-sessions' internal API, dialed only
// by relay itself over a peer-verified connection (cmd/relay/sessionhost_client.go)
// -- never through the manifest-driven front-door proxy, which injects only
// a bearer, not a peer check. A manifest that could claim either would let
// any ordinary frontend caller reach them through the unverified proxy path
// instead.
func TestManifestValidate_SessionHostRoutesRejected(t *testing.T) {
	for _, route := range []string{"/launch", "/terminate", "/launch/", "/terminate/sub"} {
		m := Manifest{Routes: []string{"/api/terminals/", route}}
		if err := m.Validate(); err == nil {
			t.Errorf("manifest declaring route %q was accepted", route)
		}
	}
}

// A route that merely starts with the same letters as a reserved one, but
// isn't actually reserved, must still validate -- the check is route-space
// containment, not a substring match.
func TestManifestValidate_SessionHostRoutesRejected_NoOverMatch(t *testing.T) {
	m := Manifest{Routes: []string{"/launchpad/", "/terminated-sessions/"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("unrelated routes sharing a prefix with /launch or /terminate were rejected: %v", err)
	}
}

func TestManifestValidate_NoConfigStillValidates(t *testing.T) {
	m := Manifest{Routes: []string{"/api/"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate with no config: %v", err)
	}
}

// Exercises every recursive node type the renderer supports, mirroring
// relayLLM's real schema shape (openai/llama/pi/pty).
func TestManifestValidate_ConfigSchema_HappyPath(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{
			Path: "/Users/x/.config/relayLLM/settings.json", Format: ConfigFormatJSONC,
			Label: "settings.json", ApplyMode: ConfigApplyRestart,
			Schema: []FieldDecl{
				{ID: "openai", Type: FieldTypeObject, Fields: []FieldDecl{
					{ID: "endpoints", Type: FieldTypeArray, Item: &FieldDecl{
						Type: FieldTypeObject, Fields: []FieldDecl{
							{ID: "name", Type: FieldTypeText, Required: true},
							{ID: "apiKey", Type: FieldTypeSecret},
							{ID: "strict", Type: FieldTypeBool},
						}}},
				}},
				{ID: "llama-server", Type: FieldTypeObject, Fields: []FieldDecl{
					{ID: "basePort", Type: FieldTypeNumber},
					{ID: "models", Type: FieldTypeArray, Item: &FieldDecl{
						Type: FieldTypeObject, Fields: []FieldDecl{
							{ID: "alias", Type: FieldTypeText, Required: true},
							{ID: "flags", Type: FieldTypeKeyValue, Rest: true, KeyLabel: "flag"},
						}}},
				}},
				{ID: "pi", Type: FieldTypeObject, Fields: []FieldDecl{
					{ID: "autoRegenSkills", Type: FieldTypeSelect, Options: []string{"always", "never"}},
					{ID: "extraArgs", Type: FieldTypeStringArr},
				}},
				{ID: "pty", Type: FieldTypeMap, KeyLabel: "template id", Item: &FieldDecl{
					Type: FieldTypeObject, Fields: []FieldDecl{
						{ID: "name", Type: FieldTypeText, Required: true},
						{ID: "env", Type: FieldTypeStringMap},
					}}},
			},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestManifestValidate_ConfigRejectsRelativePath(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "settings.json", Schema: []FieldDecl{{ID: "x", Type: FieldTypeText}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("want absolute-path error, got %v", err)
	}
}

func TestManifestValidate_ConfigRejectsDotDotPath(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/etc/../etc/passwd", Schema: []FieldDecl{{ID: "x", Type: FieldTypeText}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "..") {
		t.Errorf("want '..'-segment error, got %v", err)
	}
}

func TestManifestValidate_ConfigRejectsEmptySchema(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json"},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "schema is empty") {
		t.Errorf("want empty-schema error, got %v", err)
	}
}

func TestManifestValidate_ConfigRejectsUnknownFieldType(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json", Schema: []FieldDecl{{ID: "x", Type: "magic-string"}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("want unsupported-type error, got %v", err)
	}
}

func TestManifestValidate_ConfigObjectRequiresFields(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json", Schema: []FieldDecl{{ID: "o", Type: FieldTypeObject}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "requires fields") {
		t.Errorf("want object-requires-fields error, got %v", err)
	}
}

func TestManifestValidate_ConfigArrayRequiresItem(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json", Schema: []FieldDecl{{ID: "a", Type: FieldTypeArray}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "requires item") {
		t.Errorf("want array-requires-item error, got %v", err)
	}
}

func TestManifestValidate_ConfigSelectRequiresOptions(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json", Schema: []FieldDecl{{ID: "s", Type: FieldTypeSelect}}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "requires options") {
		t.Errorf("want select-requires-options error, got %v", err)
	}
}

func TestManifestValidate_ConfigRejectsDuplicateSiblingIDs(t *testing.T) {
	m := Manifest{
		Routes: []string{"/api/"},
		Config: &ConfigDecl{Path: "/tmp/c.json", Schema: []FieldDecl{
			{ID: "dup", Type: FieldTypeText},
			{ID: "dup", Type: FieldTypeText},
		}},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicat") {
		t.Errorf("want duplicate-id error, got %v", err)
	}
}
