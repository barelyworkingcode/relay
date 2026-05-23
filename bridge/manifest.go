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
}

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
	return nil
}
