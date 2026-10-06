package provider

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const restoredConversationID = "7d3f1c52-9a4e-4b6a-8c21-0e5f6a7b8c9d"

var remoteBlob = regexp.MustCompile(`printf %s ([A-Za-z0-9+/=]+) \|`)

var sessionIDFlag = regexp.MustCompile(`--session-id ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// A fresh conversation is launched with a pinned id so relay knows it before
// the first turn; a restored one resumes. Never both. Local and host argv agree.
func TestClaudeStart_PinsSessionIDOnlyForFreshConversation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host     bool
		restored bool
	}{
		{"fresh local", false, false},
		{"restored local", false, true},
		{"fresh host", true, false},
		{"restored host", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			script := writeEnvArgvDumpScript(t, t.TempDir())
			argvOut := filepath.Join(scratch, "argv.out")
			t.Setenv("RH_TEST_OUT_ENV", filepath.Join(scratch, "env.out"))
			t.Setenv("RH_TEST_OUT_ARGV", argvOut)

			sess := &sessionstypes.Session{ID: "p1", Model: "sonnet", Directory: t.TempDir()}
			cfg := ClaudeConfig{Binary: script}
			if tc.host {
				sess.Host = &sessionstypes.HostSpec{ID: "h1", Name: "testbox", SSHArgv: []string{script}, ClaudePath: "/remote/bin/claude"}
			}
			p := NewClaudeProvider(sess, func(string, json.RawMessage) {}, cfg, nil)
			defer p.Kill()
			if tc.restored {
				p.RestoreState(json.RawMessage(`{"claudeSessionId":"` + restoredConversationID + `"}`))
			}
			if err := p.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitForFile(t, argvOut)
			flat := strings.Join(readLines(t, argvOut), " ")
			if m := remoteBlob.FindStringSubmatch(flat); tc.host && m != nil {
				// Host argv carries claude's flags inside one encoded remote command.
				raw, err := base64.StdEncoding.DecodeString(m[1])
				if err != nil {
					t.Fatalf("decode remote command: %v", err)
				}
				flat = string(raw)
			} else if tc.host {
				t.Fatalf("no remote command in host argv: %s", flat)
			}
			flat = strings.ReplaceAll(flat, "'", "")

			if tc.restored {
				if !strings.Contains(flat, "--resume "+restoredConversationID) {
					t.Errorf("argv lacks --resume %s: %s", restoredConversationID, flat)
				}
				if strings.Contains(flat, "--session-id") {
					t.Errorf("restored conversation must not pass --session-id: %s", flat)
				}
				return
			}
			if !sessionIDFlag.MatchString(flat) {
				t.Errorf("fresh conversation argv lacks --session-id <uuid>: %s", flat)
			}
			if strings.Contains(flat, "--resume") {
				t.Errorf("fresh conversation must not pass --resume: %s", flat)
			}
		})
	}
}
