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
