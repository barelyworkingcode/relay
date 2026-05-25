package main

import (
	"fmt"
	"strings"
)

// PrimerTimeoutSec caps how long the Cocoa primer waits per service. Each
// service is sequential, so total wait worst-case is N * PrimerTimeoutSec.
// 30s gives the user time to read and click each prompt without macOS
// timing out the underlying request.
const PrimerTimeoutSec = 30

// primeRelayTccPermissions requests the given TCC services from Relay's
// own process so the user sees "Relay wants to access X" prompts. Grants
// flow to the spawned MCP via responsible-parent attribution. Each primer
// blocks the calling goroutine while the user responds.
//
// Only the services that have a Cocoa primer get one; the rest (Microphone,
// AppleEvents, etc.) rely on first-use prompts from the MCP's own runtime
// calls. Outcomes are appended to result.SpawnOutput so the UI summary
// shows what happened.
func primeRelayTccPermissions(services []string, result *ResetMcpPermissionsResult) {
	if len(services) == 0 {
		return
	}
	// Bump Relay from .accessory (LSUIElement tray) to .regular for the duration
	// of the primer batch. Without this, macOS Sequoia suppresses the TCC
	// prompts even though Relay is /Applications-resident and signed.
	cocoaBeginForegroundActivation()
	defer cocoaEndForegroundActivation()

	// Services are already canonicalized by parseTccServices; no alias matching
	// needed here. Services without a Cocoa primer (microphone, appleevents)
	// rely on first-use prompts from the MCP itself at runtime and skip silently.
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
