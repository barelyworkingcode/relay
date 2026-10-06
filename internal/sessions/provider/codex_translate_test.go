package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// feedCodexFixture replays a recorded app-server session line by line, the
// way the stdout reader does.
func feedCodexFixture(t *testing.T, p *CodexProvider, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			p.processLine(json.RawMessage(l))
		}
	}
}

func newTranslateProvider() (*CodexProvider, *codexRec, *sessionstypes.Session) {
	rec := &codexRec{}
	sess := &sessionstypes.Session{ID: "cx1", Model: "codex/gpt-6-luna", Directory: "/tmp/p1"}
	return NewCodexProvider(sess, rec.handle, CodexConfig{}), rec, sess
}

func TestCodexTranslate_RecordedTurnsGiveTextThenMessageComplete(t *testing.T) {
	p, rec, sess := newTranslateProvider()
	feedCodexFixture(t, p, codexFixtureOK)

	if got := rec.text(); got != "There are 2 entries.pong" {
		t.Errorf("text deltas = %q, want %q", got, "There are 2 entries.pong")
	}
	if n := rec.count("message_complete"); n != 2 {
		t.Errorf("message_complete = %d, want 2 (one per recorded turn)", n)
	}
	if n := rec.count("error") + rec.count("raw_output"); n != 0 {
		t.Errorf("a clean recording produced %d error/raw_output events: %v", n, rec.all())
	}

	// The shell tool: one tool_use, then its result carrying the output.
	var sawUse, sawResult bool
	for i, k := range rec.kinds {
		if k != "llm_event" {
			continue
		}
		d := rec.data[i]
		sawUse = sawUse || (strings.Contains(d, `"tool_use"`) && strings.Contains(d, `"shell"`) && strings.Contains(d, "ls"))
		sawResult = sawResult || (strings.Contains(d, `"tool_result"`) && strings.Contains(d, `a\nb\n`) && strings.Contains(d, `"is_error":false`))
	}
	if !sawUse || !sawResult {
		t.Errorf("shell tool events missing: use=%v result=%v", sawUse, sawResult)
	}

	sess.Lock()
	defer sess.Unlock()
	if len(sess.Messages) != 2 || sess.Messages[0].Role != "assistant" ||
		!strings.Contains(string(sess.Messages[0].Content), "There are 2 entries.") ||
		!strings.Contains(string(sess.Messages[1].Content), "pong") {
		t.Errorf("session.Messages = %+v, want the two assistant replies", sess.Messages)
	}
}

func TestCodexTranslate_FailedTurnEmitsErrorThenMessageComplete(t *testing.T) {
	p, rec, _ := newTranslateProvider()
	feedCodexFixture(t, p, codexFixtureFailed)

	kinds := rec.all()
	errAt, doneAt := -1, -1
	for i, k := range kinds {
		switch k {
		case "error":
			errAt = i
		case "message_complete":
			doneAt = i
		}
	}
	if errAt < 0 || doneAt < errAt {
		t.Fatalf("want error before message_complete, got %v", kinds)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(rec.data[errAt]), &payload); err != nil {
		t.Fatal(err)
	}
	const want = "The 'no-such-model' model is not supported when using Codex with a ChatGPT account."
	if payload.Error != want {
		t.Errorf("error = %q, want the nested API message %q", payload.Error, want)
	}
}

// An unrecognised method, item type or non-JSON line warns once per kind of
// surprise, is forwarded as raw_output, and does not touch the turn.
func TestCodexTranslate_UnrecognisedInputWarnsOnceAndOnlyForwardsRaw(t *testing.T) {
	cases := map[string][2]string{
		"unknown method":    {`{"method":"acme/futureEvent","params":{"x":1}}`, `{"method":"acme/futureEvent","params":{"x":2}}`},
		"unknown item type": {`{"method":"item/started","params":{"item":{"type":"acmeHologram","id":"i1"}}}`, `{"method":"item/completed","params":{"item":{"type":"acmeHologram","id":"i1"}}}`},
		"not json":          {`{"broken`, `also {broken`},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureDebugJSON(t)
			p, rec, _ := newTranslateProvider()

			p.processLine(json.RawMessage(lines[0]))
			p.processLine(json.RawMessage(lines[1]))

			if got := rec.all(); len(got) != 2 || got[0] != "raw_output" || got[1] != "raw_output" {
				t.Fatalf("handler kinds = %v, want two raw_output and nothing else", got)
			}
			if rec.data[0] != lines[0] {
				t.Errorf("raw_output = %s, want the original line %s", rec.data[0], lines[0])
			}
			var warns int
			for _, l := range strings.Split(logs.all(), "\n") {
				if strings.Contains(l, `"level":"WARN"`) && strings.Contains(l, "codex: unrecognised event") && strings.Contains(l, `"cx1"`) {
					warns++
				}
			}
			if warns != 1 {
				t.Errorf("warnings = %d, want 1; log:\n%s", warns, logs.all())
			}
		})
	}
}

// A method in codex 0.160.0's notification list that relay does not translate
// is dropped silently.
func TestCodexTranslate_KnownUntranslatedMethodIsSilent(t *testing.T) {
	logs := captureDebugJSON(t)
	p, rec, _ := newTranslateProvider()
	p.processLine(json.RawMessage(`{"method":"thread/tokenUsage/updated","params":{}}`))
	p.processLine(json.RawMessage(`{"method":"item/started","params":{"item":{"type":"reasoning","id":"r1"}}}`))

	if got := rec.all(); len(got) != 0 {
		t.Errorf("handler kinds = %v, want none", got)
	}
	if strings.Contains(logs.all(), "unrecognised") {
		t.Errorf("a known method logged as unrecognised:\n%s", logs.all())
	}
}
