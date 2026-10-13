package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// maxAgentBodyBytes bounds an agent entry request body.
const maxAgentBodyBytes = 64 << 10

// agentErrorBody is the coded error family; command and machine appear only on
// the lookup refusals.
type agentErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Command string `json:"command,omitempty"`
	Machine string `json:"machine,omitempty"`
}

// agentHTTPStatus is projectOpsHTTPStatus with one difference: an error no
// door classified is internal, not a caller mistake.
func agentHTTPStatus(err error) int {
	var ae *AgentError
	if errors.As(err, &ae) {
		return ae.Status
	}
	if status := projectOpsHTTPStatus(err); status != http.StatusBadRequest {
		return status
	}
	return http.StatusInternalServerError
}

// writeAgentError answers a coded refusal in the coded family and a presence,
// audit or internal failure in the Simple family.
func writeAgentError(w http.ResponseWriter, err error) {
	var ae *AgentError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, agentErrorBody{Error: ae.Code, Message: ae.Message, Command: ae.Command, Machine: ae.Machine})
		return
	}
	writeJSON(w, agentHTTPStatus(err), map[string]string{"error": err.Error()})
}

func decodeAgentBody(w http.ResponseWriter, r *http.Request) (config.AgentEntry, bool) {
	var in config.AgentEntry
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAgentError(w, agentErr(http.StatusBadRequest, "invalid_body", "request body must be one JSON agent entry"))
		return in, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeAgentError(w, agentErr(http.StatusBadRequest, "invalid_body", "request body must be one JSON agent entry"))
		return in, false
	}
	return in, true
}

// RegisterAgentListRoutes wires the agent-list doors over ops. Every write is
// gated by project.agent; the entry's command is what a start runs.
func RegisterAgentListRoutes(rr *control.RouteRegistrar, ops *AgentListOps) {
	rr.Handle(control.ClassRead, "GET /api/projects/{id}/agents", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		entries, err := ops.List(r.Context(), id)
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"projectId": id, "entries": entries})
	})

	rr.HandleGated(control.ClassConfigure, []string{"project.agent"}, "POST /api/projects/{id}/agents", func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeAgentBody(w, r)
		if !ok {
			return
		}
		res, err := ops.Add(r.Context(), r.PathValue("id"), in, auditViaHTTP, credIDOf(r))
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	})

	rr.HandleGated(control.ClassConfigure, []string{"project.agent"}, "PUT /api/projects/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeAgentBody(w, r)
		if !ok {
			return
		}
		res, err := ops.Update(r.Context(), r.PathValue("id"), r.PathValue("name"), in, auditViaHTTP, credIDOf(r))
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	rr.HandleGated(control.ClassConfigure, []string{"project.agent"}, "DELETE /api/projects/{id}/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, name := r.PathValue("id"), r.PathValue("name")
		if err := ops.Remove(r.Context(), id, name, auditViaHTTP, credIDOf(r)); err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"projectId": id, "name": name})
	})
}
