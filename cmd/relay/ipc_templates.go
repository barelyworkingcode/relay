package main

import (
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
)

// ipcListTemplates answers the Templates section's list — read-only, since
// the templates (Settings.TerminalTemplates, a project's ShellTemplates) have
// no editor in this unit (issue #148). Thin adapter over the same config.EffectiveTerminalTemplates
// RegisterTemplateRoutes uses, so a curl and the Settings window see the
// same list.
func ipcListTemplates(ctx *IPCContext, raw json.RawMessage) {
	ctx.UI.EmitEvent("onTemplatesListed", marshalForUI(config.EffectiveTerminalTemplates(ctx.Store.Get())))
}
