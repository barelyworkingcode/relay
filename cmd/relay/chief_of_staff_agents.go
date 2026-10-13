package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
)

// cosAgentRow is an agent entry as the Chief of Staff sees it. It carries no
// args: the scope may learn what a start can name, never how it is launched.
type cosAgentRow struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Model   string `json:"model,omitempty"`
	Command string `json:"command,omitempty"`
}

// cosChatRow is a chat template with no id, prompt, mode or voice.
type cosChatRow struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

type chiefOfStaffAgentList struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	Machine     string `json:"machine"`
	Entries     []any  `json:"entries"`
}

// chiefOfStaffPreviewBody is the whole of what the preview reads. It has no
// args field, so an unknown-key refusal keeps arguments out of the proposal.
type chiefOfStaffPreviewBody struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Model   string `json:"model"`
	Command string `json:"command"`
}

const chiefOfStaffProjectUnavailable = "project is not available for a session launch"

// chiefOfStaffProject reads the project fresh. A missing and a remote project
// answer the same, so the door is not an oracle for either.
func chiefOfStaffProject(store config.SettingsStore, id string) (*config.Project, *AgentError) {
	p, _ := config.FindProjectByID(config.FreshSettings(store), id)
	if p == nil || p.IsRemote() {
		return nil, agentErr(http.StatusForbidden, "project_not_available", chiefOfStaffProjectUnavailable)
	}
	return p, nil
}

// RegisterChiefOfStaffAgentRoutes wires the two read-only Chief of Staff
// doors. Neither writes an entry or raises a prompt; adding one stays a
// gated configure write.
func RegisterChiefOfStaffAgentRoutes(rr *control.RouteRegistrar, store config.SettingsStore, ops *AgentListOps) {
	if ops == nil {
		return
	}
	rr.Handle(control.ClassChiefOfStaff, "GET /api/chief-of-staff/projects/{projectId}/agents", func(w http.ResponseWriter, r *http.Request) {
		handleChiefOfStaffAgentList(w, r, store, ops)
	})
	rr.Handle(control.ClassChiefOfStaff, "POST /api/chief-of-staff/projects/{projectId}/agents/preview", func(w http.ResponseWriter, r *http.Request) {
		handleChiefOfStaffAgentPreview(w, r, store, ops)
	})
}

func handleChiefOfStaffAgentList(w http.ResponseWriter, r *http.Request, store config.SettingsStore, ops *AgentListOps) {
	id := r.PathValue("projectId")
	ev := logging.BeginEvent(r.Context(), "chief_of_staff.agents.list").Set("project_id", id)
	w = newEventResponseWriter(w, func(status int) { endEventHTTP(ev, status, "chief of staff agent list refused") })
	proj, aerr := chiefOfStaffProject(store, id)
	if aerr != nil {
		writeAgentError(w, aerr)
		return
	}
	entries := make([]any, 0, len(proj.Agents)+len(proj.ChatTemplates))
	for _, a := range proj.Agents {
		entries = append(entries, cosAgentRow{Type: chiefOfStaffEntryAgent, Name: a.Name, Kind: a.Kind, Model: a.Model, Command: a.Command})
	}
	for _, t := range proj.ChatTemplates {
		entries = append(entries, cosChatRow{Type: chiefOfStaffEntryChat, Name: t.Name, Model: t.Model})
	}
	ev.Set("count", len(entries))
	writeJSON(w, http.StatusOK, chiefOfStaffAgentList{
		ProjectID: id, ProjectName: proj.Name, Machine: chiefOfStaffMachine(r, ops, proj), Entries: entries,
	})
}

// chiefOfStaffMachine names where the project's commands are looked up. A
// lookup that fails still names the machine, which is all this asks of it.
func chiefOfStaffMachine(r *http.Request, ops *AgentListOps, proj *config.Project) string {
	if ops.Lookup == nil {
		return agentMachineLocal
	}
	_, machine, _ := ops.Lookup(r.Context(), proj, "")
	return machine
}

func decodeChiefOfStaffPreviewBody(w http.ResponseWriter, r *http.Request) (chiefOfStaffPreviewBody, bool) {
	var in chiefOfStaffPreviewBody
	bad := agentErr(http.StatusBadRequest, "invalid_body", `request body must be JSON {"name","kind","model","command"}`)
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

func handleChiefOfStaffAgentPreview(w http.ResponseWriter, r *http.Request, store config.SettingsStore, ops *AgentListOps) {
	id := r.PathValue("projectId")
	ev := logging.BeginEvent(r.Context(), "chief_of_staff.agents.preview").Set("project_id", id)
	w = newEventResponseWriter(w, func(status int) { endEventHTTP(ev, status, "chief of staff agent preview refused") })
	in, ok := decodeChiefOfStaffPreviewBody(w, r)
	if !ok {
		return
	}
	if _, aerr := chiefOfStaffProject(store, id); aerr != nil {
		writeAgentError(w, aerr)
		return
	}
	res, err := ops.Preview(r.Context(), id, config.AgentEntry{Name: in.Name, Kind: in.Kind, Model: in.Model, Command: in.Command})
	var ae *AgentError
	if errors.As(err, &ae) {
		if ae.Machine != "" {
			ev.Set("machine", ae.Machine)
		}
		// The project moved between the door's check and the core's read.
		if ae.Code == "project_not_found" || ae.Code == "project_not_available" {
			writeAgentError(w, agentErr(http.StatusForbidden, "project_not_available", chiefOfStaffProjectUnavailable))
			return
		}
	}
	if err != nil {
		endEvent(ev, agentEventErr(err))
		writeAgentError(w, err)
		return
	}
	ev.Set("machine", res.Machine)
	writeJSON(w, http.StatusOK, res)
}
