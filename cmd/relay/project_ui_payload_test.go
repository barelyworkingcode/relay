package main

import (
	"encoding/json"
	"testing"
)

// The settings UI builds its save payload in harvestProjectForm (web/src/app.js).
// This pins the Go side against that exact JSON shape: a mismatch in a tag name
// would fail silently, since every field on projectUpdateFields is optional and
// an unrecognised key is simply ignored.
func TestRemoteProjectPayloadFromSettingsUI(t *testing.T) {
	raw := []byte(`{
		"name":"Hermes VM",
		"kind":"remote",
		"allowed_mcp_ids":[],
		"allowed_models":[],
		"permission_policy":{"default_mode":"","allowed_tools":[],"denied_tools":[]},
		"generate_skill":false,
		"allow_cwd_auth":false,
		"disabled_tools":{}
	}`)
	var f projectCreateFields
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("settings-UI payload did not decode: %v", err)
	}
	if f.Kind != ProjectKindRemote {
		t.Fatalf(`"kind":"remote" did not reach projectCreateFields.Kind (got %q) — check the json tag`, f.Kind)
	}
	if f.Path != "" {
		t.Errorf("payload with no path key produced Path=%q", f.Path)
	}

	s := &Settings{}
	created, err := applyProjectCreate(s, f, nil)
	if err != nil {
		t.Fatalf("creating a zero-grant remote project from the UI payload failed: %v", err)
	}
	if !created.IsRemote() || created.Path != "" {
		t.Fatalf("created project is not a pathless remote project: kind=%q path=%q", created.Kind, created.Path)
	}
}
