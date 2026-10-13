package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
)

func decodeChatTemplateBody(w http.ResponseWriter, r *http.Request) (ChatTemplateInput, bool) {
	var in ChatTemplateInput
	bad := agentErr(http.StatusBadRequest, "invalid_body", `request body must be JSON {"name","model"}`)
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAgentError(w, bad)
		return in, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeAgentError(w, bad)
		return in, false
	}
	return in, true
}

// RegisterChatTemplateRoutes wires the chat-template add door. It is class
// configure, which the Chief of Staff scope never reaches.
func RegisterChatTemplateRoutes(rr *control.RouteRegistrar, ops *ChatTemplateOps) {
	rr.HandleGated(control.ClassConfigure, []string{"project.chat_template"}, "POST /api/projects/{id}/chat-templates", func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeChatTemplateBody(w, r)
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
}
