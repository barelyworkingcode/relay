package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// The manifest wire shape is duplicated across the relay-side bridge
// module and relayLLM (see ADR-001 + ROADMAP item X1). Golden a JSON
// round-trip so a field rename here breaks loudly before it can drift
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

// Validate is the schema-level gatekeeper called from bridge/server.go's
// RegisterManifest handler. Lock in the rules that matter for the
// no-carveouts contract.
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

func TestManifestValidate_EmptyRoutesRejected(t *testing.T) {
	m := Manifest{Routes: nil}
	if err := m.Validate(); err == nil {
		t.Error("empty routes must be rejected — relay can't dispatch to a service with no claims")
	}
}

// A manifest with no Config still validates — Config is optional and most
// services won't declare one. Guards the json:",omitempty" backward-compat.
func TestManifestValidate_NoConfigStillValidates(t *testing.T) {
	m := Manifest{Routes: []string{"/api/"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate with no config: %v", err)
	}
}

// Exercises every recursive node type the renderer supports: object → array of
// object, object → map of object, plus select/secret/json/string[]/stringMap
// leaves. Mirrors the shape relayLLM declares (openai/llama/pi/pty).
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
