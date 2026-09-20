package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestIPCUpdateProjectDisabledToolsWaitsBehindQueuedCommandAndLandsInOrder(t *testing.T) {
	ipc, store, ui, _ := newProjectsIPC(t)
	queue := newLoginQueue(t, 4)
	ipc.ProjectOps.Queue = queue
	proj := createTestProject(t, store, "Alpha", t.TempDir(), []string{"fsmcp", "macmcp"})

	releaseBlocker := queueBlocker(t, queue)
	raw := mustRaw(t, ipcProjectDisabledToolsMsg{ID: proj.ID, McpID: "macmcp", Disabled: []string{"runScript"}})
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		ipcUpdateProjectDisabledTools(ipc, raw)
	}()
	waitForMutationAdmission(t, queue)
	if persisted, _ := config.FindProjectByID(store.Get(), proj.ID); len(persisted.DisabledTools["macmcp"]) != 0 {
		t.Fatalf("disabled tools persisted before the queue admitted the write: %v", persisted.DisabledTools)
	}

	seen := make(chan []string, 1)
	go func() {
		_ = queue.Do(context.Background(), func(context.Context) error {
			p, _ := config.FindProjectByID(store.Get(), proj.ID)
			seen <- p.DisabledTools["macmcp"]
			return nil
		})
	}()
	waitForPending(t, queue, 2)
	releaseBlocker()
	<-handled

	if got := <-seen; !reflect.DeepEqual(got, []string{"runScript"}) {
		t.Fatalf("a command queued after the write saw %v, want [runScript]", got)
	}
	if _, ok := findEvent(ui, "onProjectUpdated"); !ok {
		t.Fatal("expected onProjectUpdated once the write landed")
	}
}

func TestProjectOpsSetDisabledToolsReportsMissingProject(t *testing.T) {
	ipc, _, _, _ := newProjectsIPC(t)
	_, found, err := ipc.ProjectOps.SetDisabledTools(context.Background(), "no-such", "macmcp", []string{"x"})
	assertNoErr(t, err, "SetDisabledTools")
	if found {
		t.Fatal("found = true for a project that does not exist")
	}
}
