package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/service"
)

// Restricted to identifier-shaped keys so a malformed manifest can't smuggle
// regex metacharacters into the substitution step.
var pathPlaceholder = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

type ipcServiceActionMsg struct {
	ServiceID string                     `json:"serviceId"`
	ActionID  string                     `json:"actionId"`
	Row       map[string]json.RawMessage `json:"row,omitempty"`
}

const MsgServiceAction = "service_action"

// The manifest *is* the action whitelist: relay refuses anything not in
// manifest.Actions for the named serviceId. Paths come from the
// service-declared manifest, never from the UI — only row *values* come from
// the UI, and they pass through url.PathEscape before being spliced in.
func ipcServiceAction(ipc *IPCContext, raw json.RawMessage) {
	var msg ipcServiceActionMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		dispatchEmit(ipc, "onServiceActionResult", map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("decode action: %v", err),
		})
		return
	}

	if ipc.Enhanced == nil {
		emitActionResult(ipc, msg, false, "no enhanced registry")
		return
	}
	rec := ipc.Enhanced.Get(msg.ServiceID)
	if rec == nil {
		emitActionResult(ipc, msg, false, fmt.Sprintf("service %q not registered", msg.ServiceID))
		return
	}
	action := findAction(rec.Manifest.Actions, msg.ActionID)
	if action == nil {
		emitActionResult(ipc, msg, false, fmt.Sprintf("action %q not declared by service %q", msg.ActionID, msg.ServiceID))
		return
	}
	path, err := buildActionPath(action, msg.Row)
	if err != nil {
		emitActionResult(ipc, msg, false, err.Error())
		return
	}

	// Off-main so a slow service can't block the UI on a 10s timeout.
	client := service.NewStatusClient(rec.InternalSocket, rec.InternalToken)
	method := action.Method
	ipc.GoFunc(func() {
		defer client.CloseIdleConnections()
		callCtx, cancel := context.WithTimeout(ipc.Ctx, 10*time.Second)
		defer cancel()
		_, err := client.DoAction(callCtx, method, path)
		errStr := ""
		if err != nil {
			slog.Warn("service action failed",
				"service", msg.ServiceID, "action", msg.ActionID, "error", err)
			errStr = err.Error()
		}
		emitActionResult(ipc, msg, err == nil, errStr)
		// Re-poll immediately so the UI reflects the post-action state
		// without waiting up to 2s for the next tick.
		if ipc.PushServiceStatusBatch != nil {
			ipc.PushServiceStatusBatch()
		}
	})
}

// Row keys with no matching placeholder are ignored (forward-compatible when
// a service ships a new column the UI hasn't been updated for). When
// ForEach == "" the row map must be empty — otherwise the UI is dispatching
// a global action with surprising context, and that's a bug we want to
// surface rather than silently dispatch.
func buildActionPath(action *bridge.ActionDecl, row map[string]json.RawMessage) (string, error) {
	if action.ForEach == "" && len(row) > 0 {
		return "", fmt.Errorf("action %q has no forEach but row was supplied", action.ID)
	}
	var missing, invalid []string
	result := pathPlaceholder.ReplaceAllStringFunc(action.PathTemplate, func(match string) string {
		key := match[1 : len(match)-1]
		raw, ok := row[key]
		if !ok {
			missing = append(missing, key)
			return match
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			s = strings.TrimSpace(string(raw))
		}
		// url.PathEscape leaves "." unescaped, so "." / ".." would become a
		// live traversal segment ("/api/x/.." -> "/api/x"). Reject them
		// outright — a legitimate row key never resolves to one.
		if s == "." || s == ".." {
			invalid = append(invalid, key)
			return match
		}
		return url.PathEscape(s)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("action %q: row missing keys %v", action.ID, missing)
	}
	if len(invalid) > 0 {
		return "", fmt.Errorf("action %q: row keys %v have illegal path-traversal values", action.ID, invalid)
	}
	return result, nil
}

func findAction(actions []bridge.ActionDecl, id string) *bridge.ActionDecl {
	for i := range actions {
		if actions[i].ID == id {
			return &actions[i]
		}
	}
	return nil
}

func emitActionResult(ipc *IPCContext, msg ipcServiceActionMsg, ok bool, errStr string) {
	if !ok {
		slog.Warn("service action rejected",
			"service", msg.ServiceID, "action", msg.ActionID, "reason", errStr)
	}
	payload := map[string]interface{}{
		"serviceId": msg.ServiceID,
		"actionId":  msg.ActionID,
		"row":       msg.Row,
		"ok":        ok,
	}
	if errStr != "" {
		payload["error"] = errStr
	}
	dispatchEmit(ipc, "onServiceActionResult", payload)
}
