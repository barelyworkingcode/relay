package bridge

import (
	"fmt"
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

	// Resources are typed record collections the service exposes for CRUD
	// management in the Service Inspector. Each ResourceDecl declares the
	// REST endpoints (list/create/update/delete) and a field schema; the
	// Inspector renders a generic table + form. Optional.
	Resources []ResourceDecl `json:"resources,omitempty"`
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

// ResourceDecl declares a typed collection the service manages. The Service
// Inspector renders one collapsible section per resource: a table of List()
// results plus optional Add/Edit forms generated from Fields. The Update and
// Delete endpoints' PathTemplate is substituted with the row's {id} (always
// the literal "id" field of each record).
//
// V1 deliberately omits search/pagination, server-defined options/enums, and
// nested objects. The minimal contract a service must satisfy:
//   - List returns a JSON array of objects.
//   - Create accepts an object body, returns the created object.
//   - Update accepts a partial object body, returns the updated object.
//   - Delete returns any 2xx on success.
type ResourceDecl struct {
	ID    string `json:"id"`              // stable identifier (e.g. "pty_templates")
	Label string `json:"label"`           // UI header (e.g. "Terminal Templates")
	Help  string `json:"help,omitempty"`  // optional one-line description shown under the header

	List   EndpointDecl  `json:"list"`
	Create *EndpointDecl `json:"create,omitempty"`
	Update *EndpointDecl `json:"update,omitempty"` // pathTemplate must contain {id}
	Delete *EndpointDecl `json:"delete,omitempty"` // pathTemplate must contain {id}

	// Fields drives both the table columns and the form layout. Each field's
	// ID is the JSON key inside a record. The order here is the display order.
	Fields []FieldDecl `json:"fields"`

	// ProtectedField, if set, names a boolean field on each record. Rows where
	// that field is true have Edit/Delete buttons suppressed in the UI. The
	// service is responsible for rejecting protected mutations server-side
	// too; this is only a UI affordance.
	ProtectedField string `json:"protectedField,omitempty"`
}

// EndpointDecl is one HTTP endpoint for a resource operation.
type EndpointDecl struct {
	Method       string `json:"method"`       // GET|POST|PUT|DELETE|PATCH
	PathTemplate string `json:"pathTemplate"` // may contain {id} for update/delete
}

// FieldDecl describes one field on a resource record. Used both for table
// display and for generating Add/Edit form inputs.
type FieldDecl struct {
	ID          string `json:"id"`                    // JSON key in the record
	Label       string `json:"label"`                 // human-readable label
	Type        string `json:"type"`                  // see field types below
	Placeholder string `json:"placeholder,omitempty"` // input placeholder
	Help        string `json:"help,omitempty"`        // help text under the input
	Required    bool   `json:"required,omitempty"`    // required on Create
	ReadOnly    bool   `json:"readOnly,omitempty"`    // shown in table, hidden in form
	HideInTable bool   `json:"hideInTable,omitempty"` // shown in form, hidden in table
}

// Field types recognized by the V1 Inspector renderer.
const (
	FieldTypeText      = "text"      // single-line string
	FieldTypeTextarea  = "textarea"  // multi-line string
	FieldTypeBool      = "bool"      // checkbox / toggle
	FieldTypeNumber    = "number"    // numeric input
	FieldTypeStringArr = "string[]"  // textarea, one entry per line, stored as []string
	FieldTypeStringMap = "stringMap" // textarea, KEY=VALUE per line, stored as map[string]string
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
		switch strings.ToUpper(a.Method) {
		case "GET", "POST", "PUT", "DELETE", "PATCH":
		default:
			return fmt.Errorf("manifest: actions[%d] (%q): method %q is not a supported HTTP verb", i, a.ID, a.Method)
		}
		if !strings.HasPrefix(a.PathTemplate, "/") {
			return fmt.Errorf("manifest: actions[%d] (%q): pathTemplate %q must start with %q", i, a.ID, a.PathTemplate, "/")
		}
	}
	resourceIDs := make(map[string]bool, len(m.Resources))
	for i, res := range m.Resources {
		if res.ID == "" {
			return fmt.Errorf("manifest: resources[%d].id is empty", i)
		}
		if resourceIDs[res.ID] {
			return fmt.Errorf("manifest: resources[%d].id %q is duplicated", i, res.ID)
		}
		resourceIDs[res.ID] = true
		if res.Label == "" {
			return fmt.Errorf("manifest: resources[%d] (%q): label is empty", i, res.ID)
		}
		if err := validateEndpoint("resources["+res.ID+"].list", res.List, false); err != nil {
			return err
		}
		if res.Create != nil {
			if err := validateEndpoint("resources["+res.ID+"].create", *res.Create, false); err != nil {
				return err
			}
		}
		if res.Update != nil {
			if err := validateEndpoint("resources["+res.ID+"].update", *res.Update, true); err != nil {
				return err
			}
		}
		if res.Delete != nil {
			if err := validateEndpoint("resources["+res.ID+"].delete", *res.Delete, true); err != nil {
				return err
			}
		}
		if len(res.Fields) == 0 {
			return fmt.Errorf("manifest: resources[%d] (%q): fields is empty", i, res.ID)
		}
		fieldIDs := make(map[string]bool, len(res.Fields))
		for j, f := range res.Fields {
			if f.ID == "" {
				return fmt.Errorf("manifest: resources[%d] (%q): fields[%d].id is empty", i, res.ID, j)
			}
			if fieldIDs[f.ID] {
				return fmt.Errorf("manifest: resources[%d] (%q): fields[%d].id %q is duplicated", i, res.ID, j, f.ID)
			}
			fieldIDs[f.ID] = true
			switch f.Type {
			case FieldTypeText, FieldTypeTextarea, FieldTypeBool, FieldTypeNumber, FieldTypeStringArr, FieldTypeStringMap:
			default:
				return fmt.Errorf("manifest: resources[%d] (%q): fields[%d] (%q): type %q is not supported", i, res.ID, j, f.ID, f.Type)
			}
		}
		if res.ProtectedField != "" && !fieldIDs[res.ProtectedField] {
			return fmt.Errorf("manifest: resources[%d] (%q): protectedField %q is not declared in fields", i, res.ID, res.ProtectedField)
		}
	}
	return nil
}

// validateEndpoint checks one EndpointDecl. requireIDPlaceholder is true for
// update/delete, which need a {id} substitution in their pathTemplate.
func validateEndpoint(label string, ep EndpointDecl, requireIDPlaceholder bool) error {
	switch strings.ToUpper(ep.Method) {
	case "GET", "POST", "PUT", "DELETE", "PATCH":
	default:
		return fmt.Errorf("manifest: %s.method %q is not a supported HTTP verb", label, ep.Method)
	}
	if !strings.HasPrefix(ep.PathTemplate, "/") {
		return fmt.Errorf("manifest: %s.pathTemplate %q must start with %q", label, ep.PathTemplate, "/")
	}
	if requireIDPlaceholder && !strings.Contains(ep.PathTemplate, "{id}") {
		return fmt.Errorf("manifest: %s.pathTemplate %q must contain {id}", label, ep.PathTemplate)
	}
	return nil
}
