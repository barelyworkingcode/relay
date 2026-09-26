package config

import (
	"encoding/json"
	"errors"
	"os"
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
	if def.ID != "shell" || def.Sandbox == nil || !*def.Sandbox || len(def.ReadWrite) != 1 || def.ReadWrite[0] != "~" || len(def.Read) != 0 {
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
	ok := TerminalTemplate{ID: "x", Name: "X", Sandbox: ptr(true),
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

// A template is sandboxed unless it says otherwise: the zero value fails closed.
func TestTerminalTemplate_Sandboxed(t *testing.T) {
	for name, c := range map[string]struct {
		sandbox *bool
		want    bool
	}{
		"absent": {nil, true},
		"true":   {ptr(true), true},
		"false":  {ptr(false), false},
	} {
		if got := (TerminalTemplate{ID: "x", Name: "X", Sandbox: c.sandbox}).Sandboxed(); got != c.want {
			t.Errorf("%s: Sandboxed() = %v, want %v", name, got, c.want)
		}
	}
}

// settings.json keeps what the operator wrote: an absent or null sandbox stays
// absent through a load and a save, and is never rewritten to false.
func TestTerminalTemplate_SandboxRoundTripsThroughSettingsJSON(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	raw := `{"terminal_templates":[
		{"id":"absent","name":"Absent"},
		{"id":"null","name":"Null","sandbox":null},
		{"id":"off","name":"Off","sandbox":false},
		{"id":"on","name":"On","sandbox":true}
	]}`
	if err := os.WriteFile(sdSettingsPath(dir), []byte(raw), 0600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
	want := map[string]struct {
		stored    string // the value on disk, "" for no key
		sandboxed bool
	}{
		"absent": {"", true},
		"null":   {"", true},
		"off":    {"false", false},
		"on":     {"true", true},
	}

	store := sealedSettingsStoreAt(dir)
	for id, w := range want {
		tmpl, ok := GetTerminalTemplate(store.Get(), id)
		if !ok {
			t.Fatalf("template %q did not load", id)
		}
		if isSet := tmpl.Sandbox != nil; isSet != (w.stored != "") {
			t.Errorf("%s: loaded Sandbox = %v, want set=%v", id, tmpl.Sandbox, w.stored != "")
		}
		if tmpl.Sandboxed() != w.sandboxed {
			t.Errorf("%s: Sandboxed() = %v, want %v", id, tmpl.Sandboxed(), w.sandboxed)
		}
	}

	if err := store.With(func(s *Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates, TerminalTemplate{ID: "other", Name: "Other", Sandbox: ptr(false)})
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var onDisk struct {
		TerminalTemplates []map[string]json.RawMessage `json:"terminal_templates"`
	}
	if err := json.Unmarshal(sdRead(t, dir), &onDisk); err != nil {
		t.Fatalf("decode settings.json: %v", err)
	}
	for _, entry := range onDisk.TerminalTemplates {
		var id string
		_ = json.Unmarshal(entry["id"], &id)
		w, ok := want[id]
		if !ok {
			continue
		}
		delete(want, id)
		if v, has := entry["sandbox"]; string(v) != w.stored || has != (w.stored != "") {
			t.Errorf("%s: saved sandbox = %q (present %v), want %q", id, v, has, w.stored)
		}
	}
	if len(want) != 0 {
		t.Errorf("templates missing after save: %v", want)
	}
}

func TestTerminalTemplate_CloneDoesNotShareSandbox(t *testing.T) {
	orig := &Settings{TerminalTemplates: []TerminalTemplate{{ID: "x", Name: "X", Sandbox: ptr(true)}}}
	cp := orig.Clone()
	*cp.TerminalTemplates[0].Sandbox = false
	if !*orig.TerminalTemplates[0].Sandbox {
		t.Fatal("setting the clone's sandbox changed the original's")
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
		{ID: "bad", Name: "Bad", Sandbox: ptr(true), ReadWrite: []string{"tools"}},
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

// --- host templates (docs/ssh-hosts.md) ---

func TestValidateHostTemplate_RefusesWhatOnlyMeansSomethingOnTheConsole(t *testing.T) {
	for name, c := range map[string]struct {
		tmpl TerminalTemplate
		want error // nil: any error will do
	}{
		"sandbox":         {TerminalTemplate{ID: "x", Name: "X", Sandbox: ptr(true)}, ErrHostTemplateSandbox},
		"read":            {TerminalTemplate{ID: "x", Name: "X", Read: []string{"/opt"}}, ErrHostTemplateSandbox},
		"read_write":      {TerminalTemplate{ID: "x", Name: "X", ReadWrite: []string{"/opt"}}, ErrHostTemplateSandbox},
		"env_passthrough": {TerminalTemplate{ID: "x", Name: "X", EnvPassthrough: []string{"PATH"}}, ErrHostTemplateEnvPassthrough},
		"model_key":       {TerminalTemplate{ID: "x", Name: "X", ModelKey: true}, ErrHostTemplateModelKey},
		// ValidateTerminalTemplate refuses this first (model_key is false),
		// so the sentinel is the base one; what matters is that it's refused.
		"${MODEL_KEY} in env": {TerminalTemplate{ID: "x", Name: "X", Env: map[string]string{"H": "X-Relay-Key: ${MODEL_KEY}"}}, nil},
	} {
		err := ValidateHostTemplate(c.tmpl)
		if err == nil {
			t.Errorf("%s: accepted, want refused", name)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	ok := TerminalTemplate{ID: "claude-code", Name: "Claude Code", Command: "/usr/local/bin/claude", Args: []string{"${PROJECT_PATH}"}, Env: map[string]string{"DEBUG": "1"}}
	if err := ValidateHostTemplate(ok); err != nil {
		t.Fatalf("a plain host template was refused: %v", err)
	}
}

// A host template never sandboxes, so leaving the key out or saying false is
// fine; only an explicit true is refused.
func TestValidateHostTemplate_AcceptsAnAbsentOrFalseSandbox(t *testing.T) {
	for name, sandbox := range map[string]*bool{"absent": nil, "false": ptr(false)} {
		if err := ValidateHostTemplate(TerminalTemplate{ID: "x", Name: "X", Sandbox: sandbox}); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestDefaultHostTemplates(t *testing.T) {
	got := DefaultHostTemplates(HostProbe{OK: true})
	if len(got) != 1 || got[0].ID != "shell" || got[0].Command != "" {
		t.Fatalf("no claude_path: got %+v, want only the login-shell template", got)
	}
	got = DefaultHostTemplates(HostProbe{OK: true, ClaudePath: "/home/u/.local/bin/claude"})
	if len(got) != 2 || got[0].ID != "shell" || got[1].ID != "claude-code" || got[1].Command != "/home/u/.local/bin/claude" {
		t.Fatalf("with claude_path: got %+v, want shell + claude-code at the probed path", got)
	}
	for _, tmpl := range got {
		if err := ValidateHostTemplate(tmpl); err != nil {
			t.Errorf("seeded template %q fails ValidateHostTemplate: %v", tmpl.ID, err)
		}
	}
}

func TestTemplatesForProject(t *testing.T) {
	s := &Settings{
		TerminalTemplates: []TerminalTemplate{{ID: "console", Name: "Console"}},
		Hosts: []Host{{ID: "h1", Name: "devbox", TerminalTemplates: []TerminalTemplate{
			{ID: "zsh", Name: "Zsh", Command: "zsh"},
			{ID: "bad", Name: "Bad", Sandbox: ptr(true)},
			{ID: "shell", Name: "Shell"},
		}}},
	}
	ids := func(ts []TerminalTemplate) string {
		var out []string
		for _, tmpl := range ts {
			out = append(out, tmpl.ID)
		}
		return strings.Join(out, ",")
	}

	hosted := &Project{HostID: "h1", AllowedTemplates: []string{"*"}}
	if got := ids(TemplatesForProject(s, hosted)); got != "shell,zsh" {
		t.Errorf("hosted project: got %q, want the host's valid templates sorted and no console ones", got)
	}

	gone := TemplatesForProject(s, &Project{HostID: "missing", AllowedTemplates: []string{"*"}})
	if gone == nil || len(gone) != 0 {
		t.Errorf("missing host: got %#v, want an empty non-nil slice", gone)
	}

	for name, p := range map[string]*Project{
		"nil":      nil,
		"wildcard": {AllowedTemplates: []string{"*"}},
		"none":     {AllowedTemplates: []string{}},
	} {
		if got, want := ids(TemplatesForProject(s, p)), ids(EffectiveTerminalTemplatesForProject(s, p)); got != want {
			t.Errorf("non-hosted %s: got %q, want EffectiveTerminalTemplatesForProject's %q", name, got, want)
		}
	}
}

// persistTestProject is a uuid-shaped project id: PersistSessionName carries
// its first 8 characters.
const persistTestProject = "0123abcd-1111-2222-3333-444455556666"

// PersistSessionName and ParsePersistSessionName are inverses for every name
// relay mints, and Parse refuses every shape Name could not have produced —
// that refusal is what keeps a foreign tmux session out of relay's list.
func TestPersistSessionName_RoundTripAndRefusals(t *testing.T) {
	for _, c := range []struct {
		projectID, templateID string
		n                     int
		wantName              string
		wantProject8          string
		wantTemplate          string
	}{
		{persistTestProject, "shell", 1, "relay-0123abcd-shell-1", "0123abcd", "shell"},
		{persistTestProject, "claude-code", 12, "relay-0123abcd-claude-code-12", "0123abcd", "claude-code"},
		// tmux refuses . and : in a session name; both become _.
		{persistTestProject, "a.b:c", 3, "relay-0123abcd-a_b_c-3", "0123abcd", "a_b_c"},
		{"ab.c:efgh-rest", "shell", 7, "relay-ab_c_efg-shell-7", "ab_c_efg", "shell"},
	} {
		name := PersistSessionName(c.projectID, c.templateID, c.n)
		if name != c.wantName {
			t.Errorf("PersistSessionName(%q, %q, %d) = %q, want %q", c.projectID, c.templateID, c.n, name, c.wantName)
			continue
		}
		p8, tmpl, n, ok := ParsePersistSessionName(name)
		if !ok || p8 != c.wantProject8 || tmpl != c.wantTemplate || n != c.n {
			t.Errorf("ParsePersistSessionName(%q) = (%q, %q, %d, %v), want (%q, %q, %d, true)", name, p8, tmpl, n, ok, c.wantProject8, c.wantTemplate, c.n)
		}
	}

	for _, bad := range []string{
		"",
		"main",
		"tmux-0123abcd-shell-1",   // wrong prefix
		"relay-p1-shell-1",        // project part shorter than 8
		"relay-0123abcdshell-1",   // no - after project8
		"relay-0123abcd--1",       // empty template
		"relay-0123abcd-shell",    // no n
		"relay-0123abcd-shell-",   // empty n
		"relay-0123abcd-shell-0",  // n = 0
		"relay-0123abcd-shell-01", // leading zero
		"relay-0123abcd-shell-+1", // sign
		"relay-0123abcd-shell-x",  // non-numeric
		"relay-0123abcd-sh.ll-1",  // unsanitized template
		"relay-0123ab:d-shell-1",  // unsanitized project
	} {
		if p8, tmpl, n, ok := ParsePersistSessionName(bad); ok {
			t.Errorf("ParsePersistSessionName(%q) = (%q, %q, %d, true), want refused", bad, p8, tmpl, n)
		}
	}
}

func TestNextPersistSessionN(t *testing.T) {
	if got := NextPersistSessionN(nil, persistTestProject, "shell"); got != 1 {
		t.Fatalf("no sessions: n = %d, want 1", got)
	}
	existing := []string{
		"relay-0123abcd-shell-1",
		"relay-0123abcd-shell-4", // gap at 2, 3: max+1, not the first hole
		"relay-0123abcd-shell-07",
		"relay-0123abcd-claude-9", // other template
		"relay-ffffffff-shell-20", // other project
		"relay-0123abcd-shell-x",
		"main",
	}
	if got := NextPersistSessionN(existing, persistTestProject, "shell"); got != 5 {
		t.Fatalf("n = %d, want 5 (max own n + 1; other projects/templates ignored)", got)
	}
	if got := NextPersistSessionN(existing, persistTestProject, "claude"); got != 10 {
		t.Fatalf("claude: n = %d, want 10", got)
	}
	// Matching is on the sanitized template id, as the names carry it.
	if got := NextPersistSessionN([]string{"relay-0123abcd-a_b-2"}, persistTestProject, "a.b"); got != 3 {
		t.Fatalf("sanitized template: n = %d, want 3", got)
	}
}

func TestParseProjectPersistSessionName_Ownership(t *testing.T) {
	tmpl, n, ok := ParseProjectPersistSessionName("relay-0123abcd-claude-code-3", persistTestProject)
	if !ok || tmpl != "claude-code" || n != 3 {
		t.Fatalf("own session: (%q, %d, %v), want (claude-code, 3, true)", tmpl, n, ok)
	}
	for _, name := range []string{
		"relay-ffffffff-shell-1", // other project
		"relay-0123abcd-shell-0", // malformed
		"main",
	} {
		if _, _, ok := ParseProjectPersistSessionName(name, persistTestProject); ok {
			t.Errorf("ParseProjectPersistSessionName(%q) accepted, want refused", name)
		}
	}
}

// Persist is a host-template-only flag: the console validator refuses it,
// the host validator allows it.
func TestValidateTemplate_PersistOnlyOnAHostTemplate(t *testing.T) {
	tmpl := TerminalTemplate{ID: "claude", Name: "Claude", Command: "claude", Persist: true}
	if err := ValidateTerminalTemplate(tmpl); !errors.Is(err, ErrConsoleTemplatePersist) {
		t.Fatalf("ValidateTerminalTemplate(persist) = %v, want ErrConsoleTemplatePersist", err)
	}
	if err := ValidateHostTemplate(tmpl); err != nil {
		t.Fatalf("ValidateHostTemplate(persist) = %v, want nil", err)
	}
}
