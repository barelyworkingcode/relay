package main

import (
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
)

// ipcListTemplates answers the Templates section's list — read-only, since
// built-ins are seeded in code and the only override points
// (Settings.TerminalTemplates, a project's ShellTemplates) have no editor
// in this unit. Thin adapter over the same config.EffectiveTerminalTemplates
// RegisterTemplateRoutes uses, so a curl and the Settings window see the
// same list.
func ipcListTemplates(ctx *IPCContext, raw json.RawMessage) {
	ctx.UI.EmitEvent("onTemplatesListed", marshalForUI(config.EffectiveTerminalTemplates(ctx.Store.Get())))
}
