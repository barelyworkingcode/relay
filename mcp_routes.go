package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

// mcpView is an explicit allow-list, not ExternalMcp marshaled directly:
// ExternalMcp carries OAuthState (client secret, access/refresh tokens), and
// none of it may ever ride out over this API. Add a field here only if it is
// safe to hand to whoever holds the frontend bearer token.
type mcpView struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Transport   string            `json:"transport,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	TccServices []string          `json:"tcc_services,omitempty"`
	// AuthRequired is set on create when the MCP answered 401 during
	// discovery. The record still landed (see McpOps.Add) -- this rides on
	// the 2xx to tell the caller OAuth is the next step, since authenticate
	// itself has no HTTP route to point at.
	AuthRequired bool `json:"auth_required,omitempty"`
}

func mcpViewOf(m ExternalMcp) mcpView {
	return mcpView{
		ID:          m.ID,
		DisplayName: m.DisplayName,
		Transport:   m.Transport,
		Command:     m.Command,
		Args:        m.Args,
		Env:         m.Env,
		URL:         m.URL,
		TccServices: m.TccServices,
	}
}

func mcpHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errMcpNotFound):
		return http.StatusNotFound
	case errors.Is(err, errMcpInvalid):
		return http.StatusBadRequest
	case errors.Is(err, errMcpDiscovery):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func writeMcpError(w http.ResponseWriter, err error) {
	writeJSON(w, mcpHTTPStatus(err), map[string]string{"error": err.Error()})
}

// RegisterMcpRoutes adds only the two MCP mutations (ADR-014 MCP-MUTATIONS
// slice). GET /api/mcps, GET /api/mcps/{id}/tools and
// GET /api/mcps/{id}/scope_fields already live on this mux via
// RegisterProjectRoutes; this does not duplicate or move them.
//
// There is deliberately no route for authenticate or reset-permissions --
// see McpOps.StartOAuth and McpOps.ResetPermissions for why neither can be
// implemented behind an API call (ADR-014 section 4).
//
// ops is the same instance the settings UI's IPC handlers use, so an MCP
// registered from curl and one registered from the tray share validation,
// the SSRF guard, and discovery.
func RegisterMcpRoutes(rr *RouteRegistrar, ops *McpOps) {
	// execute: a stdio MCP's `command` is the caller's choice of what relay
	// runs (ADR-015 decision 1) — the same reasoning as service create.
	rr.Handle(ClassExecute, "POST /api/mcps", func(w http.ResponseWriter, r *http.Request) {
		var body mcpFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		created, err := ops.Add(body)
		if err != nil && !errors.Is(err, ErrAuthRequired) {
			writeMcpError(w, err)
			return
		}
		view := mcpViewOf(created)
		view.AuthRequired = errors.Is(err, ErrAuthRequired)
		writeJSON(w, http.StatusCreated, view)
	})

	rr.Handle(ClassConfigure, "DELETE /api/mcps/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.Remove(r.PathValue("id")); err != nil {
			writeMcpError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
