package main

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"strings"
)

// The settings document is bundled from web/src/* into web/dist/settings.html by
// web/gen (esbuild). web/dist/settings.html is committed so a plain `go build`
// embeds a working artifact; build.sh re-runs the generator first so installs
// always carry a fresh bundle. Regenerate after editing web/src or web/shell.html
// with: go generate ./...   (or: go run ./web/gen)
//
//go:generate go run ./web/gen
//go:embed web/dist/settings.html
var settingsHTML string

// mustMarshalJSON marshals v to JSON, returning "null" on error and logging.
func mustMarshalJSON(label string, v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal settings for UI", "field", label, "error", err)
		return "null"
	}
	return string(data)
}

// renderSettingsHTML produces the initial WebView document. toolCache is the
// per-MCP tool list (mcpID → []ToolInfo) used by the Projects tab's tri-state
// picker; it's preseeded so the first paint of a project edit form doesn't
// have to round-trip an IPC for every allowed MCP. Pass nil in tests that
// don't exercise the picker.
func renderSettingsHTML(settings *Settings, runningIDs []string, toolCache map[string][]ToolInfo) string {
	if runningIDs == nil {
		runningIDs = []string{}
	}
	if toolCache == nil {
		toolCache = map[string][]ToolInfo{}
	}
	projects := settings.Projects
	if projects == nil {
		projects = []Project{}
	}
	return strings.NewReplacer(
		"__EXTERNAL_MCPS_JSON__", mustMarshalJSON("external_mcps", settings.ExternalMcps),
		"__SERVICES_JSON__", mustMarshalJSON("services", settings.Services),
		"__RUNNING_IDS_JSON__", mustMarshalJSON("running_ids", runningIDs),
		"__PROJECTS_JSON__", mustMarshalJSON("projects", projects),
		"__MCP_TOOL_CACHE_JSON__", mustMarshalJSON("mcp_tool_cache", toolCache),
	).Replace(settingsHTML)
}
