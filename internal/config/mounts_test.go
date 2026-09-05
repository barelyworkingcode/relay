package config

// JSON-shape tests for the mount grant fields added to Project and
// EnrolmentBudget (relayfs P1a): a record written before these fields
// existed must round-trip with no new key appearing in its JSON, matching
// the omitempty discipline TestSettingsJSONRoundTrip already pins for the
// rest of Settings.

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Project with no Mounts set must not gain a "mounts" key on marshal, and
// must round-trip through Settings' own seal/marshal/unmarshal/open pipeline
// with Mounts staying nil — the same discipline TestSettingsJSONRoundTrip
// pins for every other field.
func TestProjectJSON_NoMountsOmitsTheKeyAndRoundTrips(t *testing.T) {
	original := &Settings{
		Version: 1,
		Projects: []Project{
			{ID: "p1", Name: "NoMounts", AllowedMcpIDs: []string{}, AllowedModels: []string{}},
		},
	}
	if err := SealAllSecrets(original, testSealer()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"mounts"`) {
		t.Fatalf("a project with no Mounts must not carry a \"mounts\" key: %s", data)
	}

	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errs := openAllSecrets(&restored, testSealer()); len(errs) != 0 {
		t.Fatalf("open: %v", errs)
	}
	if len(restored.Projects) != 1 {
		t.Fatalf("projects: got %d, want 1", len(restored.Projects))
	}
	if restored.Projects[0].Mounts != nil {
		t.Fatalf("Mounts = %#v, want nil after a round trip with none set", restored.Projects[0].Mounts)
	}
}

// A Project WITH Mounts round-trips them faithfully — the positive control
// for the omitempty test above.
func TestProjectJSON_MountsRoundTrip(t *testing.T) {
	original := &Settings{
		Version: 1,
		Projects: []Project{
			{
				ID: "p1", Name: "Remote", Kind: ProjectKindRemote,
				AllowedMcpIDs: []string{}, AllowedModels: []string{},
				Mounts: []MountGrant{{ID: "mail", Path: "/tmp/mail", Access: AccessWrite}},
			},
		},
	}
	if err := SealAllSecrets(original, testSealer()); err != nil {
		t.Fatalf("seal: %v", err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored Settings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errs := openAllSecrets(&restored, testSealer()); len(errs) != 0 {
		t.Fatalf("open: %v", errs)
	}
	got := restored.Projects[0].Mounts
	if len(got) != 1 || got[0].ID != "mail" || got[0].Path != "/tmp/mail" || got[0].Access != AccessWrite {
		t.Fatalf("Mounts = %+v, did not round-trip the stored mount", got)
	}
}

// An EnrolmentBudget with the three mount fields left zero must not carry
// their keys either — the same "unset means absent, not zero written out"
// discipline the pre-existing budget fields already have.
func TestEnrolmentBudgetJSON_UnsetMountFieldsOmitTheirKeys(t *testing.T) {
	b := EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"mount_max_ops"`, `"mount_max_read_bytes"`, `"mount_max_write_bytes"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("an EnrolmentBudget with unset mount fields must not carry %s: %s", key, data)
		}
	}

	var restored EnrolmentBudget
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored != b {
		t.Fatalf("round trip changed the budget: got %+v, want %+v", restored, b)
	}
}

// The positive control: set mount fields DO round-trip and DO appear.
func TestEnrolmentBudgetJSON_SetMountFieldsRoundTrip(t *testing.T) {
	b := EnrolmentBudget{
		WindowSeconds: 3600, MaxCalls: 120, MaxResultBytes: 64 << 20,
		MountMaxOps: 500_000, MountMaxReadBytes: 512 << 20, MountMaxWriteBytes: 512 << 20,
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"mount_max_ops"`, `"mount_max_read_bytes"`, `"mount_max_write_bytes"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("an EnrolmentBudget with mount fields set must carry %s: %s", key, data)
		}
	}
	var restored EnrolmentBudget
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored != b {
		t.Fatalf("round trip changed the budget: got %+v, want %+v", restored, b)
	}
}
