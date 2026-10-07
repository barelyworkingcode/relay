package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// readOnlyRootsChanged is the /terminate reason for a session ended because
// the set of registered local projects moved under its sandbox profile.
const readOnlyRootsChanged = "read_only_roots_changed"

// readOnlyProjectsSetting reads settings.readOnlyProjects from a session's
// client settings. Absent is false. Anything present that is not a JSON
// bool, null included, is an error: the option widens what a session reads,
// so a value that only looks truthy never turns it on.
func readOnlyProjectsSetting(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, fmt.Errorf("session settings: %w", err)
	}
	v, ok := fields["readOnlyProjects"]
	if !ok {
		return false, nil
	}
	switch string(bytes.TrimSpace(v)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("session settings: readOnlyProjects must be a boolean")
}

// readOnlyProjectRoots is every directory a read-only session may read: each
// local, non-hosted project with a path, symlinks resolved so the profile
// names the real directory. A path that does not resolve is skipped with a
// warning rather than failing the launch, because one project whose folder
// moved must not blind the session to the rest. The result is never nil, so
// a caller can pass it on as "read-only mode" even with no roots.
func readOnlyProjectRoots(settings *config.Settings) []string {
	roots := []string{}
	if settings == nil {
		return roots
	}
	seen := map[string]bool{}
	for i := range settings.Projects {
		p := &settings.Projects[i]
		if p.IsRemote() || p.IsHosted() || p.Path == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			slog.Warn("read-only roots: project path skipped", "project", p.ID, "error", err)
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		roots = append(roots, resolved)
	}
	return roots
}

// endReadOnlySessions terminates every live ledger session launched with
// readOnlyProjects, so none outlives the profile that names its roots. The
// ledger record stays: the session goes dormant when the host reports its
// exit, and a resume rebuilds the profile from the roots current then.
func (d sessionRouteDeps) endReadOnlySessions(reason string) {
	if !d.ready() || d.sessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionHostRequestTimeout)
	defer cancel()

	for _, rec := range d.sessions.All() {
		if rec.State != ledger.StateLive {
			continue
		}
		body, err := decodeStoredSessionRequest(rec)
		if err != nil {
			slog.Warn("read-only sweep: stored request unreadable", "session", rec.SessionID, "error", err)
			continue
		}
		if on, _ := readOnlyProjectsSetting(body.Settings); !on {
			continue
		}
		endSandboxSession(ctx, d, rec.SessionID, reason)
	}
}
