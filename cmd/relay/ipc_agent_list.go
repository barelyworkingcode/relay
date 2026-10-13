package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
)

type ipcAgentListMsg struct {
	ProjectID string `json:"projectId"`
}

type ipcAgentAddMsg struct {
	ProjectID string            `json:"projectId"`
	Entry     config.AgentEntry `json:"entry"`
}

type ipcAgentUpdateMsg struct {
	ProjectID string            `json:"projectId"`
	Name      string            `json:"name"`
	Entry     config.AgentEntry `json:"entry"`
}

type ipcAgentRemoveMsg struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
}

type ipcAgentErrorPayload struct {
	ProjectID string `json:"projectId"`
	Error     string `json:"error"`
	Message   string `json:"message"`
	Command   string `json:"command,omitempty"`
	Machine   string `json:"machine,omitempty"`
}

// agentIPCError maps a refusal to the payload the Settings window shows. A
// presence or audit refusal reads "refused", a server fault "internal".
func agentIPCError(projectID string, err error) ipcAgentErrorPayload {
	var ae *AgentError
	if errors.As(err, &ae) {
		return ipcAgentErrorPayload{ProjectID: projectID, Error: ae.Code, Message: ae.Message, Command: ae.Command, Machine: ae.Machine}
	}
	code := "internal"
	if agentHTTPStatus(err) == http.StatusForbidden {
		code = "refused"
	}
	return ipcAgentErrorPayload{ProjectID: projectID, Error: code, Message: err.Error()}
}

// emitAgentList reads the list and emits it, or the error. Callers off the
// main thread pass dispatch true.
func emitAgentList(ctx *IPCContext, projectID string, dispatch bool) {
	emit := func(event string, payload any) {
		if dispatch {
			dispatchEmit(ctx, event, marshalForUI(payload))
			return
		}
		ctx.UI.EmitEvent(event, marshalForUI(payload))
	}
	entries, err := ctx.AgentListOps.List(ctx.Ctx, projectID)
	if err != nil {
		emit("onProjectAgentError", agentIPCError(projectID, err))
		return
	}
	emit("onProjectAgentsListed", map[string]any{"projectId": projectID, "entries": entries})
}

func ipcListProjectAgents(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAgentListMsg](raw, "list_project_agents")
	if !ok || msg.ProjectID == "" || ctx.AgentListOps == nil {
		return
	}
	emitAgentList(ctx, msg.ProjectID, false)
}

// The writes run off the main thread: each raises a presence prompt, which
// blocks on an async completion the Cocoa run loop must keep pumping.
func ipcAddProjectAgent(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAgentAddMsg](raw, "add_project_agent")
	if !ok || msg.ProjectID == "" || ctx.AgentListOps == nil {
		return
	}
	ctx.GoFunc(func() {
		if _, err := ctx.AgentListOps.Add(ctx.Ctx, msg.ProjectID, msg.Entry, auditViaIPC, ""); err != nil {
			dispatchEmit(ctx, "onProjectAgentError", marshalForUI(agentIPCError(msg.ProjectID, err)))
			return
		}
		emitAgentList(ctx, msg.ProjectID, true)
	})
}

func ipcUpdateProjectAgent(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAgentUpdateMsg](raw, "update_project_agent")
	if !ok || msg.ProjectID == "" || ctx.AgentListOps == nil {
		return
	}
	ctx.GoFunc(func() {
		if _, err := ctx.AgentListOps.Update(ctx.Ctx, msg.ProjectID, msg.Name, msg.Entry, auditViaIPC, ""); err != nil {
			dispatchEmit(ctx, "onProjectAgentError", marshalForUI(agentIPCError(msg.ProjectID, err)))
			return
		}
		emitAgentList(ctx, msg.ProjectID, true)
	})
}

func ipcRemoveProjectAgent(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAgentRemoveMsg](raw, "remove_project_agent")
	if !ok || msg.ProjectID == "" || ctx.AgentListOps == nil {
		return
	}
	ctx.GoFunc(func() {
		if err := ctx.AgentListOps.Remove(ctx.Ctx, msg.ProjectID, msg.Name, auditViaIPC, ""); err != nil {
			dispatchEmit(ctx, "onProjectAgentError", marshalForUI(agentIPCError(msg.ProjectID, err)))
			return
		}
		emitAgentList(ctx, msg.ProjectID, true)
	})
}
