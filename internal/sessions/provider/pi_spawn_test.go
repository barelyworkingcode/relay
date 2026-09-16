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
