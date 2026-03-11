package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"strings"
)

//go:embed settings.html
var settingsHTML string

func renderSettingsHTML(settings *Settings, runningIDs []string) string {
	tokensJSON, _ := json.Marshal(settings.Tokens)
	externalMcpsJSON, _ := json.Marshal(settings.ExternalMcps)
	userServicesJSON, _ := json.Marshal(settings.Services)
	runningIDsJSON, _ := json.Marshal(runningIDs)

	exePath, err := os.Executable()
	if err != nil {
		exePath = ""
	}
	exeJSON, _ := json.Marshal(exePath)

	return strings.NewReplacer(
		"__EXE_PATH_JSON__", string(exeJSON),
		"__EXTERNAL_MCPS_JSON__", string(externalMcpsJSON),
		"__SERVICES_JSON__", string(userServicesJSON),
		"__RUNNING_IDS_JSON__", string(runningIDsJSON),
		"__TOKENS_JSON__", string(tokensJSON),
	).Replace(settingsHTML)
}
