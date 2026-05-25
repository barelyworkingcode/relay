package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"relaygo/bridge"
)

// ipcServiceResourceMsg drives the Service Inspector's generic resource CRUD
// dispatch. The manifest is the authority: relay refuses any (resourceId, op)
// combination the named service hasn't declared. The UI never controls paths
// or methods — those come from the service's ResourceDecl.
//
// Op-specific payload rules:
//   - "list":   id and record both ignored.
//   - "create": id ignored; record marshaled into the request body.
//   - "update": id required; record marshaled into the request body.
//   - "delete": id required; record ignored.
type ipcServiceResourceMsg struct {
	ServiceID  string          `json:"serviceId"`
	ResourceID string          `json:"resourceId"`
	Op         string          `json:"op"` // list | create | update | delete
	ID         string          `json:"id,omitempty"`
	Record     json.RawMessage `json:"record,omitempty"`
}

const MsgServiceResource = "service_resource"

const (
	opList   = "list"
	opCreate = "create"
	opUpdate = "update"
	opDelete = "delete"
)

// resourceCallTimeout caps each CRUD request. Most run sub-second on a
// loopback Unix socket; 10s leaves headroom for a service that does
// real work on persist (e.g. file write + reconcile).
const resourceCallTimeout = 10 * time.Second

// ipcServiceResource dispatches a manifest-declared resource operation.
//
// Security: paths and methods come exclusively from the registered manifest;
// only record bodies and the {id} substitution come from the UI, and the id
// is url.PathEscape'd before splicing.
func ipcServiceResource(ipc *IPCContext, raw json.RawMessage) {
	var msg ipcServiceResourceMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		dispatchEmit(ipc, "onServiceResourceResult", map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("decode resource op: %v", err),
		})
		return
	}

	if ipc.Enhanced == nil {
		emitResourceResult(ipc, msg, false, nil, "no enhanced registry")
		return
	}
	rec := ipc.Enhanced.Get(msg.ServiceID)
	if rec == nil {
		emitResourceResult(ipc, msg, false, nil, fmt.Sprintf("service %q not registered", msg.ServiceID))
		return
	}
	resource := findResource(rec.Manifest.Resources, msg.ResourceID)
	if resource == nil {
		emitResourceResult(ipc, msg, false, nil, fmt.Sprintf("resource %q not declared by service %q", msg.ResourceID, msg.ServiceID))
		return
	}
	endpoint, body, err := pickEndpoint(resource, msg)
	if err != nil {
		emitResourceResult(ipc, msg, false, nil, err.Error())
		return
	}

	client := NewServiceStatusClient(rec.InternalSocket, rec.InternalToken)
	method := endpoint.Method
	path := endpoint.path

	// Off-main: a slow persist can't block the WebView.
	ipc.GoFunc(func() {
		callCtx, cancel := context.WithTimeout(ipc.Ctx, resourceCallTimeout)
		defer cancel()
		respBody, err := client.DoResource(callCtx, method, path, body)
		errStr := ""
		if err != nil {
			slog.Warn("service resource op failed",
				"service", msg.ServiceID, "resource", msg.ResourceID, "op", msg.Op, "error", err)
			errStr = err.Error()
		}
		emitResourceResult(ipc, msg, err == nil, respBody, errStr)
	})
}

// resolvedEndpoint pairs an endpoint declaration with its substituted path
// (the manifest's PathTemplate after {id} replacement).
type resolvedEndpoint struct {
	Method string
	path   string
}

// pickEndpoint picks the right endpoint for the op and prepares its path and
// request body. Returns errors for declared-but-not-supported ops (e.g. a
// resource that declares create but not update) and for ops that need an id
// but didn't get one.
func pickEndpoint(res *bridge.ResourceDecl, msg ipcServiceResourceMsg) (resolvedEndpoint, json.RawMessage, error) {
	switch msg.Op {
	case opList:
		return resolvedEndpoint{Method: res.List.Method, path: res.List.PathTemplate}, nil, nil

	case opCreate:
		if res.Create == nil {
			return resolvedEndpoint{}, nil, fmt.Errorf("resource %q does not support create", res.ID)
		}
		return resolvedEndpoint{Method: res.Create.Method, path: res.Create.PathTemplate}, msg.Record, nil

	case opUpdate:
		if res.Update == nil {
			return resolvedEndpoint{}, nil, fmt.Errorf("resource %q does not support update", res.ID)
		}
		path, err := substituteID(res.Update.PathTemplate, msg.ID, res.ID, msg.Op)
		if err != nil {
			return resolvedEndpoint{}, nil, err
		}
		return resolvedEndpoint{Method: res.Update.Method, path: path}, msg.Record, nil

	case opDelete:
		if res.Delete == nil {
			return resolvedEndpoint{}, nil, fmt.Errorf("resource %q does not support delete", res.ID)
		}
		path, err := substituteID(res.Delete.PathTemplate, msg.ID, res.ID, msg.Op)
		if err != nil {
			return resolvedEndpoint{}, nil, err
		}
		return resolvedEndpoint{Method: res.Delete.Method, path: path}, nil, nil

	default:
		return resolvedEndpoint{}, nil, fmt.Errorf("unsupported op %q (want list|create|update|delete)", msg.Op)
	}
}

// substituteID splices the row ID into a path template. Manifest validation
// already guarantees {id} is present in update/delete templates; the runtime
// check here protects against later refactors.
func substituteID(template, id, resourceID, op string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("resource %q %s requires id", resourceID, op)
	}
	if !strings.Contains(template, "{id}") {
		return "", fmt.Errorf("resource %q %s pathTemplate %q missing {id}", resourceID, op, template)
	}
	return strings.ReplaceAll(template, "{id}", url.PathEscape(id)), nil
}

func findResource(resources []bridge.ResourceDecl, id string) *bridge.ResourceDecl {
	for i := range resources {
		if resources[i].ID == id {
			return &resources[i]
		}
	}
	return nil
}

// emitResourceResult bundles the response shape sent back to the WebView.
// Body is whatever the service returned (may be a single record on create/
// update, an array on list, or empty on delete). The UI deserializes based
// on the op.
func emitResourceResult(ipc *IPCContext, msg ipcServiceResourceMsg, ok bool, body json.RawMessage, errStr string) {
	payload := map[string]interface{}{
		"serviceId":  msg.ServiceID,
		"resourceId": msg.ResourceID,
		"op":         msg.Op,
		"ok":         ok,
	}
	if msg.ID != "" {
		payload["id"] = msg.ID
	}
	if len(body) > 0 {
		payload["body"] = body
	}
	if errStr != "" {
		payload["error"] = errStr
	}
	dispatchEmit(ipc, "onServiceResourceResult", payload)
}
