package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/control"
)

const auditCapMarker = "…"

// hugeAuditValue is 1 MiB of mixed one- and two-byte runes, so a cut counted
// in bytes keeps fewer runes than one counted in runes.
func hugeAuditValue() string {
	return strings.Repeat("aé", (1<<20)/3)
}

func firstRunes(s string, n int) string {
	return string([]rune(s)[:n])
}

// hugeAuditDirectory is a 512 KiB path of a non-existent leaf under root, in
// a few long segments of one-, two- and three-byte runes.
func hugeAuditDirectory(root string) string {
	segs := []string{root}
	for i := 0; i < 8; i++ {
		segs = append(segs, strings.Repeat("é日", (64<<10)/5))
	}
	return filepath.Join(segs...)
}

type refusalAuditArgs struct {
	SessionKind string `json:"session_kind"`
	Directory   string `json:"directory"`
	// DirectoryTruncated is false when the key is absent.
	DirectoryTruncated bool `json:"directory_truncated"`
}

func refusalArgs(t *testing.T, refusal *LaunchRefusal) refusalAuditArgs {
	t.Helper()
	var args refusalAuditArgs
	if err := json.Unmarshal(refusal.Audit.Args, &args); err != nil {
		t.Fatalf("unmarshal audit args (len %d): %v", len(refusal.Audit.Args), err)
	}
	return args
}

func TestAuthorizeLaunch_RefusalAuditRowBounded(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	huge := hugeAuditValue()
	execute := bearerCaller(control.ClassExecute)
	// The symlink-resolved root makes a request directory under it its own
	// resolved form, however the launch resolves it.
	root, err := filepath.EvalSymlinks(proj.Path)
	if err != nil {
		t.Fatalf("resolve project root: %v", err)
	}
	hugeDir := hugeAuditDirectory(root)

	for _, tc := range []struct {
		name          string
		req           LaunchRequest
		code          string
		errorCapped   bool
		wantKind      string
		wantProject   string
		wantDirectory string
		wantDirCapped bool
		rowLimit      int
	}{
		{
			name:        "template id",
			req:         LaunchRequest{Caller: execute, ProjectID: proj.ID, Kind: KindPTY, TemplateID: huge},
			code:        "template_not_allowed",
			errorCapped: true,
			wantKind:    KindPTY,
			wantProject: proj.ID,
		},
		{
			name:        "kind",
			req:         LaunchRequest{Caller: execute, ProjectID: proj.ID, Kind: huge},
			code:        "invalid_kind",
			errorCapped: true,
			wantKind:    firstRunes(huge, 64) + auditCapMarker,
			wantProject: proj.ID,
		},
		{
			name:        "project id",
			req:         LaunchRequest{Caller: execute, ProjectID: huge, Kind: KindPTY, TemplateID: "shell"},
			code:        "project_not_available",
			wantKind:    KindPTY,
			wantProject: firstRunes(huge, 64) + auditCapMarker,
		},
		{
			name:          "directory",
			req:           LaunchRequest{Caller: execute, ProjectID: proj.ID, Kind: KindPTY, TemplateID: "nope", Directory: hugeDir},
			code:          "template_not_allowed",
			wantKind:      KindPTY,
			wantProject:   proj.ID,
			wantDirectory: firstRunes(hugeDir, 1024) + auditCapMarker,
			wantDirCapped: true,
			rowLimit:      8 << 10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, tc.req)
			if refusal == nil {
				t.Fatalf("want refusal %s, got none", tc.code)
			}
			if refusal.Code != tc.code {
				t.Fatalf("refusal code = %q, want %s", refusal.Code, tc.code)
			}
			row, err := json.Marshal(refusal.Audit)
			if err != nil {
				t.Fatalf("marshal audit event: %v", err)
			}
			limit := tc.rowLimit
			if limit == 0 {
				limit = 4 << 10
			}
			if len(row) >= limit {
				t.Errorf("audit row is %d bytes, want < %d", len(row), limit)
			}

			errText := refusal.Audit.Error
			if !utf8.ValidString(errText) {
				t.Errorf("audit error is not valid UTF-8")
			}
			if tc.errorCapped {
				if !strings.HasSuffix(errText, auditCapMarker) {
					t.Errorf("capped audit error (len %d) does not end in %q", len(errText), auditCapMarker)
				}
				if n := utf8.RuneCountInString(strings.TrimSuffix(errText, auditCapMarker)); n != 256 {
					t.Errorf("capped audit error has %d runes before the marker, want 256", n)
				}
			} else if strings.Contains(errText, auditCapMarker) {
				t.Errorf("uncapped audit error (len %d) carries the marker", len(errText))
			}

			args := refusalArgs(t, refusal)
			if args.SessionKind != tc.wantKind {
				t.Errorf("args.session_kind: len %d, want len %d (first 64 runes + marker when capped)", len(args.SessionKind), len(tc.wantKind))
			}
			if tc.wantDirectory != "" && args.Directory != tc.wantDirectory {
				t.Errorf("args.directory: len %d (%d runes), want len %d (first 1024 runes + marker)",
					len(args.Directory), utf8.RuneCountInString(args.Directory), len(tc.wantDirectory))
			}
			if args.DirectoryTruncated != tc.wantDirCapped {
				t.Errorf("args.directory_truncated = %v, want %v", args.DirectoryTruncated, tc.wantDirCapped)
			}
			if pid := refusal.Audit.Actor.ProjectID; pid != tc.wantProject {
				t.Errorf("actor.project_id: len %d, want len %d (first 64 runes + marker when capped)", len(pid), len(tc.wantProject))
			}
		})
	}

	t.Run("short values pass through", func(t *testing.T) {
		dir := filepath.Join(root, "sub")
		req := LaunchRequest{Caller: execute, ProjectID: proj.ID, Kind: KindPTY, TemplateID: "nope", Directory: dir}
		_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
		if refusal == nil || refusal.Code != "template_not_allowed" {
			t.Fatalf("want refusal template_not_allowed, got %+v", refusal)
		}
		if got := refusal.Audit.Error; got != refusal.Message || !strings.Contains(got, `"nope"`) || strings.Contains(got, auditCapMarker) {
			t.Errorf("audit error = %q, want the unclipped refusal message %q", got, refusal.Message)
		}
		args := refusalArgs(t, refusal)
		if args.SessionKind != KindPTY {
			t.Errorf("args.session_kind = %q, want %q", args.SessionKind, KindPTY)
		}
		if args.Directory != dir {
			t.Errorf("args.directory = %q, want %q", args.Directory, dir)
		}
		if args.DirectoryTruncated {
			t.Errorf("args.directory_truncated = true for an uncapped directory")
		}
		if pid := refusal.Audit.Actor.ProjectID; pid != proj.ID {
			t.Errorf("actor.project_id = %q, want %q", pid, proj.ID)
		}
	})
}
