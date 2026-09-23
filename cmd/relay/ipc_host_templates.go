package main

import (
	"context"
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Host template IPC handlers for the Hosts tab. They mirror
// host_template_routes.go the way ipc_templates.go mirrors
// template_routes.go: every mutation ends by emitting the host's list
// (onHostTemplatesListed) or onHostTemplateError, so the UI has one success
// path.

type ipcHostTemplatesListed struct {
	HostID    string                    `json:"host_id"`
	Templates []config.TerminalTemplate `json:"templates"`
}

type ipcHostTemplateMsg struct {
	HostID   string                  `json:"host_id"`
	Template config.TerminalTemplate `json:"template"`
}

// hostTemplateOps returns the shared core; the fallback serves contexts
// built without one (tests).
func (c *IPCContext) hostTemplateOps() *HostTemplateOps {
	if c.HostTemplateOps != nil {
		return c.HostTemplateOps
	}
	return &HostTemplateOps{Store: c.Store}
}

func ipcListHostTemplates(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[struct {
		HostID string `json:"host_id"`
	}](raw, MsgListHostTemplates)
	if !ok {
		return
	}
	ts, err := ctx.hostTemplateOps().List(msg.HostID)
	if err != nil {
		ctx.UI.EmitEvent("onHostTemplateError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onHostTemplatesListed", marshalForUI(ipcHostTemplatesListed{HostID: msg.HostID, Templates: ts}))
}

func emitHostTemplateResult(ctx *IPCContext, hostID string, err error) {
	if err != nil {
		dispatchEmit(ctx, "onHostTemplateError", err.Error())
		return
	}
	ts, err := ctx.hostTemplateOps().List(hostID)
	if err != nil {
		dispatchEmit(ctx, "onHostTemplateError", err.Error())
		return
	}
	dispatchEmit(ctx, "onHostTemplatesListed", marshalForUI(ipcHostTemplatesListed{HostID: hostID, Templates: ts}))
}

func ipcSaveHostTemplate(ctx *IPCContext, raw json.RawMessage, msgType string, save func(*HostTemplateOps, context.Context, string, config.TerminalTemplate) error) {
	msg, ok := unmarshalIPC[ipcHostTemplateMsg](raw, msgType)
	if !ok {
		return
	}
	ctx.GoFunc(func() {
		emitHostTemplateResult(ctx, msg.HostID, save(ctx.hostTemplateOps(), ctx.Ctx, msg.HostID, msg.Template))
	})
}

func ipcCreateHostTemplate(ctx *IPCContext, raw json.RawMessage) {
	ipcSaveHostTemplate(ctx, raw, MsgCreateHostTemplate, (*HostTemplateOps).Create)
}

func ipcUpdateHostTemplate(ctx *IPCContext, raw json.RawMessage) {
	ipcSaveHostTemplate(ctx, raw, MsgUpdateHostTemplate, (*HostTemplateOps).Update)
}

func ipcRemoveHostTemplate(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[struct {
		HostID string `json:"host_id"`
		ID     string `json:"id"`
	}](raw, MsgRemoveHostTemplate)
	if !ok {
		return
	}
	ctx.GoFunc(func() {
		emitHostTemplateResult(ctx, msg.HostID, ctx.hostTemplateOps().Remove(ctx.Ctx, msg.HostID, msg.ID))
	})
}
