// Package terminal hosts PTY-backed terminal sessions for relay-sessions
// (plan-broker-and-sessions.md's R-S6 unit): local shells, agent CLIs and
// SSH-backed remote shells, each spawned through an internal/sessions/shim
// child rather than execed directly.
//
// This package spawns the shim itself (buildShimCmd in session.go) rather
// than going through hostapi's own spawnShim, an unexported method on
// hostapi.Server. hostapi's /launch dispatches "pty" requests to this
// package's Manager.
package terminal
