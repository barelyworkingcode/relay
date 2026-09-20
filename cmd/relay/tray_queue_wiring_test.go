package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrayAppWiresEnrolmentAndEveOpsToTheSharedQueue(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	for _, want := range []string{
		"enrolmentOps := &EnrolmentOps{\n\t\tStore: store,\n\t\tQueue: serviceQueue,",
		"eveEnrolmentOps := &EveEnrolmentOps{\n\t\tStore: store,\n\t\tQueue: serviceQueue,",
		"evePasskeyOps := &EvePasskeyOps{\n\t\tStore: store,\n\t\tQueue: serviceQueue,",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("tray construction no longer shares the command queue:\n%s", want)
		}
	}
}

func TestTrayAppWiresTemplateOpsToTheSharedQueueAndBothDoors(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	for _, want := range []string{
		"templateOps := &TemplateOps{Store: store, Queue: serviceQueue}",
		"app.ipcCtx.TemplateOps = templateOps",
		"hostOps, templateOps, eveEnrolmentOps",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("tray construction no longer shares the template core and queue:\n%s", want)
		}
	}
}

func TestTrayAppWiresLoginRoutesToTheSharedQueue(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	if want := "frontend.routeDeps.loginOps = loginOps"; !strings.Contains(string(source), want) {
		t.Fatalf("the login routes no longer share the login core and its command queue:\n%s", want)
	}
}

func TestTrayAppRoutesOAuthRefreshPersistenceThroughMcpOps(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	for _, want := range []string{
		"mcpOps.PersistOAuthState(mcpID, oauth)",
		"mcpOps = &McpOps{\n\t\tStore:           store,\n\t\tCtx:             ctx,\n\t\tQueue:           serviceQueue,",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("the OAuth refresh callback no longer persists through the queued McpOps:\n%s", want)
		}
	}
}
