package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// hostTargetPattern is decision 10's "fail fast, never prompt" charset check
// applied a step earlier, at the point the operator types it: a target that
// could carry a shell metacharacter or whitespace must never reach exec.Command
// as an ssh argument, even though exec.Command (unlike a shell) would not
// glue it into a single command line — a bad value here is confusion, not an
// injection, but there is no legitimate ssh target this pattern excludes.
var hostTargetPattern = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9._:-]+$`)

// ValidateHost enforces docs/ssh-hosts.md's host rules: name unique
// case-insensitively (excluding excludeID, so a no-op rename of the host
// being edited doesn't collide with itself), target non-empty and
// shell-metacharacter-free, port 0 or 1-65535, identity_file absolute or
// empty.
func ValidateHost(h *Host, existing []Host, excludeID string) error {
	name := strings.TrimSpace(h.Name)
	if name == "" {
		return fmt.Errorf("host name is required")
	}
	for _, o := range existing {
		if o.ID == excludeID {
			continue
		}
		if strings.EqualFold(o.Name, name) {
			return fmt.Errorf("a host named %q already exists", name)
		}
	}
	h.Name = name

	target := strings.TrimSpace(h.Target)
	if target == "" {
		return fmt.Errorf("host target is required")
	}
	if !hostTargetPattern.MatchString(target) {
		return fmt.Errorf("host target %q is not valid: expected [user@]host, using only letters, digits, '.', '_', '-', ':' and one '@'", target)
	}
	// sshhost.SSHArgv places the target positionally, after its fixed option
	// prefix, with no "--" to end option parsing. A host or user part
	// beginning with '-' (e.g. "-oProxyCommand=...", "-A") is therefore an
	// ssh option, not a destination, no matter what the rest of SSHArgv
	// looks like — reject it here, since hostTargetPattern's charset admits
	// '-' anywhere including first.
	user, host := splitHostTarget(target)
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("host target %q is not valid: the host part must not begin with '-'", target)
	}
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("host target %q is not valid: the user part must not begin with '-'", target)
	}
	h.Target = target

	if h.Port != 0 && (h.Port < 1 || h.Port > 65535) {
		return fmt.Errorf("host port %d is out of range: must be 0 (default) or 1-65535", h.Port)
	}

	if h.IdentityFile != "" && !filepath.IsAbs(h.IdentityFile) {
		return fmt.Errorf("host identity_file must be an absolute path or empty: %q", h.IdentityFile)
	}

	return nil
}

// splitHostTarget separates a validated [user@]host target on its last '@',
// matching how ssh itself reads the destination. user is "" when target
// carries no '@'.
func splitHostTarget(target string) (user, host string) {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[:i], target[i+1:]
	}
	return "", target
}
