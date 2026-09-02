package main

import (
	"net"
	"strings"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// requireService returns a bridge client for command, or exits — naming
// command by its own name — when relay is not running.
//
// This is deliberate: the probe dial is the very first thing this function
// does, before anything that could touch settings.json. A mutating command
// that read or opened the file first, and only then discovered there was no
// service to broker its write, would have already spent the one guarantee
// brokering exists to give: that settings.json is left exactly as it was
// when there is nothing to write it through.
func requireService(command string) *bridge.Client {
	if !serviceReachable() {
		exitError(serviceRequiredMessage(command))
	}
	return bridge.NewClient("")
}

func serviceReachable() bool {
	conn, err := net.Dial("unix", bridge.SocketPath())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func serviceRequiredMessage(command string) string {
	return "relay is not running; `" + command + "` requires the service.\n" +
		"  relay is the sole broker of its own credentials: the secrets are sealed and\n" +
		"  only the tray holds the key (ADR-017 decision 2). Start Relay and retry.\n" +
		"  Read commands still work with relay stopped: `relay credential list`,\n" +
		"  `relay grant`, `relay audit`."
}

// sshRefusalMessage is §6.6's shared refusal text: what a human reads when a
// gated operation cannot display a presence prompt on this connection.
const sshRefusalMessage = "refused: this needs your confirmation on the Mac's screen, and the session this\n" +
	"  command is running in cannot show a prompt (for example, you are over SSH).\n" +
	"  There is no queue and no pending-approval list.\n" +
	"  Run it from a terminal in the logged-in desktop session, or from the Relay\n" +
	"  Settings window.\n" +
	"  Read commands are unaffected: relay audit, relay grant, and every `list`."

// adminOpErrorText renders an admin_op failure for a human at a terminal.
//
// This is subtle: the bridge round trip discards the error's type — checkError
// wraps every non-nil response code in a plain fmt.Errorf carrying only the
// message text, so there is no presence.ErrNoSession left on the CLI side of
// the wire to errors.Is against. The gate's own refusal for that case is
// accurate but terse ("no session can display a presence prompt"); matching
// its exact, stable sentinel text is what lets a privileged operation over
// SSH surface as §6.6's full explanation instead of a generic RPC error. Any
// other failure — a validation error, a not-found, an issuance-auditing
// refusal — prints exactly as the service returned it.
func adminOpErrorText(err error) string {
	if strings.Contains(err.Error(), presence.ErrNoSession.Error()) {
		return sshRefusalMessage
	}
	return err.Error()
}
