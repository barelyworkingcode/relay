package main

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// TestGrantAndAudit_UnaffectedByAKeyTheCLINeverHas is the CLI half of
// §5.6's "the read half works in full" and the testing checklist's
// "relay grant and relay audit still work while degraded": `relay grant`
// and `relay audit` never construct a keychain-backed store at all
// (NewSettingsStore/NewSettingsStoreAt leave the sealer nil — §5.4): grant
// is answered by the tray, which holds the key, and only its clear-field
// views cross the bridge; audit reads its own log. Proven directly rather
// than assumed: seed settings.json fully sealed under a real key, serve it
// from a tray router, then run both CLI paths with no keyring in the CLI
// process whatsoever.
func TestGrantAndAudit_UnaffectedByAKeyTheCLINeverHas(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	sealed := sealedSettingsStoreAt(dir)
	assertNoErr(t, sealed.EnsureInitialized(), "EnsureInitialized")
	assertNoErr(t, sealed.With(func(s *config.Settings) {
		hash := config.HashToken("grant-audit-token")
		s.Projects = append(s.Projects, config.Project{
			ID: "gaproj", Name: "Grant Audit Project", Path: t.TempDir(),
			Token: config.NewSecret("grant-audit-token"), TokenHash: hash,
			AllowedMcpIDs: []string{"*"},
		})
	}), "seed a project")

	serveBroker(t, newBrokerRouter(t, sealed, nil))

	// relay grant --json: the CLI process holds no sealer and never calls
	// Reveal (AC-3) — it must succeed and name the project by its clear
	// `name` field regardless of any keychain state.
	out := captureStdout(t, func() { runGrantCommand([]string{"--json"}) })
	var views []map[string]any
	assertNoErr(t, json.Unmarshal([]byte(out), &views), "unmarshal grant --json output")
	if len(views) != 1 || views[0]["name"] != "Grant Audit Project" {
		t.Fatalf("relay grant --json did not surface the seeded project: %s", out)
	}

	// relay audit --path: proves the audit command runs to completion
	// against this same sealed settings.json without ever touching a
	// keyring — it only needs the audit config's clear fields.
	pathOut := captureStdout(t, func() { runAuditCommand([]string{"--path"}) })
	if strings.TrimSpace(pathOut) == "" {
		t.Fatal("relay audit --path printed nothing")
	}
}
