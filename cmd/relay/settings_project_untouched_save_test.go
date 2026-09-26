package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/project"
)

// untouchedSave harvests stored unchanged and saves it through ProjectOps
// with a gate that refuses and counts every prompt.
func untouchedSave(t *testing.T, store config.SettingsStore, stored config.Project, surfaces project.McpSurfaces) (*presencetest.Recording, error) {
	t.Helper()
	msg := ctxGateHarvest(t, store, stored, surfaces, "")
	rec := presencetest.NewRecording(presence.ErrRefused)
	_, _, err := ctxGateOps(t, store, rec).Update(context.Background(), msg.ID, msg.UpdateFields,
		func() project.McpSurfaces { return surfaces }, auditViaIPC, "")
	return rec, err
}

func TestSettingsForm_UnchangedSaveWithV1McpDoesNotPrompt(t *testing.T) {
	surfaces := v2Surfaces()
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Acme", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp", "fsmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	if _, ok := decodedContext(t, store, stored.ID)["fsmcp"]; !ok {
		t.Fatal("seed did not derive the fsmcp v1 blob")
	}
	before := decodedContext(t, store, stored.ID)
	rec, err := untouchedSave(t, store, stored, surfaces)
	if err != nil || rec.Calls() != 0 {
		t.Fatalf("unchanged save: err = %v, prompts = %d; want success with none", err, rec.Calls())
	}
	if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("stored context changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSettingsForm_UnchangedSaveKeepsOddStoredValues(t *testing.T) {
	store, stored := untouchedLocalAcme(t, `{"acme_accounts":[" Alice "],"acme_owner":"  Carol ","acme_limit":"123"}`)
	before := decodedContext(t, store, stored.ID)
	rec, err := untouchedSave(t, store, stored, untouchedAcmeSurfaces())
	if err != nil || rec.Calls() != 0 {
		t.Fatalf("unchanged save: err = %v, prompts = %d; want success with none", err, rec.Calls())
	}
	if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("stored context changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSettingsForm_UnchangedSaveRefusesAnInvalidStoredValueByName(t *testing.T) {
	rows := []struct{ name, stored string }{
		{"null", `{"acme_accounts":null}`},
		{"string in an array field", `{"acme_accounts":"Alice"}`},
		{"empty element", `{"acme_accounts":["Alice",""]}`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			store, stored := untouchedLocalAcme(t, r.stored)
			before := decodedContext(t, store, stored.ID)
			rec, err := untouchedSave(t, store, stored, untouchedAcmeSurfaces())
			if rec.Calls() != 0 {
				t.Fatalf("an untouched value raised %d prompts", rec.Calls())
			}
			if err == nil {
				t.Fatal("an invalid stored value was saved")
			}
			msg := strings.ToLower(err.Error())
			for _, want := range []string{"acmemcp", "acme_accounts", "clear"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("refused save changed the stored context:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}
