package main

import (
	"net"

	"relaygo/bridge"
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
