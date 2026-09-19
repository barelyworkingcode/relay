package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Nothing is computed in code: an install with no templates in settings.json
// resolves to none, not to a built-in set.
func TestEffectiveTerminalTemplates_ComesOnlyFromSettings(t *testing.T) {
	if got := EffectiveTerminalTemplates(&Settings{}); len(got) != 0 {
		t.Fatalf("an empty settings resolved to %d template(s), want none: %+v", len(got), got)
	}
	s := &Settings{TerminalTemplates: []TerminalTemplate{
		{ID: "b", Name: "B"},
		{ID: "a", Name: "A"},
	}}
	got := EffectiveTerminalTemplates(s)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("effective set = %+v, want the two settings entries sorted by id", got)
	}
}

// The default is what the operator asked for: the shell, sandboxed, with the
// home directory read-write. It is written into settings, not held in code.
func TestEnsureDefaultTerminalTemplates(t *testing.T) {
	s := &Settings{}
	if !EnsureDefaultTerminalTemplates(s) {
		t.Fatal("an empty settings was not seeded")
	}
	if len(s.TerminalTemplates) != 1 {
		t.Fatalf("seeded %d templates, want one default", len(s.TerminalTemplates))
	}
	def := s.TerminalTemplates[0]
	if def.ID != "shell" || !def.Sandbox || len(def.ReadWrite) != 1 || def.ReadWrite[0] != "~" || len(def.Read) != 0 {
		t.Fatalf("default template = %+v, want a sandboxed shell with ~ read-write", def)
	}
	if err := ValidateTerminalTemplate(def); err != nil {
		t.Fatalf("the default template does not validate: %v", err)
	}
	if _, ok := GetTerminalTemplate(s, "shell"); !ok {
		t.Fatal("the seeded template does not resolve")
	}

	t.Run("does not touch an install that has templates", func(t *testing.T) {
		existing := &Settings{TerminalTemplates: []TerminalTemplate{{ID: "mine", Name: "Mine"}}}
		if EnsureDefaultTerminalTemplates(existing) {
			t.Fatal("seeded over an existing template")
		}
		if len(existing.TerminalTemplates) != 1 || existing.TerminalTemplates[0].ID != "mine" {
			t.Fatalf("the operator's templates changed: %+v", existing.TerminalTemplates)
		}
	})

	t.Run("seeds again when the list is emptied", func(t *testing.T) {
		s := &Settings{TerminalTemplates: []TerminalTemplate{}}
		if !EnsureDefaultTerminalTemplates(s) {
			t.Fatal("an emptied list was not seeded")
		}
	})
}

// The sandbox folders are a template's own: they validate as shapes, and a
// folder relay could never place is refused when the template is read.
func TestValidateTerminalTemplate_SandboxFolders(t *testing.T) {
	ok := TerminalTemplate{ID: "x", Name: "X", Sandbox: true,
		Read:      []string{"~", "~/.gitconfig", "/opt/homebrew", "~/Library/Application Support/tool"},
		ReadWrite: []string{"~/.claude", "/private/tmp/cc-socks"}}
	if err := ValidateTerminalTemplate(ok); err != nil {
		t.Fatalf("a well-formed template was refused: %v", err)
	}
	for name, folder := range map[string]string{
		"relative":          "tools/bin",
		"empty":             "",
		"the root":          "/",
		"the root, unclean": "/tmp/..",
		"another user's ~":  "~alice/x",
		"dot-relative":      "./tools",
	} {
		for _, field := range []string{"read", "read_write", "deny"} {
			tmpl := TerminalTemplate{ID: "x", Name: "X"}
			switch field {
			case "read":
				tmpl.Read = []string{folder}
			case "read_write":
				tmpl.ReadWrite = []string{folder}
			default:
				tmpl.Deny = []string{folder}
			}
			err := ValidateTerminalTemplate(tmpl)
			if !errors.Is(err, ErrTemplateGrant) {
				t.Errorf("%s %s (%q): err = %v, want ErrTemplateGrant", field, name, folder, err)
			}
		}
	}
}

func TestValidateTerminalTemplate_ModelEndpointURLOnlyInEnv(t *testing.T) {
	if err := ValidateTerminalTemplate(TerminalTemplate{ID: "x", Name: "X", Env: map[string]string{"U": ModelEndpointURLMarker}}); err != nil {
		t.Fatalf("${MODEL_ENDPOINT_URL} in env was refused: %v", err)
	}
	for name, tmpl := range map[string]TerminalTemplate{
		"command": {ID: "x", Name: "X", Command: "curl " + ModelEndpointURLMarker},
		"args":    {ID: "x", Name: "X", Args: []string{ModelEndpointURLMarker}},
	} {
		if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrModelEndpointSubstitution) {
			t.Errorf("%s: err = %v, want ErrModelEndpointSubstitution", name, err)
		}
	}
}

func TestTerminalTemplate_CloneKeepsFoldersIndependent(t *testing.T) {
	orig := &Settings{TerminalTemplates: []TerminalTemplate{{ID: "x", Name: "X", Read: []string{"/a"}, ReadWrite: []string{"/b"}, Deny: []string{"/c"}}}}
	cp := orig.Clone()
	cp.TerminalTemplates[0].Deny[0] = "/changed"
	cp.TerminalTemplates[0].Read[0] = "/changed"
	cp.TerminalTemplates[0].ReadWrite = append(cp.TerminalTemplates[0].ReadWrite, "/extra")
	if orig.TerminalTemplates[0].Deny[0] != "/c" || orig.TerminalTemplates[0].Read[0] != "/a" || len(orig.TerminalTemplates[0].ReadWrite) != 1 {
		t.Fatalf("mutating the clone changed the original: %+v", orig.TerminalTemplates[0])
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

func TestValidateTerminalTemplate_RefusesRelayTokenInCommand(t *testing.T) {
	tmpl := TerminalTemplate{ID: "x", Name: "X", Command: "/bin/echo-${RELAY_TOKEN}"}
	err := ValidateTerminalTemplate(tmpl)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("expected ErrRelayTokenSubstitution, got %v", err)
	}
}

func TestValidateTerminalTemplate_RefusesRelayPrefixedEnvPassthrough(t *testing.T) {
	for _, name := range []string{"RELAY_PROJECT_TOKEN", "RELAY_TOKEN", "RELAY_LLM_HOOK_TOKEN"} {
		tmpl := TerminalTemplate{ID: "x", Name: "X", EnvPassthrough: []string{name}}
		err := ValidateTerminalTemplate(tmpl)
		if err == nil {
			t.Fatalf("%s: expected refusal, got nil", name)
		}
		if !errors.Is(err, ErrRelayEnvPassthrough) {
			t.Fatalf("%s: expected ErrRelayEnvPassthrough, got %v", name, err)
		}
	}
}

func TestValidateTerminalTemplate_AllowsNonRelayEnvPassthrough(t *testing.T) {
	tmpl := TerminalTemplate{
		ID:             "x",
		Name:           "X",
		EnvPassthrough: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
	}
	if err := ValidateTerminalTemplate(tmpl); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
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

// A hand-edited settings.json (or a future editor) that writes
// ${RELAY_TOKEN} into Settings.TerminalTemplates must not reach the
// resolved list -- resolution refuses it the same as ValidateTerminalTemplate
// does at the point of writing.
func TestEffectiveTerminalTemplates_SkipsInvalidEntry(t *testing.T) {
	s := &Settings{
		TerminalTemplates: []TerminalTemplate{
			{ID: "bad", Name: "Bad", Args: []string{"${RELAY_TOKEN}"}},
			{ID: "good", Name: "Good"},
		},
	}
	got := EffectiveTerminalTemplates(s)
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("effective set = %+v, want only the valid entry", got)
	}
}

// A template with a folder relay cannot place is refused like any other invalid
// one, so it never reaches a launch with a grant that would be dropped.
func TestEffectiveTerminalTemplates_SkipsARelativeFolder(t *testing.T) {
	s := &Settings{TerminalTemplates: []TerminalTemplate{
		{ID: "bad", Name: "Bad", Sandbox: true, ReadWrite: []string{"tools"}},
	}}
	if got := EffectiveTerminalTemplates(s); len(got) != 0 {
		t.Fatalf("a template with a relative folder resolved: %+v", got)
	}
}

func TestEffectiveTerminalTemplatesForProject_FiltersByAllowedTemplates(t *testing.T) {
	s := &Settings{TerminalTemplates: []TerminalTemplate{
		{ID: "shell", Name: "Shell"}, {ID: "pi", Name: "pi"}, {ID: "rh", Name: "rh"},
	}}
	ids := func(p *Project) string {
		var out []string
		for _, tmpl := range EffectiveTerminalTemplatesForProject(s, p) {
			out = append(out, tmpl.ID)
		}
		return strings.Join(out, ",")
	}
	for name, c := range map[string]struct {
		proj *Project
		want string
	}{
		"nil project":   {nil, ""},
		"nil list":      {&Project{}, ""},
		"empty list":    {&Project{AllowedTemplates: []string{}}, ""},
		"wildcard":      {&Project{AllowedTemplates: []string{"*"}}, "pi,rh,shell"},
		"listed":        {&Project{AllowedTemplates: []string{"shell", "rh", "gone"}}, "rh,shell"},
		"star + others": {&Project{AllowedTemplates: []string{"*", "shell"}}, "shell"},
	} {
		if got := ids(c.proj); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

// A project from before allowed_templates existed keeps every template; one
// created after (empty list, current version) is never widened by a load.
func TestNormalize_MigratesTemplateAccessOnce(t *testing.T) {
	s := &Settings{Version: 1, Projects: []Project{
		{ID: "old"},
		{ID: "remote", Kind: ProjectKindRemote},
		{ID: "set", AllowedTemplates: []string{"shell"}},
	}}
	s.normalize()
	if got := s.Projects[0].AllowedTemplates; !IsWildcard(got) {
		t.Errorf("existing project = %v, want [*]", got)
	}
	if got := s.Projects[1].AllowedTemplates; got == nil || len(got) != 0 {
		t.Errorf("remote project = %#v, want empty", got)
	}
	if got := s.Projects[2].AllowedTemplates; len(got) != 1 || got[0] != "shell" {
		t.Errorf("explicit list = %v, want it kept", got)
	}
	if s.Version != CurrentSettingsVersion {
		t.Errorf("version = %d, want %d", s.Version, CurrentSettingsVersion)
	}

	s.Projects = append(s.Projects, Project{ID: "new", AllowedTemplates: []string{}})
	s.normalize()
	if got := s.Projects[3].AllowedTemplates; len(got) != 0 {
		t.Errorf("a project created after the migration was widened: %v", got)
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

// --- ${MODEL_KEY}: the one credential a template may map into its env ---

func TestValidateTerminalTemplate_AllowsModelKeyInEnvOfAModelKeyTemplate(t *testing.T) {
	tmpl := TerminalTemplate{
		ID: "cc", Name: "CC", ModelKey: true,
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":       "http://127.0.0.1:9911",
			"ANTHROPIC_CUSTOM_HEADERS": "X-Relay-Key: ${MODEL_KEY}",
		},
	}
	if err := ValidateTerminalTemplate(tmpl); err != nil {
		t.Fatalf("a model_key template mapping the key into env was refused: %v", err)
	}
}

func TestValidateTerminalTemplate_RefusesModelKeyInATemplateThatDidNotOptIn(t *testing.T) {
	tmpl := TerminalTemplate{ID: "cc", Name: "CC", Env: map[string]string{"H": "X-Relay-Key: ${MODEL_KEY}"}}
	if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrModelKeySubstitution) {
		t.Fatalf("model_key false with a ${MODEL_KEY} mapping: err = %v, want ErrModelKeySubstitution", err)
	}
}

func TestValidateTerminalTemplate_RefusesModelKeyInArgv(t *testing.T) {
	for name, tmpl := range map[string]TerminalTemplate{
		"command": {ID: "x", Name: "X", ModelKey: true, Command: "tool-${MODEL_KEY}"},
		"args":    {ID: "x", Name: "X", ModelKey: true, Args: []string{"--key", "${MODEL_KEY}"}},
	} {
		if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrModelKeySubstitution) {
			t.Errorf("${MODEL_KEY} in %s: err = %v, want ErrModelKeySubstitution", name, err)
		}
	}
}

// The carve-out is for ${MODEL_KEY} alone: the retired credential stays
// refused, including in a model_key template's env.
func TestValidateTerminalTemplate_ModelKeyCarveOutDoesNotReopenRelayToken(t *testing.T) {
	tmpl := TerminalTemplate{ID: "x", Name: "X", ModelKey: true, Env: map[string]string{"T": "${RELAY_TOKEN}", "K": "${MODEL_KEY}"}}
	if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrRelayTokenSubstitution) {
		t.Fatalf("err = %v, want ErrRelayTokenSubstitution", err)
	}
}

// ExpandTemplateVars is argv-only and knows nothing of the key: the marker
// survives it untouched (it is expanded at spawn, where the minted key exists).
func TestExpandTemplateVars_LeavesModelKeyMarkerAlone(t *testing.T) {
	if got := ExpandTemplateVars("${MODEL_KEY}", "/p", "id"); got != "${MODEL_KEY}" {
		t.Fatalf("ExpandTemplateVars expanded the model key marker: %q", got)
	}
}

func TestEffectiveTerminalTemplates_SkipsAModelKeyMappingWithoutOptIn(t *testing.T) {
	s := &Settings{TerminalTemplates: []TerminalTemplate{
		{ID: "unmapped-optin", Name: "Opted in, no mapping", ModelKey: true},
		{ID: "mapped-noopt", Name: "Mapped, not opted in", Env: map[string]string{"H": "${MODEL_KEY}"}},
	}}
	var sawOptIn, sawNoOpt bool
	for _, tmpl := range EffectiveTerminalTemplates(s) {
		switch tmpl.ID {
		case "unmapped-optin":
			sawOptIn = true
		case "mapped-noopt":
			sawNoOpt = true
		}
	}
	if !sawOptIn {
		t.Error("a model_key template with no mapping was refused; it is valid, it just gets no env injected")
	}
	if sawNoOpt {
		t.Error("a template mapping ${MODEL_KEY} without model_key: true reached the effective list")
	}
}
