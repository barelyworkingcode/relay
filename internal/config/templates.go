package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// TerminalTemplate is relay's own launch config for a PTY terminal — the
// data relayLLM's internal/terminal/terminal_template.go used to own before
// session-host moved launching into relay (SH §terminal templates).
// Settings.TerminalTemplates is the complete set: nothing is computed in code,
// so every template, including the ones relay seeds on first start, is an
// entry in settings.json that can be changed or removed.
//
// There is deliberately no UseRelayToken field. relayLLM's template could
// interpolate ${RELAY_TOKEN} into a spawned child's argv/env; that is a
// bearer credential in a place any same-uid process can read via
// KERN_PROCARGS2, and it is retired, not ported (ValidateTerminalTemplate,
// ExpandTemplateVars).
//
// The one credential a template may place in its env is the launch's own
// model key, spelled ${MODEL_KEY} (ModelKeyMarker), and only when the
// template opts in with model_key: true. That reverses the stance above for
// a much lower-value credential on purpose: it is a per-session key that
// reaches relay's model endpoint only, scoped to the launching project's
// allowed_models and dead when the session's launch ends, where a project
// token reaches every tool the project holds. The exposure to same-uid
// processes is the same one RELAY_PROJECT_TOKEN already has. A template with
// no ${MODEL_KEY} mapping gets no key in its env at all: nothing sets a
// default variable for it.
type TerminalTemplate struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
	IdleTimeout int               `json:"idleTimeout,omitempty"` // minutes, 0 = default (1440 = 24h)

	// EnvPassthrough names host environment variables copied into the
	// child's env verbatim (e.g. provider API keys claude-code picks up).
	// Never a place a relay credential belongs — spawn-time enforcement of
	// that is R-S4a/R-S4b's launch path, not this package.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	// Sandbox is the template's opt-in to C7 seatbelt confinement. When it is
	// set, relay builds the session's profile from the launch's project and
	// the folders below. Absent means unsandboxed, so a template that should
	// be confined says so.
	Sandbox bool `json:"sandbox,omitempty"`

	// Read and ReadWrite are the folders a sandboxed launch of this template
	// may reach, and the only ones: file access is denied by default, so a
	// folder that is not listed here (or the launching project's own
	// directory, which is always read-write, or the fixed system baseline in
	// internal/sessions/sandbox) is unreachable. Each entry is an absolute
	// path or starts with `~`. A directory grants its whole subtree; a
	// regular file grants that file, and a read-write file also grants the
	// atomic-write siblings a CLI leaves beside it (`.lock`, `.tmp.*`,
	// `.backup`). Ignored when Sandbox is false.
	Read      []string `json:"read,omitempty"`
	ReadWrite []string `json:"read_write,omitempty"`

	// Deny lists paths a sandboxed launch cannot reach at all, whatever the
	// grants above (or the project directory) say: no read, write or stat.
	// It exists to carve a hole out of a wide grant, such as `~/.ssh` under a
	// `~` read-write. Same entry shape as Read and ReadWrite. Ignored when
	// Sandbox is false.
	Deny []string `json:"deny,omitempty"`

	// ModelKey opts a pty template into a minted model-broker key at launch
	// (C8), the way the `pi` template does so its interactive CLI reaches
	// relay's model endpoint. Distinct from C5's LaunchSpec `model_key`
	// field, which carries the minted secret itself — this is only the
	// template's declaration that it wants one.
	//
	// Minting a key does not deliver it. It reaches the child's env only
	// through a ${MODEL_KEY} the template's own Env values name (for example
	// ANTHROPIC_CUSTOM_HEADERS: "X-Relay-Key: ${MODEL_KEY}"), expanded by
	// relay-sessions at spawn.
	ModelKey bool `json:"model_key,omitempty"`
}

// DefaultShellTemplate is the one template relay writes into settings.json
// when it holds none (EnsureDefaultTerminalTemplates): the system shell,
// sandboxed, with read-write access to the home directory. That is a wide
// grant on purpose, and it includes credential directories such as ~/.ssh: an
// operator narrows it by editing the template's folders.
func DefaultShellTemplate() TerminalTemplate {
	return TerminalTemplate{
		ID:          "shell",
		Name:        "Shell",
		Icon:        "shell",
		Description: "Default system shell",
		Sandbox:     true,
		ReadWrite:   []string{"~"},
	}
}

// EnsureDefaultTerminalTemplates writes DefaultShellTemplate into s iff s holds
// no template at all, and reports whether it did. Relay calls it once at start
// (through WithDeclinable, so an install that already has templates is not
// rewritten): the default lives in settings.json where it can be seen and
// edited, not in code where it could not. It re-fires whenever the list is
// empty, so an operator cannot leave relay with zero templates.
func EnsureDefaultTerminalTemplates(s *Settings) bool {
	if len(s.TerminalTemplates) > 0 {
		return false
	}
	s.TerminalTemplates = []TerminalTemplate{DefaultShellTemplate()}
	return true
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

// ModelKeyMarker is the one designated credential substitution in a template:
// the launch's own model key. Valid only in Env values, and only in a
// template with model_key: true (ValidateTerminalTemplate); relay-sessions
// expands it at spawn (internal/sessions/terminal mirrors this constant and
// does not import this package).
const ModelKeyMarker = "${MODEL_KEY}"

// ModelEndpointURLMarker is the second designated substitution: the URL of
// relay's model endpoint listener, `http://<model_endpoint.listen>`. Relay
// expands it at launch, in env values only, and a launch of a template that
// uses it is refused when the listener is off (cmd/relay resolveTemplateEnv):
// leaving it unexpanded would point a client at a literal placeholder, and
// dropping only the URL while keeping a ${MODEL_KEY} header would send the
// relay key to the client's real provider.
const ModelEndpointURLMarker = "${MODEL_ENDPOINT_URL}"

// ErrModelEndpointSubstitution is refused at validation: ${MODEL_ENDPOINT_URL}
// in a template's command or args, where a URL has no business being expanded.
var ErrModelEndpointSubstitution = errors.New("template uses ${MODEL_ENDPOINT_URL} outside its env")

// ErrTemplateGrant is refused at validation: a sandbox folder that is not an
// absolute or ~ path, or that grants the whole filesystem.
var ErrTemplateGrant = errors.New("sandbox folder must be an absolute path or start with ~, and not be /")

// ErrModelKeySubstitution is refused at validation: ${MODEL_KEY} outside an
// env value, or in a template that did not opt in with model_key: true.
// Argv is the worse place for a credential (ps shows it to every user), and
// a template that names a key it will never be minted would launch with the
// literal placeholder text in its header.
var ErrModelKeySubstitution = errors.New("template uses ${MODEL_KEY} outside the env of a model_key template")

// ValidateTerminalTemplate refuses a template whose argv or env contains
// the literal substring ${RELAY_TOKEN}, whose EnvPassthrough names a
// RELAY_-prefixed variable, or that names ${MODEL_KEY} anywhere but the env
// of a model_key template. This is the one security-relevant check in this
// file — see the package doc on TerminalTemplate for why: a bearer
// credential must never reach a spawned child's environment or argv, and
// ExpandTemplateVars only ever substitutes ${PROJECT_PATH} and
// ${PROJECT_ID}, so a template that depends on RELAY_TOKEN expansion can
// never actually get it — it must be refused up front instead of launching
// with the literal text still in place. ${MODEL_KEY} is the one credential
// that is expanded, at spawn and not by ExpandTemplateVars, and is carved out
// only where that expansion happens.
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
	if strings.Contains(t.Command, ModelKeyMarker) {
		return fmt.Errorf("terminal template %q: %w (in command)", t.ID, ErrModelKeySubstitution)
	}
	for _, a := range t.Args {
		if strings.Contains(a, ModelKeyMarker) {
			return fmt.Errorf("terminal template %q: %w (in args)", t.ID, ErrModelKeySubstitution)
		}
	}
	if !t.ModelKey {
		for k, v := range t.Env {
			if strings.Contains(v, ModelKeyMarker) {
				return fmt.Errorf("terminal template %q: %w (in env %q; set model_key: true)", t.ID, ErrModelKeySubstitution, k)
			}
		}
	}
	if strings.Contains(t.Command, ModelEndpointURLMarker) {
		return fmt.Errorf("terminal template %q: %w (in command)", t.ID, ErrModelEndpointSubstitution)
	}
	for _, a := range t.Args {
		if strings.Contains(a, ModelEndpointURLMarker) {
			return fmt.Errorf("terminal template %q: %w (in args)", t.ID, ErrModelEndpointSubstitution)
		}
	}
	for _, list := range []struct {
		field string
		paths []string
	}{{"read", t.Read}, {"read_write", t.ReadWrite}, {"deny", t.Deny}} {
		for _, p := range list.paths {
			if err := validateGrantPath(p); err != nil {
				return fmt.Errorf("terminal template %q: %s %q: %w", t.ID, list.field, p, err)
			}
		}
	}
	return nil
}

// validateGrantPath is the shape check for one sandbox folder. Expansion of
// `~` needs the launching user's home, so it happens at launch; this only
// refuses what can never be placed: a relative path, `~user`, an empty entry,
// and the whole filesystem, which would make the sandbox decoration.
func validateGrantPath(p string) error {
	if p == "" {
		return ErrTemplateGrant
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !filepath.IsAbs(p) {
		return ErrTemplateGrant
	}
	if filepath.Clean(p) == "/" {
		return ErrTemplateGrant
	}
	return nil
}

// ExpandTemplateVars substitutes ${PROJECT_PATH} and ${PROJECT_ID} into in.
// Nothing else expands — an unknown token, including a stale
// ${RELAY_TOKEN} that reached this function by some route
// ValidateTerminalTemplate didn't catch, is left as the literal text it
// already was (strings.Replacer only substitutes tokens it knows).
// ${MODEL_KEY} is deliberately not here: it needs the minted key, which does
// not exist yet when a template's argv is resolved, and it applies to env
// values only (ModelKeyMarker).
func ExpandTemplateVars(in, projectPath, projectID string) string {
	r := strings.NewReplacer(
		"${PROJECT_PATH}", projectPath,
		"${PROJECT_ID}", projectID,
	)
	return r.Replace(in)
}

// EffectiveTerminalTemplates is Settings.TerminalTemplates, sorted by id, minus
// any entry that fails ValidateTerminalTemplate (a hand-edited settings.json
// reintroducing ${RELAY_TOKEN}, or a relative sandbox folder) — refused at
// resolution rather than served, and logged. Nothing is added from code.
func EffectiveTerminalTemplates(s *Settings) []TerminalTemplate {
	byID := make(map[string]TerminalTemplate, len(s.TerminalTemplates))
	for _, t := range s.TerminalTemplates {
		if err := ValidateTerminalTemplate(t); err != nil {
			slog.Warn("terminal template refused at resolution", "id", t.ID, "error", err)
			continue
		}
		byID[t.ID] = t
	}
	return sortedTemplates(byID)
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

// AllowsTemplate reports whether p may launch the template with this id: a
// lone "*" allows every template, an empty list none. A nil project (no
// project scope) allows nothing.
func (p *Project) AllowsTemplate(id string) bool {
	if p == nil {
		return false
	}
	return IsWildcard(p.AllowedTemplates) || slices.Contains(p.AllowedTemplates, id)
}

// EffectiveTerminalTemplatesForProject is EffectiveTerminalTemplates narrowed
// to the templates proj may launch (Project.AllowedTemplates).
func EffectiveTerminalTemplatesForProject(s *Settings, proj *Project) []TerminalTemplate {
	all := EffectiveTerminalTemplates(s)
	return slices.DeleteFunc(all, func(t TerminalTemplate) bool { return !proj.AllowsTemplate(t.ID) })
}

// ErrHostTemplateSandbox is refused at validation: a host template that opts
// into sandboxing or names sandbox folders. The seatbelt profile is built and
// applied on the console, so it would confine nothing on the host.
var ErrHostTemplateSandbox = errors.New("host template cannot be sandboxed")

// ErrHostTemplateEnvPassthrough is refused at validation: env_passthrough
// copies variables from relay's own environment, which is the console's, not
// the host's.
var ErrHostTemplateEnvPassthrough = errors.New("host template cannot pass through console environment variables")

// ErrHostTemplateModelKey is refused at validation: model_key or ${MODEL_KEY}
// in a host template's env. A host has no console model socket to reach with
// the key.
var ErrHostTemplateModelKey = errors.New("host template cannot use ${MODEL_KEY}")

// ValidateHostTemplate is ValidateTerminalTemplate plus the refusals that
// follow from a host template running on the host: no sandbox, no sandbox
// folders, no env passthrough, and no model_key or ${MODEL_KEY}.
func ValidateHostTemplate(t TerminalTemplate) error {
	if err := ValidateTerminalTemplate(t); err != nil {
		return err
	}
	if t.Sandbox {
		return fmt.Errorf("terminal template %q: %w (sandbox)", t.ID, ErrHostTemplateSandbox)
	}
	if len(t.Read) > 0 {
		return fmt.Errorf("terminal template %q: %w (read)", t.ID, ErrHostTemplateSandbox)
	}
	if len(t.ReadWrite) > 0 {
		return fmt.Errorf("terminal template %q: %w (read_write)", t.ID, ErrHostTemplateSandbox)
	}
	if len(t.EnvPassthrough) > 0 {
		return fmt.Errorf("terminal template %q: %w", t.ID, ErrHostTemplateEnvPassthrough)
	}
	if t.ModelKey {
		return fmt.Errorf("terminal template %q: %w (model_key)", t.ID, ErrHostTemplateModelKey)
	}
	for k, v := range t.Env {
		if strings.Contains(v, ModelKeyMarker) {
			return fmt.Errorf("terminal template %q: %w (in env %q)", t.ID, ErrHostTemplateModelKey, k)
		}
	}
	return nil
}

// DefaultHostTemplates is what a successful probe seeds into a host that has
// no templates: the host's login shell (empty Command), and Claude Code at
// the probed path when the probe found one.
func DefaultHostTemplates(p HostProbe) []TerminalTemplate {
	out := []TerminalTemplate{{ID: "shell", Name: "Shell"}}
	if p.ClaudePath != "" {
		out = append(out, TerminalTemplate{ID: "claude-code", Name: "Claude Code", Command: p.ClaudePath})
	}
	return out
}

// TemplatesForProject is the template catalog proj may launch. A hosted
// project gets a copy of its host's templates, sorted by id, minus any that
// fail ValidateHostTemplate (logged) — never the console's, and none when the
// host is gone. Project.AllowedTemplates gates console templates only. Every
// other project gets EffectiveTerminalTemplatesForProject.
func TemplatesForProject(s *Settings, proj *Project) []TerminalTemplate {
	if proj == nil || !proj.IsHosted() {
		return EffectiveTerminalTemplatesForProject(s, proj)
	}
	h, _ := s.findHostByID(proj.HostID)
	if h == nil {
		return []TerminalTemplate{}
	}
	byID := make(map[string]TerminalTemplate, len(h.TerminalTemplates))
	for _, t := range h.TerminalTemplates {
		if err := ValidateHostTemplate(t); err != nil {
			slog.Warn("host terminal template refused at resolution", "host", h.ID, "id", t.ID, "error", err)
			continue
		}
		byID[t.ID] = cloneTerminalTemplate(t)
	}
	return sortedTemplates(byID)
}

func sortedTemplates(byID map[string]TerminalTemplate) []TerminalTemplate {
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
