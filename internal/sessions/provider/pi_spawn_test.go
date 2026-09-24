package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/pioverlay"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func newTestPiProvider(session *sessionstypes.Session, cfg PiConfig) *PiProvider {
	return NewPiProvider(session, func(string, json.RawMessage) {}, cfg)
}

func TestBuildPiArgs_ProviderIsAlwaysRelay(t *testing.T) {
	p := newTestPiProvider(&sessionstypes.Session{Model: "claude-sonnet-4"}, PiConfig{})
	args := p.buildPiArgs("/tmp/sessdir", "", "")

	assertNoForbiddenSecrets(t, "pi argv", args)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--provider "+pioverlay.RelayProvider) {
		t.Errorf("argv missing --provider %s: %v", pioverlay.RelayProvider, args)
	}
	if !strings.Contains(joined, "--model claude-sonnet-4") {
		t.Errorf("argv missing --model: %v", args)
	}
}

func TestBuildPiArgs_SystemPromptIsAFilePath(t *testing.T) {
	p := newTestPiProvider(&sessionstypes.Session{Model: "m"}, PiConfig{})
	args := p.buildPiArgs("/tmp/sessdir", "", "/tmp/sysprompt.txt")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--append-system-prompt /tmp/sysprompt.txt") {
		t.Errorf("argv missing --append-system-prompt file path: %v", args)
	}
	for _, a := range args {
		if a == "secret prompt text" {
			t.Fatal("system prompt leaked into argv instead of a file path")
		}
	}
}

func TestBuildPiArgs_ExtraArgsAppendedVerbatimNoExpansion(t *testing.T) {
	p := newTestPiProvider(&sessionstypes.Session{Model: "m"}, PiConfig{
		ExtraArgs: []string{"--foo", "${RELAY_TOKEN}"},
	})
	args := p.buildPiArgs("/tmp/sessdir", "", "")
	joined := strings.Join(args, " ")
	// Verbatim: the literal placeholder string passes through unexpanded —
	// it must never be replaced by an actual token value.
	if !strings.Contains(joined, "${RELAY_TOKEN}") {
		t.Errorf("extraArgs not appended verbatim: %v", args)
	}
}

func TestBuildPiArgs_SkillDirOmittedWhenAlreadyInExtraArgs(t *testing.T) {
	p := newTestPiProvider(&sessionstypes.Session{Model: "m"}, PiConfig{
		ExtraArgs: []string{"--skill", "/custom/skills"},
	})
	if got := p.resolveSkillDir(); got != "" {
		t.Errorf("resolveSkillDir = %q, want empty when --skill already in extraArgs", got)
	}
}

func TestBuildPiArgs_PiSpelledModelSendsBareBrokerID(t *testing.T) {
	p := newTestPiProvider(&sessionstypes.Session{Model: "pi/acme/Chat"}, PiConfig{})
	args := p.buildPiArgs("/tmp/sessdir", "", "")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model Chat ") {
		t.Errorf("argv should carry the bare broker id: %v", args)
	}
}

func TestPiBrokerModelID(t *testing.T) {
	cases := map[string]string{
		"pi/acme/Chat":              "Chat",
		"pi/relay-router/org/model": "org/model",
		"Chat":                      "Chat",
		"claude-sonnet-4":           "claude-sonnet-4",
		"pi/acme":                   "pi/acme",
		"pi/acme/":                  "pi/acme/",
		"pi//Chat":                  "pi//Chat",
	}
	for in, want := range cases {
		if got := piBrokerModelID(in); got != want {
			t.Errorf("piBrokerModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// recordPiEvents returns a provider whose handler records every event kind
// and the payload of each "error" event.
func recordPiEvents(t *testing.T) (p *PiProvider, kinds *[]string, errPayloads *[]string) {
	t.Helper()
	kinds, errPayloads = &[]string{}, &[]string{}
	p = NewPiProvider(&sessionstypes.Session{Model: "pi/acme/Chat"}, func(kind string, data json.RawMessage) {
		*kinds = append(*kinds, kind)
		if kind == "error" {
			*errPayloads = append(*errPayloads, string(data))
		}
	}, PiConfig{})
	return p, kinds, errPayloads
}

func feedPiLines(p *PiProvider, lines ...string) {
	for _, l := range lines {
		p.processLine(json.RawMessage(l))
	}
}

const (
	piFailedMessageEnd = `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"404 model not found"}}`
	piOKMessageEnd     = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"stopReason":"stop"}}`
	piAgentStart       = `{"type":"agent_start"}`
	piAgentEndRetry    = `{"type":"agent_end","messages":[],"willRetry":true}`
	piAgentEndFinal    = `{"type":"agent_end","messages":[],"willRetry":false}`
	piRetryStart       = `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"404 model not found"}`
)

func TestPiFailedAssistantMessageSurfacesError(t *testing.T) {
	p, kinds, errs := recordPiEvents(t)
	feedPiLines(p, piAgentStart, piFailedMessageEnd, piAgentEndFinal)

	if len(*errs) != 1 {
		t.Fatalf("want exactly one error event, got kinds %v", *kinds)
	}
	if !strings.Contains((*errs)[0], "404 model not found") {
		t.Errorf("error payload lost pi's errorMessage: %s", (*errs)[0])
	}
}

func TestPiSuccessfulTurnEmitsNoError(t *testing.T) {
	p, kinds, errs := recordPiEvents(t)
	feedPiLines(p, piAgentStart, piOKMessageEnd, piAgentEndFinal)
	if len(*errs) != 0 {
		t.Errorf("a successful turn must emit no error, got kinds %v", *kinds)
	}
}

func TestPiFailureThenSuccessfulRetryEmitsNoError(t *testing.T) {
	p, kinds, errs := recordPiEvents(t)
	feedPiLines(p,
		piAgentStart, piFailedMessageEnd, piAgentEndRetry, piRetryStart,
		piAgentStart, piOKMessageEnd,
		`{"type":"auto_retry_end","success":true,"attempt":1}`,
		piAgentEndFinal,
	)
	if len(*errs) != 0 {
		t.Errorf("a retried-then-successful turn must emit no error, got kinds %v", *kinds)
	}
}

func TestPiRetriesExhaustedEmitsOneError(t *testing.T) {
	p, kinds, errs := recordPiEvents(t)
	feedPiLines(p,
		piAgentStart, piFailedMessageEnd, piAgentEndRetry, piRetryStart,
		piAgentStart, piFailedMessageEnd, piAgentEndFinal,
		`{"type":"auto_retry_end","success":false,"attempt":1,"finalError":"404 model not found"}`,
	)
	if len(*errs) != 1 {
		t.Errorf("a finally failed turn must emit exactly one error, got kinds %v", *kinds)
	}
}

func TestPiMessageUpdateErrorEmitsNothing(t *testing.T) {
	p, kinds, _ := recordPiEvents(t)
	feedPiLines(p, `{"type":"message_update","assistantMessageEvent":{"type":"error","reason":"error"}}`)
	if len(*kinds) != 0 {
		t.Errorf("a message_update error must emit nothing, got %v", *kinds)
	}
}
