package main

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"strings"
)

//go:embed settings.html
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

func renderSettingsHTML(settings *Settings, runningIDs []string) string {
	if runningIDs == nil {
		runningIDs = []string{}
	}
	return strings.NewReplacer(
		"__EXTERNAL_MCPS_JSON__", mustMarshalJSON("external_mcps", settings.ExternalMcps),
		"__SERVICES_JSON__", mustMarshalJSON("services", settings.Services),
		"__RUNNING_IDS_JSON__", mustMarshalJSON("running_ids", runningIDs),
	).Replace(settingsHTML)
}
