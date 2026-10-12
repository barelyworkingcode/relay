package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"relaye2e/harness"
)

// b367Script is a stand-in claude binary. It answers --version, waits for the
// first user message, writes twelve stderr lines and exits 127.
const b367Script = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.0.0 (Claude Code)"; exit 0; fi
read -r first_message
i=1
while [ $i -le 12 ]; do
  case $i in
    5) printf '%02000d\n' 0 | tr 0 x >&2 ;;
    11) echo "acme-line-11 key sk-acmeacmeacmeacmeacme0000 end" >&2 ;;
    12) echo "env: /opt/acme/bin/claude: No such file or directory" >&2 ;;
    *) echo "acme-line-$i" >&2 ;;
  esac
  i=$((i+1))
done
exit 127
`

type b367Exit struct {
	Code   int      `json:"code"`
	Stderr []string `json:"stderr"`
}

type b367Frame struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	State     string    `json:"state"`
	Exit      *b367Exit `json:"exit"`
}

// b367Next reads frames until one of the session has the type (and, for
// session_state, the state).
func b367Next(t *testing.T, ws *harness.WSConn, sid, typ, state string) b367Frame {
	t.Helper()
	for n := 0; n < g1tFrameLimit; n++ {
		var f b367Frame
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.SessionID == sid && f.Type == typ && f.State == state {
			return f
		}
	}
	t.Fatalf("no %s frame (state %q) for session %s in %d frames", typ, state, sid, g1tFrameLimit)
	return b367Frame{}
}

func TestBug367ErroredStateCarriesProviderExit(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "acme-claude")
	if err := os.WriteFile(script, []byte(b367Script), 0o755); err != nil {
		t.Fatalf("writing the stand-in claude: %v", err)
	}
	tmpl, _ := json.Marshal([]map[string]any{{"id": "claude-code", "name": "Claude Code", "command": script}})
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-exit-detail", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "hello", "trace_id": harness.NewTrace(t)})

	errored := b367Next(t, ws, sid, "session_state", "errored")
	if errored.Exit == nil || errored.Exit.Code != 127 {
		t.Fatalf("errored session_state frame has exit %+v, want code 127", errored.Exit)
	}
	stderr := errored.Exit.Stderr
	if len(stderr) != 10 {
		t.Fatalf("exit.stderr holds %d lines, want 10: %q", len(stderr), stderr)
	}
	if stderr[0] != "acme-line-3" || stderr[9] != "env: /opt/acme/bin/claude: No such file or directory" {
		t.Errorf("exit.stderr runs from %q to %q, want acme-line-3 to the env: line", stderr[0], stderr[9])
	}
	for n, line := range stderr {
		if len(line) > 1024 {
			t.Errorf("exit.stderr[%d] is %d bytes, want at most 1024", n, len(line))
		}
	}
	if len(stderr[2]) != 1024 || strings.Trim(stderr[2], "x") != "" {
		t.Errorf("exit.stderr[2] (the 2000-byte line) is %d bytes, want 1024 of x", len(stderr[2]))
	}
	if !strings.Contains(stderr[8], "sk-[redacted]") || strings.Contains(stderr[8], "acmeacmeacmeacmeacme0000") {
		t.Errorf("exit.stderr[8] = %q, want the sk- token redacted", stderr[8])
	}

	exited := b367Next(t, ws, sid, "process_exited", "")
	if !reflect.DeepEqual(exited.Exit, errored.Exit) {
		t.Errorf("process_exited carries exit %+v, want %+v", exited.Exit, errored.Exit)
	}

	var listed struct {
		Sessions []struct {
			ID        string `json:"id"`
			Attention struct {
				Exit *b367Exit `json:"exit"`
			} `json:"attention"`
		} `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &listed)
	found := false
	for _, s := range listed.Sessions {
		if s.ID != sid {
			continue
		}
		found = true
		if !reflect.DeepEqual(s.Attention.Exit, errored.Exit) {
			t.Errorf("session list attention.exit = %+v, want %+v", s.Attention.Exit, errored.Exit)
		}
	}
	if !found {
		t.Fatalf("session list does not name %s", sid)
	}

	i.WaitEvent(harness.EventQuery{Key: "session.state", Fields: map[string]any{"session_id": sid, "to": "errored"}}, g1tHostDeadline)
	logged := i.Events(harness.EventQuery{Fields: map[string]any{"msg": "provider exited with error", "session": sid}})
	if len(logged) == 0 || logged[0]["exitCode"] != float64(127) {
		t.Errorf("provider exited with error lines for %s: %v, want one with exitCode 127", sid, logged)
	}
}
