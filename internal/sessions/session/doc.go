// Package session hosts persistent Claude Code / pi CLI sessions for
// relay-sessions (plan-broker-and-sessions.md's R-S7c unit): creation,
// on-disk persistence, resume, and periodic sweep of stale session/pi-session
// files. It spawns providers through internal/sessions/provider's
// ClaudeProvider/PiProvider, the same credential-safe spawn path R-S7b built,
// and applies internal/sessions/terminal's two hard-won R-S6 patterns: an
// atomic slot reservation so a duplicate-id race can never orphan a live
// process outside this package's own table, and never holding mu across a
// blocking provider call (Start, Kill, SendMessage all do their own I/O
// unlocked).
//
// Decision SH-6 (plan-broker-and-sessions.md's C5 section, "Resume (user
// action only)") means this package never respawns a project-bound
// session's dead provider on its own — SendMessage returns ErrResumeRequired
// instead, and only a caller-supplied resume (Manager.Create with
// CreateSpec.Resume, driven by relay's own POST /launch resume:true) brings
// it back. Only an ad-hoc session (ProjectID == "") still respawns
// automatically, matching the plan's own exception for that case.
//
// Wiring Manager into internal/sessions/hostapi's POST /launch dispatch for
// kind claude/pi is left to a later integration unit, the same gap R-S6's
// terminal package reported for pty: hostapi's handleLaunch has no
// extension point yet for a provider-backed Launcher, and this package does
// not reach into hostapi (an R-S5, already-reviewed package) to add one
// uninvited.
package session
