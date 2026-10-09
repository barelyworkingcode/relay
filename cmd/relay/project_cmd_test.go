package main

import (
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func filesReadOnlyOf(t *testing.T, store config.SettingsStore, id string) bool {
	t.Helper()
	p, _ := config.FindProjectByID(store.Get(), id)
	if p == nil {
		t.Fatalf("project %s not found", id)
	}
	return p.FilesReadOnly
}

func TestProjectUpdate_FilesReadOnlySetsAndClearsTheFlag(t *testing.T) {
	store := newCLISandboxStore(t)
	proj := mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
	other := mkStoreProject(t, store, config.ProjectKindLocal, "Other", t.TempDir())
	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.projectOps = &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	}))

	aiQuiet(t, func() { projectUpdate([]string{"--id", proj.ID, "--files-read-only=true"}) })
	if !filesReadOnlyOf(t, store, proj.ID) {
		t.Fatal("--files-read-only=true did not set the flag")
	}
	if filesReadOnlyOf(t, store, other.ID) {
		t.Error("the flag reached a project that was not named")
	}

	aiQuiet(t, func() { projectUpdate([]string{"--id", proj.ID, "--files-read-only=false"}) })
	if filesReadOnlyOf(t, store, proj.ID) {
		t.Fatal("--files-read-only=false did not clear the flag")
	}
}

func TestProjectUpdate_RefusesBadArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"bad value":  {[]string{"project", "update", "--id", "p1", "--files-read-only=maybe"}, "files-read-only"},
		"missing id": {[]string{"project", "update", "--files-read-only=true"}, "--id"},
	} {
		t.Run(name, func(t *testing.T) {
			out, code := runCLISubprocess(t, mkShortTempDir(t, "relay-project-"), tc.args...)
			if code == 0 {
				t.Fatalf("exited 0; output:\n%s", out)
			}
			// The refusal names the argument, so it is not the "relay is not
			// running" refusal that every brokered command shares.
			if !strings.Contains(out, tc.want) || strings.Contains(out, "requires the service") {
				t.Errorf("output %q should name %q and not be the stopped-service refusal", out, tc.want)
			}
		})
	}
}
