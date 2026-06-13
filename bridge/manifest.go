package bridge

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Manifest declares how an enhanced service plugs into relay's front door
// and settings UI. Carried over the bridge as the payload of a
// ReqRegisterManifest. Intentionally minimal — see service-manifest-spec.md.
//
// Keep additions backward-compatible (new optional fields are fine; renaming
// or removing fields breaks every implementor).
type Manifest struct {
	// Routes are the HTTP path prefixes and exact paths the service serves.
	// Relay's front-door dispatcher uses longest-prefix-match against this
	// list to pick which service handles an inbound request. WebSocket paths
	// (e.g. "/ws") are valid entries.
	Routes []string `json:"routes"`

	// Status declares the GET endpoint relay polls to render the service's
	// status in the settings UI. Optional — services with no status surface
	// can omit it.
	Status *StatusDecl `json:"status,omitempty"`

	// Actions are user-triggerable RPCs that surface as buttons in the
	// settings UI. Optional.
	Actions []ActionDecl `json:"actions,omitempty"`

	// Config declares a single config file the service wants relay to expose
	// for editing in the settings UI, plus the schema relay renders an editor
	// from. Relay reads and writes the file directly from the tray process —
	// the service hosts no endpoint for this. Optional.
	Config *ConfigDecl `json:"config,omitempty"`
}

// StatusDecl is the read-only status endpoint relay polls for the service.
// Response body is free-form JSON; the UI renders it generically.
type StatusDecl struct {
	Path string `json:"path"`
}

// ActionDecl is a single user-triggerable RPC. PathTemplate may contain
// `{key}` placeholders that the UI substitutes from a row in the status
// response (driving per-row action buttons in array-shaped status payloads).
type ActionDecl struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
	// ForEach names a top-level array key in the service's status response.
	// When set, the UI renders one button per row in that array and
	// substitutes the row's keys into PathTemplate's {placeholders}.
	// Empty = single global button with no substitution.
	ForEach string `json:"forEach,omitempty"`
}

// ConfigDecl declares one editable config file plus the schema relay uses to
// render an editor for it. Carried in the Manifest at RegisterManifest time.
// Relay reads and writes the file directly (the service hosts no endpoint),
// treating the bytes as opaque text on the wire; Schema drives the settings-UI
// form. The service is restarted to apply unless ApplyMode is "live".
//
// Path is validated here as absolute + no ".." segment (schema-level only).
// Relay re-validates it against an allowed root and a regular-file check at use
// time — a service-declared path is never trusted blindly, even though
// RegisterManifest is service-token authenticated.
type ConfigDecl struct {
	Path      string      `json:"path"`                // absolute path to the config file (required)
	Format    string      `json:"format,omitempty"`    // "jsonc" (default) | "json"
	Label     string      `json:"label,omitempty"`     // UI header, e.g. "settings.json"
	Help      string      `json:"help,omitempty"`      // one-line description under the header
	ApplyMode string      `json:"applyMode,omitempty"` // "restart" (default) | "live"
	Schema    []FieldDecl `json:"schema"`              // top-level fields of the config object
}

// FieldDecl describes one node in a config schema. Leaf types render a single
// input; the recursive types (object/array/map) nest. The same declaration
// drives both the form layout and the harvest/serialize back to JSON in the UI.
type FieldDecl struct {
	ID          string   `json:"id"`                    // JSON key in the enclosing object
	Label       string   `json:"label,omitempty"`       // human-readable label
	Type        string   `json:"type"`                  // see field types below
	Help        string   `json:"help,omitempty"`        // help text under the input (replaces JSONC comments)
	Placeholder string   `json:"placeholder,omitempty"` // input placeholder
	Required    bool     `json:"required,omitempty"`    // required (non-empty) leaf
	ReadOnly    bool     `json:"readOnly,omitempty"`    // rendered disabled
	Secret      bool     `json:"secret,omitempty"`      // mask the input (e.g. apiKey)
	Options     []string `json:"options,omitempty"`     // allowed values for type "select"

	// Recursive shapes — exactly one applies, selected by Type:
	Fields   []FieldDecl `json:"fields,omitempty"`   // type "object": named child fields
	Item     *FieldDecl  `json:"item,omitempty"`     // type "array"/"map": schema of each element/value
	KeyLabel string      `json:"keyLabel,omitempty"` // type "map"/"keyValue": label for the user-chosen key

	// Rest applies only to a "keyValue" field declared inside an "object". When
	// true the editor binds it to ALL of the parent object's keys except the
	// other declared sibling fields (an "everything else" key/value editor —
	// e.g. llama-server model flags, which sit as siblings of "alias"). Without
	// Rest, a "keyValue" field owns its own nested object at its own key.
	Rest bool `json:"rest,omitempty"`
}

// Field types recognized by the config-schema renderer.
const (
	// Leaves.
	FieldTypeText      = "text"      // single-line string
	FieldTypeTextarea  = "textarea"  // multi-line string
	FieldTypeBool      = "bool"      // checkbox / toggle
	FieldTypeNumber    = "number"    // numeric input
	FieldTypeSelect    = "select"    // dropdown over Options
	FieldTypeSecret    = "secret"    // masked single-line string
	FieldTypeStringArr = "string[]"  // textarea, one entry per line, stored as []string
	FieldTypeStringMap = "stringMap" // textarea, KEY=VALUE per line, stored as map[string]string
	FieldTypeKeyValue  = "keyValue"  // repeater of key/value rows; values typed (bool/number/string)
	FieldTypeJSON      = "json"      // raw-JSON textarea — escape hatch for irregular sub-trees

	// Recursive.
	FieldTypeObject = "object" // fixed set of named child Fields
	FieldTypeArray  = "array"  // repeatable list, each element typed by Item
	FieldTypeMap    = "map"    // user-keyed collection, each value typed by Item
)

// ConfigDecl.Format values.
const (
	ConfigFormatJSONC = "jsonc"
	ConfigFormatJSON  = "json"
)

// ConfigDecl.ApplyMode values.
const (
	ConfigApplyRestart = "restart"
	ConfigApplyLive    = "live"
)

// RegisterManifestRequest is the Arguments payload for a ReqRegisterManifest
// bridge call. Sent by an enhanced service on startup, after its internal
// listener is bound and ready to serve traffic.
//
// The service picks InternalSocket and InternalToken itself and tells relay
// both — relay never dictates them. The bridge connection is already
// authenticated with the service's MCP token, so relay trusts the declared
// values as defense-in-depth on top of socket FS permissions.
type RegisterManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// Validate checks the registration request: the service-declared internal
// socket and token, plus the manifest itself. Conflict detection against
// other registered manifests is the relay router's job — this only validates
// the request in isolation.
func (r *RegisterManifestRequest) Validate() error {
	if r.ServiceID == "" {
		return fmt.Errorf("register_manifest: serviceId is empty")
	}
	if r.InternalSocket == "" {
		return fmt.Errorf("register_manifest: internalSocket is empty")
	}
	if r.InternalToken == "" {
		return fmt.Errorf("register_manifest: internalToken is empty")
	}
	return r.Manifest.Validate()
}

// Validate checks the manifest for schema-level errors. Conflict detection
// against other registered manifests is the relay router's job — this only
// validates the manifest in isolation.
func (m *Manifest) Validate() error {
	if len(m.Routes) == 0 {
		return fmt.Errorf("manifest: routes is empty")
	}
	seen := make(map[string]bool, len(m.Routes))
	for i, r := range m.Routes {
		if r == "" {
			return fmt.Errorf("manifest: routes[%d] is empty", i)
		}
		if !strings.HasPrefix(r, "/") {
			return fmt.Errorf("manifest: routes[%d] %q must start with %q", i, r, "/")
		}
		if seen[r] {
			return fmt.Errorf("manifest: routes[%d] %q is duplicated", i, r)
		}
		seen[r] = true
	}
	if m.Status != nil {
		if !strings.HasPrefix(m.Status.Path, "/") {
			return fmt.Errorf("manifest: status.path %q must start with %q", m.Status.Path, "/")
		}
	}
	actionIDs := make(map[string]bool, len(m.Actions))
	for i, a := range m.Actions {
		if a.ID == "" {
			return fmt.Errorf("manifest: actions[%d].id is empty", i)
		}
		if actionIDs[a.ID] {
			return fmt.Errorf("manifest: actions[%d].id %q is duplicated", i, a.ID)
		}
		actionIDs[a.ID] = true
		if a.Label == "" {
			return fmt.Errorf("manifest: actions[%d] (%q): label is empty", i, a.ID)
		}
		// Normalize the verb in place so downstream HTTP dispatch (which uses
		// action.Method verbatim) issues a canonical upper-case verb. A manifest
		// declaring "get" must not reach the wire as a literal "get" that servers
		// won't match.
		method := strings.ToUpper(a.Method)
		switch method {
		case "GET", "POST", "PUT", "DELETE", "PATCH":
			m.Actions[i].Method = method
		default:
			return fmt.Errorf("manifest: actions[%d] (%q): method %q is not a supported HTTP verb", i, a.ID, a.Method)
		}
		if !strings.HasPrefix(a.PathTemplate, "/") {
			return fmt.Errorf("manifest: actions[%d] (%q): pathTemplate %q must start with %q", i, a.ID, a.PathTemplate, "/")
		}
	}
	if m.Config != nil {
		if err := m.Config.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate checks a ConfigDecl's schema-level invariants. Filesystem checks
// (regular file, allowed root, size) happen in relay at use time — a manifest
// is validated in isolation.
func (c *ConfigDecl) validate() error {
	if c.Path == "" {
		return fmt.Errorf("manifest: config.path is empty")
	}
	if !filepath.IsAbs(c.Path) {
		return fmt.Errorf("manifest: config.path %q must be absolute", c.Path)
	}
	for _, seg := range strings.Split(c.Path, string(filepath.Separator)) {
		if seg == ".." {
			return fmt.Errorf("manifest: config.path %q must not contain a %q segment", c.Path, "..")
		}
	}
	switch c.Format {
	case "", ConfigFormatJSONC, ConfigFormatJSON:
	default:
		return fmt.Errorf("manifest: config.format %q is not supported", c.Format)
	}
	switch c.ApplyMode {
	case "", ConfigApplyRestart, ConfigApplyLive:
	default:
		return fmt.Errorf("manifest: config.applyMode %q is not supported", c.ApplyMode)
	}
	if len(c.Schema) == 0 {
		return fmt.Errorf("manifest: config.schema is empty")
	}
	return validateFields("config.schema", c.Schema)
}

// validateFields recursively validates a list of sibling field declarations.
// label is a dotted path used only in error messages.
func validateFields(label string, fields []FieldDecl) error {
	seen := make(map[string]bool, len(fields))
	for i := range fields {
		f := &fields[i]
		if f.ID == "" {
			return fmt.Errorf("manifest: %s[%d].id is empty", label, i)
		}
		if seen[f.ID] {
			return fmt.Errorf("manifest: %s: field id %q is duplicated", label, f.ID)
		}
		seen[f.ID] = true
		if err := f.validate(label + "." + f.ID); err != nil {
			return err
		}
	}
	return nil
}

// validate checks one FieldDecl and recurses into object/array/map shapes.
func (f *FieldDecl) validate(label string) error {
	switch f.Type {
	case FieldTypeText, FieldTypeTextarea, FieldTypeBool, FieldTypeNumber,
		FieldTypeSecret, FieldTypeStringArr, FieldTypeStringMap, FieldTypeKeyValue, FieldTypeJSON:
		// scalar / dynamic-map leaves — nothing further to validate
	case FieldTypeSelect:
		if len(f.Options) == 0 {
			return fmt.Errorf("manifest: %s: select field requires options", label)
		}
	case FieldTypeObject:
		if len(f.Fields) == 0 {
			return fmt.Errorf("manifest: %s: object field requires fields", label)
		}
		return validateFields(label, f.Fields)
	case FieldTypeArray, FieldTypeMap:
		if f.Item == nil {
			return fmt.Errorf("manifest: %s: %s field requires item", label, f.Type)
		}
		return f.Item.validate(label + "[]")
	default:
		return fmt.Errorf("manifest: %s: type %q is not supported", label, f.Type)
	}
	return nil
}
