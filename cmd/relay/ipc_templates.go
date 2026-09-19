package main

import (
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
)

// ipcListTemplates answers the Templates section's list. Thin adapter over
// the same config.EffectiveTerminalTemplates RegisterTemplateRoutes uses, so a
// curl and the Settings window see the same list. Every mutation below ends
// by emitting it, so the UI has one success path.
func ipcListTemplates(ctx *IPCContext, raw json.RawMessage) {
	ctx.UI.EmitEvent("onTemplatesListed", marshalForUI(config.EffectiveTerminalTemplates(ctx.Store.Get())))
}

func ipcSaveTemplate(ctx *IPCContext, raw json.RawMessage, msgType string, save func(*TemplateOps, config.TerminalTemplate) error) {
	t, ok := unmarshalIPC[config.TerminalTemplate](raw, msgType)
	if !ok {
		return
	}
	if err := save(&TemplateOps{Store: ctx.Store}, *t); err != nil {
		ctx.UI.EmitEvent("onTemplateError", err.Error())
		return
	}
	ipcListTemplates(ctx, nil)
}

func ipcCreateTemplate(ctx *IPCContext, raw json.RawMessage) {
	ipcSaveTemplate(ctx, raw, MsgCreateTemplate, (*TemplateOps).Create)
}

func ipcUpdateTemplate(ctx *IPCContext, raw json.RawMessage) {
	ipcSaveTemplate(ctx, raw, MsgUpdateTemplate, (*TemplateOps).Update)
}

func ipcRemoveTemplate(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[struct {
		ID string `json:"id"`
	}](raw, MsgRemoveTemplate)
	if !ok {
		return
	}
	if err := (&TemplateOps{Store: ctx.Store}).Remove(msg.ID); err != nil {
		ctx.UI.EmitEvent("onTemplateError", err.Error())
		return
	}
	ipcListTemplates(ctx, nil)
}
