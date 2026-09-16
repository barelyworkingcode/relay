package main

// R-S8: C7's sandbox input, built from relay's own settings, rendered by
// internal/sessions/sandbox and written to the file AuthorizeLaunch names in
// the LaunchSpec. Relay decides what a session may reach; the sandbox
// package decides how that is spelled in SBPL.

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

// C7's two fixed loopback denials: eve's dev server and relayLLM's status
// listener. The model endpoint's port is not a constant here — it is read
// from settings, so an operator who moved it does not silently lose the one
// loopback port a session is meant to reach.
var sandboxDeniedLoopbackPorts = []int{3000, 8181}

// sessionProfilesDir is C5's host data dir plus C7's "profiles/": relay
// writes the profile, relay-sessions' shim reads it by absolute path.
func sessionProfilesDir() string {
	return filepath.Join(bridge.ConfigDir(), "sessions", "profiles")
}

// writeSessionSandboxProfile is AuthorizeLaunch's extension point: it turns
// "this session wants sandboxing" into an SBPL file on disk and returns its
// absolute path. Every failure is a refusal to launch — there is no branch
// that returns an empty path and lets the caller spawn anyway.
func writeSessionSandboxProfile(settings *config.Settings, proj *config.Project, directory, sessionID string) (string, error) {
	spec, err := sandboxSpecForLaunch(settings, proj, directory)
	if err != nil {
		return "", err
	}
	return sandbox.Write(sessionProfilesDir(), sessionID, spec)
}

// sandboxProfilePath is the profile AuthorizeLaunch wrote for result's
// session, or "" for a session it left unsandboxed.
func sandboxProfilePath(result *LaunchResult) string {
	if result == nil || result.Spec.Sandbox == nil {
		return ""
	}
	return result.Spec.Sandbox.ProfilePath
}

// sandboxSpecForLaunch builds C7's input for one session, with SP2's
// amendments applied: `~/Library/Keychains` is absent from read_deny on
// purpose (denying it logs Claude Code out), `~/.claude.json`'s atomic-write
// siblings ride along with it, and `/private/tmp/cc-socks` plus the Darwin
// per-user temp dir are write-allowed because Claude Code and Apple's own
// tools write there whatever TMPDIR says.
func sandboxSpecForLaunch(settings *config.Settings, proj *config.Project, directory string) (sandbox.Spec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandbox.Spec{}, fmt.Errorf("resolve home directory: %w", err)
	}
	appSupport, err := os.UserConfigDir()
	if err != nil {
		return sandbox.Spec{}, fmt.Errorf("resolve application support directory: %w", err)
	}
	// relay's own directory comes from bridge.ConfigDir (which a test
	// redirects) while its siblings come from the OS: the two are the same
	// place in production, and a test that redirected only the first must
	// not end up denying a directory it never created.
	relayDir := bridge.ConfigDir()
	relayLLMDir := filepath.Join(appSupport, "relayLLM")
	eveDir := filepath.Join(appSupport, "eve")

	workDir := directory
	if proj != nil && proj.Path != "" {
		workDir = proj.Path
	}

	toolchainDirs := ensureToolchainDirs(home)

	writeAllow := []string{}
	if workDir != "" {
		writeAllow = append(writeAllow, workDir)
	}
	writeAllow = append(writeAllow, toolchainDirs...)
	// Both temp directories, deliberately: the session's own TMPDIR is
	// whatever relay's environment carries, while xcrun and Apple's command
	// line tools write to DARWIN_USER_TEMP_DIR whatever TMPDIR says (SP2
	// change 3, and `git` under a profile missing it logs an xcrun cache
	// failure on every call).
	writeAllow = append(writeAllow, "/private/tmp/cc-socks", os.TempDir())
	// Cleaned before comparing: getconf answers with a trailing slash and
	// os.TempDir does not, so the raw strings differ for the same directory.
	if darwin := darwinUserTempDir(); darwin != "" && filepath.Clean(darwin) != filepath.Clean(os.TempDir()) {
		writeAllow = append(writeAllow, darwin)
	}
	writeAllow = append(writeAllow, "/dev")

	spec := sandbox.Spec{
		WriteAllowDirs:  writeAllow,
		WriteAllowFiles: []string{filepath.Join(home, ".claude.json")},
		// C7's "<other sessions' data dirs>" needs no entry of its own: the
		// host keeps them under relay's own directory, which is denied whole.
		ReadDeny: append([]string{relayDir, relayLLMDir, eveDir}, otherProjectPaths(settings, proj, workDir)...),
		// Directory prefixes, not just the four named sockets C7 lists: a
		// literal-only denylist leaves any socket that appears in one of
		// these directories later reachable under (allow default) — SP2 row
		// 6 measured exactly that.
		UnixConnectDenyDirs: []string{relayDir, relayLLMDir, eveDir},
		UnixConnectAllow: []string{
			bridge.SocketPath(),
			bridge.ModelSocketPath(),
			service.RelaySessionsHookSocketPath(relayDir),
		},
		TCPLoopbackDeny: sandboxDeniedLoopbackPorts,
		DenySetIDExec:   true,
	}
	if port, ok := modelEndpointLoopbackPort(settings); ok {
		spec.TCPLoopbackAllow = []int{port}
	}
	return spec, nil
}

// ensureToolchainDirs returns C7's agent-state and toolchain write
// allowances, creating any that are missing first.
//
// A (subpath …) rule lets the session create that directory itself but not
// an ancestor of it: on a machine with no ~/go, an allowance on ~/go/pkg
// leaves `go build` unable to create the module cache at all (measured).
// Creating them here is what makes each allowance real, and it is exactly
// what the toolchain would do if it were allowed to.
func ensureToolchainDirs(home string) []string {
	dirs := []string{
		filepath.Join(home, ".cache"),
		filepath.Join(home, "go", "pkg"),
		filepath.Join(home, ".npm"),
		filepath.Join(home, "Library", "Caches"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".pi"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Warn("session sandbox: could not create a write-allowed directory", "dir", dir, "error", err)
		}
	}
	return dirs
}

var (
	darwinTempOnce sync.Once
	darwinTempDir  string
)

// darwinUserTempDir is `getconf DARWIN_USER_TEMP_DIR`, the per-user temp
// directory Apple's own tools use regardless of TMPDIR. Read through getconf
// rather than from the environment because the environment is exactly what
// those tools ignore; memoized because it is fixed for the life of the
// process. An empty answer means the entry is simply omitted — os.TempDir()
// still covers the session's own TMPDIR.
func darwinUserTempDir() string {
	darwinTempOnce.Do(func() {
		out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
		if err != nil {
			slog.Warn("session sandbox: getconf DARWIN_USER_TEMP_DIR failed", "error", err)
			return
		}
		darwinTempDir = strings.TrimSpace(string(out))
	})
	return darwinTempDir
}

// otherProjectPaths is C7's "<other projects' paths>", read from settings at
// launch. A remote record has no path at all and a hosted project's path
// exists on the SSH target — denying either would deny a local directory
// that merely shares its spelling.
//
// A project whose path CONTAINS this session's working directory is skipped:
// with nested projects (a directory project inside a wider one) the deny
// would land on the session's own tree, which is a dead session rather than
// a confined one. A project nested inside this one stays denied.
func otherProjectPaths(settings *config.Settings, self *config.Project, workDir string) []string {
	if settings == nil {
		return nil
	}
	var out []string
	for i := range settings.Projects {
		p := &settings.Projects[i]
		if self != nil && p.ID == self.ID {
			continue
		}
		if p.Path == "" || p.IsRemote() || p.IsHosted() {
			continue
		}
		cleaned := filepath.Clean(p.Path)
		if workDir != "" && lexicalDirWithin(filepath.Clean(workDir), cleaned) {
			continue
		}
		out = append(out, cleaned)
	}
	sort.Strings(out)
	return out
}

// modelEndpointLoopbackPort is C7's tcp_loopback_allow: the model endpoint's
// TCP port when one is configured. An absent block means no TCP listener at
// all (C8), so there is nothing to allow — model.sock still carries every
// model call.
func modelEndpointLoopbackPort(settings *config.Settings) (int, bool) {
	if settings == nil || settings.ModelEndpoint == nil || settings.ModelEndpoint.Listen == "" {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(settings.ModelEndpoint.Listen)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}
