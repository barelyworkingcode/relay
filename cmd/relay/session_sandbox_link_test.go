package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

const refusedGrantWarning = "session sandbox: read-write grant refused"

// captureWarnRecords sends the default logger's Warn-and-above records, one
// JSON object per line, into a buffer for the duration of one test.
func captureWarnRecords(t *testing.T) *lrSyncBuffer {
	t.Helper()
	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func recordsWithMessage(t *testing.T, logs *lrSyncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestAuthorizeLaunch_RefusesReadWriteGrantThroughUserLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory, so no link is in a locked one")
	}
	for _, tc := range []struct {
		name, entry string
		plant       func(t *testing.T, home string)
	}{
		{"directory entry replaced by a link to a sibling", "~/data", func(t *testing.T, home string) {
			sibling := filepath.Join(home, "sibling")
			if err := os.Mkdir(sibling, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Symlink(sibling, filepath.Join(home, "data")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}},
		{"file entry replaced by a link to a directory", "~/.claude.json", func(t *testing.T, home string) {
			elsewhere := filepath.Join(home, "elsewhere")
			if err := os.Mkdir(elsewhere, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(home, ".claude.json")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noDeveloperTools(t)
			store := newLaunchTestStore(t)
			proj := addLaunchTestProject(t, store, nil)
			home := sandboxRealPath(t, t.TempDir())
			t.Setenv("HOME", home)
			if err := store.With(func(s *config.Settings) {
				s.TerminalTemplates = append(s.TerminalTemplates, config.TerminalTemplate{
					ID: "custom", Name: "Custom", Sandbox: ptr(true), ReadWrite: []string{tc.entry},
				})
			}); err != nil {
				t.Fatalf("store.With: %v", err)
			}
			tc.plant(t, home)
			link := filepath.Join(home, strings.TrimPrefix(tc.entry, "~/"))
			logs := captureWarnRecords(t)

			result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
				Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindPTY, TemplateID: "custom",
			})
			if refusal == nil {
				t.Fatalf("launch %s was authorized with a read-write grant through %s", result.SessionID, link)
			}
			if refusal.Status != 400 || refusal.Code != "sandbox_unavailable" {
				t.Errorf("refusal = %d %q, want 400 sandbox_unavailable", refusal.Status, refusal.Code)
			}
			if want := `grant "` + link + `" follows symlink "` + link + `"`; !strings.Contains(refusal.Message, want) {
				t.Errorf("refusal message %q does not name the grant and the link (%s)", refusal.Message, want)
			}
			if refusal.Audit.Event != audit.AuditEventSessionLaunch || refusal.Audit.Outcome != audit.AuditOutcomeError {
				t.Errorf("audit = %s/%s, want %s/%s", refusal.Audit.Event, refusal.Audit.Outcome, audit.AuditEventSessionLaunch, audit.AuditOutcomeError)
			}

			var args sessionLaunchAuditArgs
			if err := json.Unmarshal(refusal.Audit.Args, &args); err != nil {
				t.Fatalf("audit args: %v", err)
			}
			warns := recordsWithMessage(t, logs, refusedGrantWarning)
			if len(warns) != 1 {
				t.Fatalf("got %d %q records, want exactly one:\n%s", len(warns), refusedGrantWarning, logs.String())
			}
			w := warns[0]
			if w["level"] != "WARN" || w["grant"] != link || w["link"] != link || w["kind"] != KindPTY || w["session"] != args.SessionID || args.SessionID == "" {
				t.Errorf("warn record = %v, want level WARN, grant and link %s, kind %s, session %q", w, link, KindPTY, args.SessionID)
			}

			entries, err := os.ReadDir(sessionProfilesDir())
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read profiles dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("a refused launch left %d profile(s) behind", len(entries))
			}
		})
	}
}
