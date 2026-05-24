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
