package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// TerminalTemplate is relay's own launch config for a PTY terminal — the
// data relayLLM's internal/terminal/terminal_template.go used to own before
// session-host moved launching into relay (SH §terminal templates). On disk
// inside Settings.TerminalTemplates the entries carry their own ID; the five
// built-ins (BuiltinTerminalTemplates) are computed in code and never
// written to settings.json unless an operator's own entry shares one's ID.
//
// There is deliberately no UseRelayToken field. relayLLM's template could
// interpolate ${RELAY_TOKEN} into a spawned child's argv/env; that is a
// bearer credential in a place any same-uid process can read via
// KERN_PROCARGS2, and it is retired, not ported (ValidateTerminalTemplate,
// ExpandTemplateVars).
type TerminalTemplate struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
	BuiltIn     bool              `json:"builtIn,omitempty"`
	IdleTimeout int               `json:"idleTimeout,omitempty"` // minutes, 0 = default (1440 = 24h)

	// EnvPassthrough names host environment variables copied into the
	// child's env verbatim (e.g. provider API keys claude-code picks up).
	// Never a place a relay credential belongs — spawn-time enforcement of
	// that is R-S4a/R-S4b's launch path, not this package.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	// Sandbox is the template's default opt-in to C7 seatbelt confinement.
	// relay-sessions builds the real sandbox profile from this bool plus the
	// launch's project/session paths; this field only records the
	// template's own default. False for every human pty template
	// (shell, opencode) unless the operator opts one in.
	Sandbox bool `json:"sandbox,omitempty"`

	// ModelKey opts a pty template into a minted model-broker key at launch
	// (C8), the way the `pi` template does so its interactive CLI reaches
	// relay's model endpoint. Distinct from C5's LaunchSpec `model_key`
	// field, which carries the minted secret itself — this is only the
	// template's declaration that it wants one.
	ModelKey bool `json:"model_key,omitempty"`
}

// builtinTerminalTemplateIDs drives the BuiltIn field on every resolved
// template, the same way relayLLM's protectedTemplateIDs did — computed
// from the ID, never stored, so a hand-edited settings.json can't grant or
// revoke the flag by writing it.
var builtinTerminalTemplateIDs = map[string]bool{
	"claude-code": true,
	"opencode":    true,
	"shell":       true,
	"rh":          true,
	"pi":          true,
}

// BuiltinTerminalTemplates returns relay's five seeded templates, freshly
// constructed on every call so a caller mutating one slice can never affect
// another. Ported from relayLLM's seedDefaultPTYConfig plus the two new
// session-host templates (rh, pi), with useRelayToken dropped everywhere.
func BuiltinTerminalTemplates() []TerminalTemplate {
	return []TerminalTemplate{
		{
			ID:             "claude-code",
			Name:           "Claude Code",
			Command:        "claude",
			Icon:           "terminal",
			Description:    "Claude Code CLI agent",
			EnvPassthrough: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"},
			Sandbox:        true,
		},
		{
			ID:          "opencode",
			Name:        "OpenCode",
			Command:     "opencode",
			Icon:        "terminal",
			Description: "OpenCode CLI agent",
		},
		{
			ID:          "shell",
			Name:        "Shell",
			Icon:        "shell",
			Description: "Default system shell",
		},
		{
			ID:          "rh",
			Name:        "rh",
			Command:     "rh",
			Args:        []string{"--project", "${PROJECT_ID}"},
			Icon:        "terminal",
			Description: "relayHarness: folder- and tool-confined coding agent",
			Sandbox:     true,
		},
		{
			ID:          "pi",
			Name:        "pi",
			Command:     "pi",
			Icon:        "terminal",
			Description: "pi coding agent",
			Sandbox:     true,
			ModelKey:    true,
		},
	}
}

// ErrRelayTokenSubstitution is refused at validation, never silently
// stripped or expanded to empty: a template whose argv or env still
// reaches ${RELAY_TOKEN} is invalid input, the same way a template that
// relied on relayLLM's retired useRelayToken flag becomes invalid input on
// import (ImportLegacyPTYTemplates).
var ErrRelayTokenSubstitution = errors.New("template references ${RELAY_TOKEN}, which relay refuses to expand")

// ErrRelayEnvPassthrough is refused at validation: EnvPassthrough names a
// host environment variable to copy verbatim into the spawned child, and a
// name starting with "RELAY_" reaches relay's own bearer credentials
// (RELAY_PROJECT_TOKEN, its legacy alias RELAY_TOKEN, RELAY_LLM_HOOK_TOKEN)
// the same way relayLLM's ChildBaseEnv() strip-list is defeated by the
// analogous mechanism in internal/terminal/terminal_session.go — a template
// doesn't set the value here, it only names which host-process variable to
// copy, so refusing the whole RELAY_ prefix is the only check that can't be
// bypassed by picking a name the strip-list doesn't yet know about. None of
// relay's own env injections are things a template legitimately wants to
// "pass through" from the host — they're set at spawn time, not read from
// the parent's environment — so there is no legitimate RELAY_-prefixed name
// to carve out of this check.
var ErrRelayEnvPassthrough = errors.New("template passes through a RELAY_-prefixed environment variable")

const relayTokenMarker = "${RELAY_TOKEN}"

// ValidateTerminalTemplate refuses a template whose argv or env contains
// the literal substring ${RELAY_TOKEN}, or whose EnvPassthrough names a
// RELAY_-prefixed variable. This is the one security-relevant check in this
// file — see the package doc on TerminalTemplate for why: a bearer
// credential must never reach a spawned child's environment or argv, and
// ExpandTemplateVars only ever substitutes ${PROJECT_PATH} and
// ${PROJECT_ID}, so a template that depends on RELAY_TOKEN expansion can
// never actually get it — it must be refused up front instead of launching
// with the literal text still in place.
func ValidateTerminalTemplate(t TerminalTemplate) error {
	if strings.TrimSpace(t.ID) == "" {
		return fmt.Errorf("terminal template: id is required")
	}
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("terminal template %q: name is required", t.ID)
	}
	if strings.Contains(t.Command, relayTokenMarker) {
		return fmt.Errorf("terminal template %q: %w (in command)", t.ID, ErrRelayTokenSubstitution)
	}
	for _, a := range t.Args {
		if strings.Contains(a, relayTokenMarker) {
			return fmt.Errorf("terminal template %q: %w (in args)", t.ID, ErrRelayTokenSubstitution)
		}
	}
	for k, v := range t.Env {
		if strings.Contains(v, relayTokenMarker) {
			return fmt.Errorf("terminal template %q: %w (in env %q)", t.ID, ErrRelayTokenSubstitution, k)
		}
	}
	for _, name := range t.EnvPassthrough {
		if strings.HasPrefix(name, "RELAY_") {
			return fmt.Errorf("terminal template %q: %w (%q)", t.ID, ErrRelayEnvPassthrough, name)
		}
	}
	return nil
}

// ExpandTemplateVars substitutes ${PROJECT_PATH} and ${PROJECT_ID} into in.
// Nothing else expands — an unknown token, including a stale
// ${RELAY_TOKEN} that reached this function by some route
// ValidateTerminalTemplate didn't catch, is left as the literal text it
// already was (strings.Replacer only substitutes tokens it knows).
func ExpandTemplateVars(in, projectPath, projectID string) string {
	r := strings.NewReplacer(
		"${PROJECT_PATH}", projectPath,
		"${PROJECT_ID}", projectID,
	)
	return r.Replace(in)
}

// hydrateTerminalTemplate fills in BuiltIn from the id, the same computed
// -not-stored shape relayLLM's TemplateStore.hydrate used.
func hydrateTerminalTemplate(t TerminalTemplate) TerminalTemplate {
	t.BuiltIn = builtinTerminalTemplateIDs[t.ID]
	return t
}

// EffectiveTerminalTemplates overlays Settings.TerminalTemplates on the
// built-in set: an entry sharing a built-in's ID replaces it, any other ID
// is appended. An install that has never customized a template keeps
// settings.json byte-identical to one written before this feature existed,
// because the built-ins here are never persisted — only an override is.
// An override that fails ValidateTerminalTemplate (e.g. a hand-edited
// settings.json reintroducing ${RELAY_TOKEN}) is refused at resolution
// rather than served, and logged.
func EffectiveTerminalTemplates(s *Settings) []TerminalTemplate {
	byID := make(map[string]TerminalTemplate, len(builtinTerminalTemplateIDs)+len(s.TerminalTemplates))
	for _, t := range BuiltinTerminalTemplates() {
		byID[t.ID] = t
	}
	for _, t := range s.TerminalTemplates {
		if err := ValidateTerminalTemplate(t); err != nil {
			slog.Warn("terminal template refused at resolution", "id", t.ID, "error", err)
			continue
		}
		byID[t.ID] = t
	}
	return sortedHydratedTemplates(byID)
}

// GetTerminalTemplate looks up one template by id from the effective set.
func GetTerminalTemplate(s *Settings, id string) (TerminalTemplate, bool) {
	for _, t := range EffectiveTerminalTemplates(s) {
		if t.ID == id {
			return t, true
		}
	}
	return TerminalTemplate{}, false
}

// EffectiveTerminalTemplatesForProject overlays a project's own
// ShellTemplates (internal/project's per-project override — a project can
// carry private shells, e.g. an ssh alias, not shared elsewhere) on top of
// EffectiveTerminalTemplates: same override-by-id shape one level down. A
// project-scoped entry never inherits BuiltIn even when its id collides
// with a global built-in's — a project that overrides "shell" is no longer
// serving relay's own shell template. proj may be nil (no project scope).
func EffectiveTerminalTemplatesForProject(s *Settings, proj *Project) []TerminalTemplate {
	base := EffectiveTerminalTemplates(s)
	if proj == nil || len(proj.ShellTemplates) == 0 {
		return base
	}
	byID := make(map[string]TerminalTemplate, len(base)+len(proj.ShellTemplates))
	for _, t := range base {
		byID[t.ID] = t
	}
	for _, st := range proj.ShellTemplates {
		t := TerminalTemplate{
			ID:          st.ID,
			Name:        st.Name,
			Command:     st.Command,
			Args:        st.Args,
			Env:         st.Env,
			Description: st.Description,
			Icon:        st.Icon,
		}
		// ShellTemplate has no Sandbox field of its own (see its doc
		// comment), so an override sharing a shadowed entry's id would
		// otherwise silently reset Sandbox to false via the zero value.
		// Carry the shadowed entry's Sandbox forward -- it always wins, an
		// operator has no way to clear it through a project override.
		// Deliberately NOT done for ModelKey: inheriting it here would let
		// a project override widen a template's authority (grant a model
		// key the shadowed entry didn't have), the opposite of what this
		// carries-forward is for.
		if base, ok := byID[st.ID]; ok {
			t.Sandbox = base.Sandbox
		}
		if err := ValidateTerminalTemplate(t); err != nil {
			slog.Warn("project shell template refused at resolution", "project", proj.ID, "id", st.ID, "error", err)
			continue
		}
		byID[st.ID] = t
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]TerminalTemplate, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func sortedHydratedTemplates(byID map[string]TerminalTemplate) []TerminalTemplate {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]TerminalTemplate, 0, len(ids))
	for _, id := range ids {
		out = append(out, hydrateTerminalTemplate(byID[id]))
	}
	return out
}

// legacyPTYTemplate mirrors relayLLM's config.TerminalTemplate JSON shape
// (relayLLM internal/config/terminal.go) closely enough to unmarshal its
// settings.json `pty` map directly. UseRelayToken is read only to be
// dropped — see ImportLegacyPTYTemplates.
type legacyPTYTemplate struct {
	Name           string            `json:"name"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Description    string            `json:"description,omitempty"`
	Icon           string            `json:"icon,omitempty"`
	IdleTimeout    int               `json:"idleTimeout,omitempty"`
	UseRelayToken  bool              `json:"useRelayToken,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
}

// ImportLegacyPTYTemplates converts relayLLM's settings.json `pty` map
// (id -> legacy template JSON) into relay's schema, one time. UseRelayToken
// is dropped, never converted into an equivalent: a legacy template that
// relied on it becomes invalid input only if its argv/env still contains
// the literal ${RELAY_TOKEN} text (ValidateTerminalTemplate) — the flag
// itself just has no field to land in on this side.
func ImportLegacyPTYTemplates(pty map[string]json.RawMessage) ([]TerminalTemplate, error) {
	ids := make([]string, 0, len(pty))
	for id := range pty {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]TerminalTemplate, 0, len(pty))
	for _, id := range ids {
		var legacy legacyPTYTemplate
		if err := json.Unmarshal(pty[id], &legacy); err != nil {
			return nil, fmt.Errorf("import terminal template %q: %w", id, err)
		}
		t := TerminalTemplate{
			ID:             id,
			Name:           legacy.Name,
			Command:        legacy.Command,
			Args:           legacy.Args,
			Env:            legacy.Env,
			Description:    legacy.Description,
			Icon:           legacy.Icon,
			IdleTimeout:    legacy.IdleTimeout,
			EnvPassthrough: legacy.EnvPassthrough,
		}
		if err := ValidateTerminalTemplate(t); err != nil {
			return nil, fmt.Errorf("import terminal template %q: %w", id, err)
		}
		out = append(out, t)
	}
	return out, nil
}
