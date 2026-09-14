package bridge

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Manifest declares how an enhanced service plugs into relay's front door
// and settings UI (payload of ReqRegisterManifest; see service-manifest-spec.md).
// Keep additions backward-compatible: renaming or removing a field breaks
// every implementor.
type Manifest struct {
	Routes []string `json:"routes"`

	Status *StatusDecl `json:"status,omitempty"`

	Actions []ActionDecl `json:"actions,omitempty"`

	Config *ConfigDecl `json:"config,omitempty"`
}

type StatusDecl struct {
	Path string `json:"path"`
}

type ActionDecl struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
	ForEach      string `json:"forEach,omitempty"`
}

// ConfigDecl declares one editable config file plus the schema relay uses to
// render an editor for it. Path is validated here as absolute + no ".."
// (schema-level only) — relay re-validates against an allowed root and a
// regular-file check at use time, since a service-declared path is never
// trusted blindly even though registration is authenticated by a launch identity
// holding the manifest capability.
type ConfigDecl struct {
	Path      string      `json:"path"`
	Format    string      `json:"format,omitempty"`
	Label     string      `json:"label,omitempty"`
	Help      string      `json:"help,omitempty"`
	ApplyMode string      `json:"applyMode,omitempty"`
	Schema    []FieldDecl `json:"schema"`
}

type FieldDecl struct {
	ID          string   `json:"id"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type"`
	Help        string   `json:"help,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
	ReadOnly    bool     `json:"readOnly,omitempty"`
	Secret      bool     `json:"secret,omitempty"`
	Options     []string `json:"options,omitempty"`

	// Recursive shapes: exactly one of these applies, selected by Type.
	Fields   []FieldDecl `json:"fields,omitempty"`
	Item     *FieldDecl  `json:"item,omitempty"`
	KeyLabel string      `json:"keyLabel,omitempty"`

	// Rest, on a "keyValue" field inside an "object", binds it to ALL of the
	// parent's keys except the other declared sibling fields, instead of its
	// own nested object.
	Rest bool `json:"rest,omitempty"`
}

const (
	FieldTypeText      = "text"
	FieldTypeTextarea  = "textarea"
	FieldTypeBool      = "bool"
	FieldTypeNumber    = "number"
	FieldTypeSelect    = "select"
	FieldTypeSecret    = "secret"
	FieldTypeStringArr = "string[]"
	FieldTypeStringMap = "stringMap"
	FieldTypeKeyValue  = "keyValue"
	FieldTypeJSON      = "json"

	FieldTypeObject = "object"
	FieldTypeArray  = "array"
	FieldTypeMap    = "map"
)

const (
	ConfigFormatJSONC = "jsonc"
	ConfigFormatJSON  = "json"
)

const (
	ConfigApplyRestart = "restart"
	ConfigApplyLive    = "live"
)

// RegisterManifestRequest is the Arguments payload for a ReqRegisterManifest
// call. The service picks InternalSocket and InternalToken itself; relay
// trusts the declared values as defense-in-depth on top of socket FS
// permissions, since the call is already authenticated by a launch identity
// holding the manifest capability.
type RegisterManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// Validate covers only the request in isolation; conflict detection against
// other manifests is the router's job.
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
		// A manifest declaring "get" must reach the wire as "GET", or dispatch
		// (which compares action.Method verbatim) won't match it.
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

func (f *FieldDecl) validate(label string) error {
	switch f.Type {
	case FieldTypeText, FieldTypeTextarea, FieldTypeBool, FieldTypeNumber,
		FieldTypeSecret, FieldTypeStringArr, FieldTypeStringMap, FieldTypeKeyValue, FieldTypeJSON:
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
