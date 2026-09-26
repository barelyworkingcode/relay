package main

import (
	"context"
	"errors"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

func pudOps(t *testing.T, store config.SettingsStore) *ProjectOps {
	t.Helper()
	return &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
}

func pudNoSurfaces() project.McpSurfaces { return project.McpSurfaces{} }

// A refusal from the update's own validation must decline the settings write,
// not commit the unchanged settings back over the file.
func TestProjectUpdateThatIsRefusedWritesNothing(t *testing.T) {
	badMode := config.ProjectMode("sometimes")
	badAccess := map[string]string{"keeper": "admin"}
	cases := []struct {
		name   string
		fields project.UpdateFields
	}{
		{name: "invalid mode", fields: project.UpdateFields{Mode: &badMode}},
		{name: "invalid access", fields: project.UpdateFields{Access: &badAccess}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := odwSandbox(t)
			stored := mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
			before := odwSnap(t, dir)

			_, found, err := pudOps(t, store).Update(context.Background(), stored.ID, tc.fields, pudNoSurfaces, auditViaIPC, "")
			if err == nil {
				t.Fatal("Update accepted an invalid field; want a refusal")
			}
			if !found {
				t.Fatalf("Update reported the stored project as missing (err = %v); want found with a refusal", err)
			}
			if errors.Is(err, errProjectSaveFailed) {
				t.Fatalf("Update reported a validation refusal as a save failure: %v", err)
			}
			before.assertUntouched(t, dir, "ProjectOps.Update ("+tc.name+")")
		})
	}
}

func TestProjectUpdateOfAMissingProjectWritesNothing(t *testing.T) {
	dir, store := odwSandbox(t)
	mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
	before := odwSnap(t, dir)

	name := "Renamed"
	got, found, err := pudOps(t, store).Update(context.Background(), "ghost", project.UpdateFields{Name: &name}, pudNoSurfaces, auditViaIPC, "")
	if err != nil {
		t.Fatalf("Update of an unknown id returned error %v; want none", err)
	}
	if found {
		t.Fatalf("Update of an unknown id reported found (returned %+v)", got)
	}
	before.assertUntouched(t, dir, "ProjectOps.Update (unknown id)")
}

// Declining is for refusals only: an accepted update still reaches the file.
func TestProjectUpdateThatIsValidStillWrites(t *testing.T) {
	dir, store := odwSandbox(t)
	stored := mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
	before := odwSnap(t, dir)

	name := "Acme Renamed"
	got, found, err := pudOps(t, store).Update(context.Background(), stored.ID, project.UpdateFields{Name: &name}, pudNoSurfaces, auditViaIPC, "")
	if err != nil || !found {
		t.Fatalf("Update = (found %v, err %v); want found with no error", found, err)
	}
	if got.ID != stored.ID || got.Name != name {
		t.Fatalf("Update returned project %q named %q; want %q named %q", got.ID, got.Name, stored.ID, name)
	}
	if string(sdRead(t, dir)) == string(before.bytes) {
		t.Fatal("a valid Update left settings.json unchanged")
	}
	onDisk, _ := config.FindProjectByID(sealedSettingsStoreAt(dir).Get(), stored.ID)
	if onDisk == nil || onDisk.Name != name {
		t.Fatalf("settings.json after a valid Update holds %+v; want the project named %q", onDisk, name)
	}
}
