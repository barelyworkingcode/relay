package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrayAppWiresOpsToTheSharedQueue(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(relaySourceDir(t), "trayapp.go"))
	assertNoErr(t, err, "read trayapp.go")
	source := string(raw)
	for _, tc := range []struct {
		name    string
		message string
		pinned  []string
	}{
		{
			name:    "enrolment and eve",
			message: "tray construction no longer shares the command queue",
			pinned: []string{
				"enrolmentOps := &EnrolmentOps{\n\t\tStore: store,\n\t\tQueue: serviceQueue,",
				"eveEnrolmentOps := &EveEnrolmentOps{\n\t\tStore:  store,\n\t\tQueue:  serviceQueue,",
				"evePasskeyOps := &EvePasskeyOps{\n\t\tStore:  store,\n\t\tQueue:  serviceQueue,",
			},
		},
		{
			name:    "templates and both doors",
			message: "tray construction no longer shares the template core and queue",
			pinned: []string{
				"templateOps := &TemplateOps{Store: store, Queue: serviceQueue}",
				"app.ipcCtx.TemplateOps = templateOps",
				"hostOps, templateOps, eveEnrolmentOps",
			},
		},
		{
			name:    "login routes",
			message: "the login routes no longer share the login core and its command queue",
			pinned:  []string{"frontend.routeDeps.loginOps = loginOps"},
		},
		{
			name:    "oauth refresh through mcp ops",
			message: "the OAuth refresh callback no longer persists through the queued McpOps",
			pinned: []string{
				"mcpOps.PersistOAuthState(mcpID, oauth)",
				"mcpOps = &McpOps{\n\t\tStore:           store,\n\t\tCtx:             ctx,\n\t\tQueue:           serviceQueue,",
			},
		},
		{
			name:    "service config saves",
			message: "service config saves no longer run through the queued ServiceOps",
			pinned: []string{
				"Enhanced: enhancedRegistry,\n\t\tQueue:    serviceQueue,",
				"Ops:                    serviceOps,",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.pinned {
				if !strings.Contains(source, want) {
					t.Fatalf("%s:\n%s", tc.message, want)
				}
			}
		})
	}
}
