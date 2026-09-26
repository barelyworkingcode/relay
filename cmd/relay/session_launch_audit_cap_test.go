package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/control"
)

const auditCapMarker = "…"

// hugeAuditValue is 1 MiB of mixed one- and two-byte runes, so a byte-wise
// cut would leave invalid UTF-8 behind.
func hugeAuditValue() string {
	return strings.Repeat("aé", (1<<20)/3)
}

func firstRunes(s string, n int) string {
	return string([]rune(s)[:n])
}

func refusalSessionKind(t *testing.T, refusal *LaunchRefusal) string {
	t.Helper()
	var args struct {
		SessionKind string `json:"session_kind"`
	}
	if err := json.Unmarshal(refusal.Audit.Args, &args); err != nil {
		t.Fatalf("unmarshal audit args (len %d): %v", len(refusal.Audit.Args), err)
	}
	return args.SessionKind
}

func TestAuthorizeLaunch_RefusalAuditRowBounded(t *testing.T) {
	store := newLaunchTestStore(t)
	sessions := newLaunchTestLedger(t)
	proj := addLaunchTestProject(t, store, nil)
	huge := hugeAuditValue()
	execute := bearerCaller(control.ClassExecute)

	for _, tc := range []struct {
		name        string
		req         LaunchRequest
		code        string
		errorCapped bool
		wantKind    string
		wantProject string
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
			if len(row) >= 4<<10 {
				t.Errorf("audit row is %d bytes, want < 4096", len(row))
			}

			errText := refusal.Audit.Error
			if !utf8.ValidString(errText) {
				t.Errorf("audit error is not valid UTF-8")
			}
			if n := utf8.RuneCountInString(strings.TrimSuffix(errText, auditCapMarker)); n > 256 {
				t.Errorf("audit error has %d runes before the marker, want <= 256", n)
			}
			if tc.errorCapped && !strings.HasSuffix(errText, auditCapMarker) {
				t.Errorf("capped audit error (len %d) does not end in %q", len(errText), auditCapMarker)
			}

			if kind := refusalSessionKind(t, refusal); kind != tc.wantKind {
				t.Errorf("args.session_kind: len %d, want len %d (first 64 runes + marker when capped)", len(kind), len(tc.wantKind))
			}
			if pid := refusal.Audit.Actor.ProjectID; pid != tc.wantProject {
				t.Errorf("actor.project_id: len %d, want len %d (first 64 runes + marker when capped)", len(pid), len(tc.wantProject))
			}
		})
	}

	t.Run("short values pass through", func(t *testing.T) {
		req := LaunchRequest{Caller: execute, ProjectID: proj.ID, Kind: KindPTY, TemplateID: "nope"}
		_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)
		if refusal == nil || refusal.Code != "template_not_allowed" {
			t.Fatalf("want refusal template_not_allowed, got %+v", refusal)
		}
		if got := refusal.Audit.Error; got != refusal.Message || !strings.Contains(got, `"nope"`) || strings.Contains(got, auditCapMarker) {
			t.Errorf("audit error = %q, want the unclipped refusal message %q", got, refusal.Message)
		}
		if kind := refusalSessionKind(t, refusal); kind != KindPTY {
			t.Errorf("args.session_kind = %q, want %q", kind, KindPTY)
		}
		if pid := refusal.Audit.Actor.ProjectID; pid != proj.ID {
			t.Errorf("actor.project_id = %q, want %q", pid, proj.ID)
		}
	})
}
