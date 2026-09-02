package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/webassets"
)

// The settings document is bundled from web/src/* into
// internal/webassets/settings.html by web/gen (esbuild). The generated artifact
// belongs to internal/webassets so Go can embed it without reaching outside this
// package. It is committed so a plain
// `go build` embeds a working artifact; build.sh re-runs the generator first so
// installs always carry a fresh bundle. Regenerate after editing web/src or
// web/shell.html with: go generate ./...   (or: go run ./web/gen)
//
//go:generate go run ../../web/gen
var settingsHTML = webassets.SettingsHTML

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
//
// scopeFields is the same idea for ADR-011's per-MCP permission panel: the
// scope: "restrict" fields each MCP declares, so the editor can render one
// input per field with the MCP's own description as help text. It is seeded
// rather than fetched because it is small (a handful of fields per MCP) and
// because the PROJECT LIST needs it too — a row has to say "needs a scope
// value" without anyone opening the editor first, and a list that had to
// round-trip for that would render the reassuring answer first.
func renderSettingsHTML(settings *config.Settings, runningIDs []string, toolCache map[string][]config.ToolInfo, scopeFields map[string][]project.ScopeFieldView) string {
	return renderSettingsDocument(settings, runningIDs, toolCache, scopeFields, nil, "")
}

// renderSettingsDocument is renderSettingsHTML plus the two things only the
// tray can supply: a bootstrap code minted moments ago by the menu item that
// opened this window, and the page that menu item wants the window to open
// on. Both are seeded into the first paint rather than emitted, because a
// window that is not up yet has no document to receive an emit — see
// App.showLoginCode and App.openRemoteClientsPage. nil and "" are the
// ordinary case and every other caller's.
//
// initialPage is one of web/src/app.js's showPage ids and is set only from a
// constant in this repository; it never carries anything a network peer
// supplied.
func renderSettingsDocument(settings *config.Settings, runningIDs []string, toolCache map[string][]config.ToolInfo, scopeFields map[string][]project.ScopeFieldView, loginCode *loginCodeView, initialPage string) string {
	if runningIDs == nil {
		runningIDs = []string{}
	}
	if toolCache == nil {
		toolCache = map[string][]config.ToolInfo{}
	}
	if scopeFields == nil {
		scopeFields = map[string][]project.ScopeFieldView{}
	}
	projects := settings.Projects
	if projects == nil {
		projects = []config.Project{}
	}
	// Enrolments are seeded like projects rather than fetched on tab switch:
	// the list is small (one row per enrolled certificate), and a credential
	// you cannot see is one you will not revoke — it should be on screen the
	// moment the tab is, with no loading state to fail into.
	enrolments := settings.Enrolments
	if enrolments == nil {
		enrolments = []config.Enrolment{}
	}
	// The first paint has no *audit.AuditRecorder to consult, so the remote view's
	// audit state comes from the configuration. That is what the operator
	// edits and what NewRemoteServer's refusal is phrased in terms of; the IPC
	// handlers, which do hold the recorder, pass its live answer instead (see
	// remoteConfigViewOf).
	remote := remoteConfigViewOf(settings, audit.ResolveAuditConfig(settings.Audit).Enabled)
	return strings.NewReplacer(
		"__EXTERNAL_MCPS_JSON__", mustMarshalJSON("external_mcps", externalMcpsToNativeView(settings.ExternalMcps)),
		"__SERVICES_JSON__", mustMarshalJSON("services", serviceConfigsToNativeView(settings.Services)),
		"__RUNNING_IDS_JSON__", mustMarshalJSON("running_ids", runningIDs),
		"__PROJECTS_JSON__", mustMarshalJSON("projects", projectsToNativeView(projects)),
		"__MCP_TOOL_CACHE_JSON__", mustMarshalJSON("mcp_tool_cache", toolCache),
		"__MCP_SCOPE_FIELDS_JSON__", mustMarshalJSON("mcp_scope_fields", scopeFields),
		"__ENROLMENTS_JSON__", mustMarshalJSON("enrolments", enrolments),
		"__REMOTE_JSON__", mustMarshalJSON("remote", remote),
		"__ENROLMENT_BUDGET_DEFAULTS_JSON__", mustMarshalJSON("enrolment_budget_defaults", enrolmentBudgetDefaults()),
		// Seeded for the enrolments' reason, and projected through the same
		// view type the IPC door uses so there is exactly one definition of
		// what a passkey looks like outside relay — one with no field for
		// the public key. Live sessions ride along because a revoked passkey
		// does not end a session it already signed in, and the tab has to be
		// able to say so with both lists on screen.
		"__PASSKEYS_JSON__", mustMarshalJSON("passkeys", passkeyViews(settings)),
		"__LOGIN_SESSIONS_JSON__", mustMarshalJSON("login_sessions", loginSessionViews(settings, time.Now())),
		"__LOGIN_CODE_JSON__", mustMarshalJSON("login_code", loginCode),
		"__INITIAL_PAGE_JSON__", mustMarshalJSON("initial_page", initialPage),
	).Replace(settingsHTML)
}
