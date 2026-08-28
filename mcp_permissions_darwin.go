package main

import (
	"fmt"
	"strings"
)

// Each service primer is sequential and blocks, so total wait worst-case is
// N * PrimerTimeoutSec.
const PrimerTimeoutSec = 30

// Grants made to Relay's own process flow to the spawned MCP via
// responsible-parent attribution.
func primeRelayTccPermissions(services []string, result *ResetMcpPermissionsResult) {
	if len(services) == 0 {
		return
	}
	// Bump Relay from .accessory (LSUIElement tray) to .regular for the duration
	// of the primer batch: macOS Sequoia otherwise suppresses the TCC prompts
	// even though Relay is /Applications-resident and signed.
	cocoaBeginForegroundActivation()
	defer cocoaEndForegroundActivation()

	// Services without a Cocoa primer (microphone, appleevents) fall through
	// the switch and rely on first-use prompts from the MCP itself at runtime.
	lines := []string{"--- Relay TCC primer ---"}
	for _, svc := range services {
		switch svc {
		case "calendar":
			lines = append(lines, fmt.Sprintf("  calendar: relay grant = %s", grantWord(cocoaRequestTccCalendar(PrimerTimeoutSec))))
		case "reminders":
			lines = append(lines, fmt.Sprintf("  reminders: relay grant = %s", grantWord(cocoaRequestTccReminders(PrimerTimeoutSec))))
		case "contacts":
			lines = append(lines, fmt.Sprintf("  contacts: relay grant = %s", grantWord(cocoaRequestTccContacts(PrimerTimeoutSec))))
		}
	}
	if len(lines) > 1 {
		result.SpawnOutput = strings.Join(lines, "\n") + "\n\n" + result.SpawnOutput
	}
}

func grantWord(ok bool) string {
	if ok {
		return "authorized"
	}
	return "not granted (timeout, denied, or already in a non-prompt state)"
}
