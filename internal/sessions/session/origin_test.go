package session_test

import (
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const cos = sessionstypes.OriginChiefOfStaff

func msg(role, text, origin string) sessionstypes.Message {
	content, _ := json.Marshal(text)
	return sessionstypes.Message{Role: role, Content: content, Origin: origin}
}

func TestSendMessageAs_RecordsOriginAndItSurvivesReload(t *testing.T) {
	mgr, store := newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))
	const id = "11111111-1111-1111-1111-111111111111"
	sess, err := mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "p1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.SendMessageAs(id, "from the chief", nil, cos); err != nil {
		t.Fatalf("SendMessageAs: %v", err)
	}
	sess.SetProcessing(false)
	if err := mgr.SendMessage(id, "from the person", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("reloaded %d messages, want 2: %+v", len(loaded.Messages), loaded.Messages)
	}
	if loaded.Messages[0].Origin != cos {
		t.Errorf("reloaded Chief of Staff message origin = %q, want %q", loaded.Messages[0].Origin, cos)
	}
	if loaded.Messages[1].Origin != "" {
		t.Errorf("reloaded person message origin = %q, want none", loaded.Messages[1].Origin)
	}
}

func TestMarkOrigins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []sessionstypes.Message
		own     []sessionstypes.Message
		want    []string // origin per history entry
	}{
		{
			name: "same text sent by the person then by the chief of staff",
			history: []sessionstypes.Message{
				msg("user", "go on", ""), msg("assistant", "ok", ""),
				msg("user", "go on", ""), msg("assistant", "ok again", ""),
			},
			own: []sessionstypes.Message{
				msg("user", "go on", ""), msg("assistant", "ok", ""),
				msg("user", "go on", cos), msg("assistant", "ok again", ""),
			},
			want: []string{"", "", cos, ""},
		},
		{
			name: "a failed send that never reached the transcript",
			history: []sessionstypes.Message{
				msg("user", "something else", ""),
			},
			own: []sessionstypes.Message{
				msg("user", "lost instruction", cos),
				msg("user", "something else", ""),
			},
			want: []string{""},
		},
		{
			name: "transcript entries the session never recorded do not shift later marks",
			history: []sessionstypes.Message{
				msg("user", "carried over from before", ""),
				msg("user", "restart the job", ""),
				msg("user", "and report", ""),
			},
			own: []sessionstypes.Message{
				msg("user", "restart the job", cos),
				msg("user", "and report", ""),
			},
			want: []string{"", cos, ""},
		},
		{
			name: "only user turns are marked",
			history: []sessionstypes.Message{
				msg("user", "ping", ""), msg("assistant", "ping", ""),
			},
			own: []sessionstypes.Message{
				msg("user", "ping", cos), msg("assistant", "ping", ""),
			},
			want: []string{cos, ""},
		},
		{
			name:    "no own messages leaves the transcript unmarked",
			history: []sessionstypes.Message{msg("user", "hi", "")},
			own:     nil,
			want:    []string{""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.history)
			got := session.MarkOrigins(tc.history, tc.own)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d messages, want %d", len(got), len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Origin != w {
					t.Errorf("entry %d origin = %q, want %q", i, got[i].Origin, w)
				}
			}
			if after, _ := json.Marshal(tc.history); string(after) != string(before) {
				t.Errorf("MarkOrigins modified its history input")
			}
		})
	}
}
