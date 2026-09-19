package main

// R-S8: C7's sandbox input, built from relay's own settings, rendered by
// internal/sessions/sandbox and written to the file AuthorizeLaunch names in
// the LaunchSpec. Relay decides what a session may reach; the sandbox
// package decides how that is spelled in SBPL.
//
// File access is denied by default. A session reaches its project, a fixed
// set of toolchain and temp directories, a short read-only set, and whatever
// `sandbox` in settings.json adds. Nothing here lists a directory to deny:
// relay's own data, other projects, eve's data and ~/.ssh are unreachable
// because nothing grants them.

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

// C7's two fixed loopback denials: eve's dev server and relayLLM's status
// listener. Relay's own API port joins them at build time (SH §5.2) because
// it is bound only when RELAY_API_LISTEN asks for it. The model endpoint's
// port is not a constant here either — it is read from settings, so an
// operator who moved it does not silently lose the one loopback port a
// session is meant to reach.
var sandboxDeniedLoopbackPorts = []int{3000, 8181}

// eveServiceID is the id eve registers under (`relay service register --id
// eve`, eve/docs/setup.md). Eve's auth material lives in its own data
// directory rather than anywhere relay owns, so the socket denial below is
// derived from that registered record and nothing else: a machine where eve is not
// registered, or is registered under some other id, gets no eve rule at all
// rather than one naming a path eve never writes.
const eveServiceID = "eve"

// sessionProfilesDir is C5's host data dir plus C7's "profiles/": relay
// writes the profile, relay-sessions' shim reads it by absolute path.
func sessionProfilesDir() string {
	return filepath.Join(bridge.ConfigDir(), "sessions", "profiles")
}

// sessionPiSessionsDir is where pi's own JSONL transcripts live under this
// host's data directory (provider.PiProvider.sessionDir, computed
// independently from the same bridge.ConfigDir()) — the one leaf inside
// relay's own directory a sandboxed pi launch is granted, or it cannot write
// its own transcript on its first turn.
func sessionPiSessionsDir() string {
	return filepath.Join(bridge.ConfigDir(), "sessions", "pi-sessions")
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
// amendments applied: `~/Library/Keychains` is readable on purpose (denying
// it logs Claude Code out), `~/.claude.json`'s atomic-write siblings ride
// along with it, and `/private/tmp/cc-socks` plus the Darwin per-user temp dir
// are writable because Claude Code and Apple's own tools write there whatever
// TMPDIR says.
//
// Every path returned is a grant. What relay does not name is what a session
// cannot reach, so a new directory worth protecting needs no entry here.
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
	// not end up naming a directory it never created.
	relayDir := bridge.ConfigDir()
	relayLLMDir := filepath.Join(appSupport, "relayLLM")

	// Sockets are not files: connecting to one is a network operation the
	// file rules do not cover, so the socket denials stay a list.
	unixConnectDeny := []string{relayDir, relayLLMDir}
	if eveData := eveDataDir(settings); eveData != "" {
		unixConnectDeny = append(unixConnectDeny, eveData)
	}

	workDir := directory
	if proj != nil && proj.Path != "" {
		workDir = proj.Path
	}

	readWrite := []string{}
	if workDir != "" {
		readWrite = append(readWrite, workDir)
	}
	readWrite = append(readWrite, ensureToolchainDirs(home)...)
	// Both temp directories, deliberately: the session's own TMPDIR is
	// whatever relay's environment carries, while xcrun and Apple's command
	// line tools write to DARWIN_USER_TEMP_DIR whatever TMPDIR says (SP2
	// change 3, and `git` under a profile missing it logs an xcrun cache
	// failure on every call).
	readWrite = append(readWrite, "/private/tmp/cc-socks", os.TempDir())
	// Cleaned before comparing: getconf answers with a trailing slash and
	// os.TempDir does not, so the raw strings differ for the same directory.
	if darwin := darwinUserTempDir(); darwin != "" && filepath.Clean(darwin) != filepath.Clean(os.TempDir()) {
		readWrite = append(readWrite, darwin)
	}
	// pi's transcripts live under relay's own directory, which nothing else
	// grants; this one leaf is what lets a sandboxed pi write its own on its
	// first turn. It means one sandboxed pi session can read another's
	// transcripts (documented tradeoff, not fixed here).
	readWrite = append(readWrite, "/dev", sessionPiSessionsDir())

	read := []string{filepath.Join(home, "Library", "Keychains"), "/opt/homebrew"}
	if dev := developerTools(); dev != "" {
		read = append(read, dev)
	}
	readFiles := []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".zshenv"),
		filepath.Join(home, ".zprofile"),
		filepath.Join(home, ".zshrc"),
	}

	extraRead, extraReadWrite, err := sandboxSettingsPaths(settings, home)
	if err != nil {
		return sandbox.Spec{}, err
	}
	read = append(read, extraRead...)
	readWrite = append(readWrite, extraReadWrite...)

	spec := sandbox.Spec{
		Read:           read,
		ReadFiles:      readFiles,
		ReadWrite:      readWrite,
		ReadWriteFiles: []string{filepath.Join(home, ".claude.json")},
		// Directory prefixes, not just the four named sockets C7 lists: a
		// literal-only denylist leaves any socket that appears in one of
		// these directories later reachable under (allow default) — SP2 row
		// 6 measured exactly that.
		UnixConnectDenyDirs: unixConnectDeny,
		UnixConnectAllow: []string{
			bridge.SocketPath(),
			bridge.ModelSocketPath(),
			service.RelaySessionsHookSocketPath(relayDir),
		},
		TCPLoopbackDeny: deniedLoopbackPorts(),
		DenySetIDExec:   true,
	}
	if port, ok := modelEndpointLoopbackPort(settings); ok {
		spec.TCPLoopbackAllow = []int{port}
	}
	return spec, nil
}

// sandboxSettingsPaths is `sandbox` in settings.json, ~-expanded and checked.
// An entry that is not an absolute or ~ path, or that names the whole
// filesystem, refuses the launch: a grant relay cannot place is a grant it
// cannot enforce, and dropping it quietly would leave the operator believing a
// tool's directory was reachable when it was not.
func sandboxSettingsPaths(settings *config.Settings, home string) (read, readWrite []string, err error) {
	if settings == nil || settings.Sandbox == nil {
		return nil, nil, nil
	}
	if read, err = expandSandboxPaths("sandbox.read", settings.Sandbox.Read, home); err != nil {
		return nil, nil, err
	}
	if readWrite, err = expandSandboxPaths("sandbox.read_write", settings.Sandbox.ReadWrite, home); err != nil {
		return nil, nil, err
	}
	return read, readWrite, nil
}

func expandSandboxPaths(field string, in []string, home string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("settings %s: %q is not an absolute path or a ~ path", field, raw)
		}
		p = filepath.Clean(p)
		if p == "/" {
			return nil, fmt.Errorf("settings %s: %q grants the whole filesystem", field, raw)
		}
		out = append(out, p)
	}
	return out, nil
}

// developerToolsDir is where `git`, `clang` and the rest resolve their real
// binary from: the target of /var/select/developer_dir. Under an Xcode install
// that is `<Xcode>.app/Contents/Developer`, and the tools also read the app
// bundle's own metadata beside it, so the bundle's Contents is the directory
// granted; under the command line tools it is the tools directory itself. It
// is read-only application content, not user data. An empty answer means no
// developer tools are installed and there is nothing to grant.
func developerToolsDir() string {
	target, err := os.Readlink("/private/var/select/developer_dir")
	if err != nil {
		if _, statErr := os.Stat("/Library/Developer/CommandLineTools"); statErr == nil {
			return "/Library/Developer/CommandLineTools"
		}
		return ""
	}
	return developerToolsRoot(target)
}

// developerToolsRoot maps developer_dir's target to the directory granted: the
// enclosing app bundle's Contents for an Xcode path, the path itself otherwise.
func developerToolsRoot(target string) string {
	if i := strings.Index(target, ".app/Contents"); i >= 0 {
		return target[:i+len(".app/Contents")]
	}
	return target
}

// developerTools is developerToolsDir behind a var so the golden test does not
// depend on which developer tools the machine running it has installed.
var developerTools = developerToolsDir

// eveDataDir is where eve keeps auth.json and sessions.json, resolved from
// eve's own registered service record rather than guessed: eve reads
// `--data <path>` from its argv and otherwise writes beside its checkout
// (`eve/server.js` parseDataDir). A relative `--data` resolves against the
// working directory relay launches the service in, which is what eve's
// process.cwd() is.
//
// An empty answer — eve unregistered, or registered with no working
// directory to anchor the default against — means no eve rule is emitted.
// That is deliberate: a socket rule naming a directory nothing writes reads
// as enforcement and denies nothing. It feeds only the socket denial now; eve's
// files are unreachable because nothing grants them.
func eveDataDir(settings *config.Settings) string {
	if settings == nil {
		return ""
	}
	svc, _ := config.FindServiceByID(settings, eveServiceID)
	if svc == nil {
		return ""
	}
	for i, arg := range svc.Args {
		if arg != "--data" || i+1 >= len(svc.Args) {
			continue
		}
		data := svc.Args[i+1]
		if filepath.IsAbs(data) {
			return filepath.Clean(data)
		}
		if svc.WorkingDir == "" {
			return ""
		}
		return filepath.Join(svc.WorkingDir, data)
	}
	if svc.WorkingDir == "" {
		return ""
	}
	return filepath.Join(svc.WorkingDir, "data")
}

// deniedLoopbackPorts is SH §5.2's TCP row: the two fixed ports plus relay's
// own API listener when an operator bound one.
func deniedLoopbackPorts() []int {
	ports := append([]int(nil), sandboxDeniedLoopbackPorts...)
	if port, ok := apiListenLoopbackPort(); ok {
		ports = append(ports, port)
	}
	return ports
}

// apiListenLoopbackPort is the port RELAY_API_LISTEN names, read from the
// environment because that is the only place relay itself reads it
// (trayapp's ListenLoopback call). Unset is the default and contributes
// nothing — there is no listener to deny.
func apiListenLoopbackPort() (int, bool) {
	addr := os.Getenv(EnvAPIListen)
	if addr == "" {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
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
