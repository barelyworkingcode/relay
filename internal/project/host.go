package project

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/config"
)

// validateHostShape is ValidateShape's host-specific half, called only from
// the non-remote branch (a host project is kind: local in shape). Returns
// nil immediately for a console project.
//
// Relay-brokered tools, mounts, cwd auth and the skill generator all
// presume a console directory relay itself can reach — a host project's
// directory is reachable only over ssh (docs/ssh-hosts.md decision 6), so
// each of these reads as a feature that would silently no-op rather than a
// boundary, and is refused outright instead.
func validateHostShape(proj *config.Project) error {
	if !proj.IsHosted() {
		return nil
	}
	if len(proj.AllowedMcpIDs) > 0 {
		return fmt.Errorf("host project must not set allowed_mcp_ids: relay-brokered tools live on the console only — a host session gets Claude Code's built-in tools (docs/ssh-hosts.md)")
	}
	if proj.AllowCwdAuth {
		return fmt.Errorf("host project must not enable allow_cwd_auth: a caller's cwd is checked against the console filesystem, and a host project's directory is not on it")
	}
	if proj.GenerateSkill {
		return fmt.Errorf("host project must not enable generate_skill: skills are written under <path>/.claude/skills on the console filesystem, and a host project's path is not there")
	}
	// Mounts are already refused for any non-remote project by ValidateMounts,
	// which runs before this function and names the same reasoning.
	return nil
}

// ValidateHostRef checks proj.HostID against the live host list — the one
// host rule ValidateShape cannot make on its own, since it takes no
// *config.Settings. Called from CreateWithTokenKind and ApplyCreate/ApplyUpdate,
// which already have s in scope, the same split ValidateGrants(surfaces) uses
// for the rule that needs live MCP data ValidateShape doesn't have either.
func ValidateHostRef(s *config.Settings, proj *config.Project) error {
	if !proj.IsHosted() {
		return nil
	}
	if _, idx := config.FindHostByID(s, proj.HostID); idx < 0 {
		return fmt.Errorf("host %q does not exist", proj.HostID)
	}
	return nil
}
