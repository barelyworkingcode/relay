package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

const refusedDenyWarning = "session sandbox: deny refused"

func TestAuthorizeLaunch_RefusesDenyThroughUserLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory, so no link is in a locked one")
	}
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := sandboxRealPath(t, t.TempDir())
	t.Setenv("HOME", home)
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
			ID: "custom", Name: "Custom", Sandbox: ptr(true), ReadWrite: []string{"~"}, Deny: []string{"~/.config/gh"},
		})
	}); err != nil {
		t.Fatalf("store.With: %v", err)
	}
	elsewhere := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "gh"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(home, ".config")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	deny := filepath.Join(link, "gh")
	logs := captureWarnRecords(t)

	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "custom",
	})
	if refusal == nil {
		t.Fatalf("launch %s was authorized with a deny through %s", result.SessionID, link)
	}
	if refusal.Status != 400 || refusal.Code != "sandbox_unavailable" {
		t.Errorf("refusal = %d %q, want 400 sandbox_unavailable", refusal.Status, refusal.Code)
	}
	if want := `deny "` + deny + `" follows symlink "` + link + `"`; !strings.Contains(refusal.Message, want) {
		t.Errorf("refusal message %q does not name the deny and the link (%s)", refusal.Message, want)
	}
	if refusal.Audit.Event != audit.AuditEventSessionLaunch || refusal.Audit.Outcome != audit.AuditOutcomeError {
		t.Errorf("audit = %s/%s, want %s/%s", refusal.Audit.Event, refusal.Audit.Outcome, audit.AuditEventSessionLaunch, audit.AuditOutcomeError)
	}

	var args sessionLaunchAuditArgs
	if err := json.Unmarshal(refusal.Audit.Args, &args); err != nil {
		t.Fatalf("audit args: %v", err)
	}
	warns := recordsWithMessage(t, logs, refusedDenyWarning)
	if len(warns) != 1 {
		t.Fatalf("got %d %q records, want exactly one:\n%s", len(warns), refusedDenyWarning, logs.String())
	}
	w := warns[0]
	if w["level"] != "WARN" || w["deny"] != deny || w["link"] != link || w["kind"] != KindPTY || w["session"] != args.SessionID || args.SessionID == "" {
		t.Errorf("warn record = %v, want level WARN, deny %s, link %s, kind %s, session %q", w, deny, link, KindPTY, args.SessionID)
	}

	entries, err := os.ReadDir(sessionProfilesDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read profiles dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused launch left %d profile(s) behind", len(entries))
	}
}
