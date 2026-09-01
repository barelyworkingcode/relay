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
// (NewSettingsStore/NewSettingsStoreAt leave the sealer nil — §5.4), so
// whether THIS machine's keychain happens to hold the key settings.json
// names is not a question either command ever asks. Proven directly rather
// than assumed: seed settings.json fully sealed under a real key, then run
// both CLI paths against it with no keyring in the picture whatsoever.
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

	// relay grant --json: a CLI-shaped store (nil sealer) reads the clear
	// fields fine and never calls Reveal (AC-3) — it must succeed and name
	// the project by its clear `name` field regardless of any keychain state.
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
