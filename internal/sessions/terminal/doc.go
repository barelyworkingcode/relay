// Package terminal hosts PTY-backed terminal sessions for relay-sessions
// (plan-broker-and-sessions.md's R-S6 unit): local shells, agent CLIs and
// SSH-backed remote shells, each spawned through an internal/sessions/shim
// child rather than execed directly.
//
// This package spawns the shim itself (buildShimCmd in session.go) instead
// of calling into internal/sessions/hostapi, whose own spawnShim is an
// unexported method on hostapi.Server: that skeleton also hard-refuses any
// "pty" LaunchRequest (hostapi/launch.go's own doc comment names this as
// R-S6's job). Wiring this package's Manager into hostapi's dispatch so a
// real relay-sessions process can serve pty launches end to end is left to
// a later integration unit (hostapi's handleLaunch would need an extension
// point for a pty-capable Launcher; none exists yet — see this unit's own
// report for that gap).
package terminal
