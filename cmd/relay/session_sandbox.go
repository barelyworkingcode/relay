package main

// R-S8: C7's sandbox input, built from relay's own settings, rendered by
// internal/sessions/sandbox and written to the file AuthorizeLaunch names in
// the LaunchSpec. Relay decides what a session may reach; the sandbox
// package decides how that is spelled in SBPL.
//
// File access is denied by default. A session reaches its project, the temp
// directories and /dev every process needs, the developer tools, and the
// `read` and `read_write` folders of the template it launches from
// (settings.json). Nothing here lists a directory to deny: relay's own data,
// other projects, eve's data and ~/.ssh are unreachable unless a template
// grants them.

import (
	"errors"
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
func writeSessionSandboxProfile(settings *config.Settings, proj *config.Project, directory, sessionID, kind string, tmpl *config.TerminalTemplate) (string, error) {
	spec, err := sandboxSpecForLaunch(settings, proj, directory, kind, tmpl)
	if err != nil {
		return "", err
	}
	path, err := sandbox.Write(sessionProfilesDir(), sessionID, spec)
	var linked *sandbox.LinkedGrantError
	if errors.As(err, &linked) {
		slog.Warn("session sandbox: read-write grant refused", "session", sessionID, "kind", kind, "grant", linked.Grant, "link", linked.Link)
	}
	return path, err
}

// sandboxProfilePath is the profile AuthorizeLaunch wrote for result's
// session, or "" for a session it left unsandboxed.
func sandboxProfilePath(result *LaunchResult) string {
	if result == nil || result.Spec.Sandbox == nil {
		return ""
	}
	return result.Spec.Sandbox.ProfilePath
}

// sandboxSpecForLaunch builds C7's input for one session. tmpl is the template
// whose folders the session gets: the one a terminal launches, or for a
// claude, pi or chat session the template named for its kind
// (templateForKind). A nil tmpl grants only what every session gets.
//
// Every path returned is a grant. What relay does not name is what a session
// cannot reach, so a directory worth protecting needs no entry here. What a
// tool needs to run lives in the template, not in this function: the project's
// own directory, the temp directories and /dev every process uses, and the
// developer tools are the only grants that do not come from one.
func sandboxSpecForLaunch(settings *config.Settings, proj *config.Project, directory, kind string, tmpl *config.TerminalTemplate) (sandbox.Spec, error) {
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
	// Both temp directories, deliberately: the session's own TMPDIR is
	// whatever relay's environment carries, while xcrun and Apple's command
	// line tools write to DARWIN_USER_TEMP_DIR whatever TMPDIR says (SP2
	// change 3, and `git` under a profile missing it logs an xcrun cache
	// failure on every call).
	readWrite = append(readWrite, os.TempDir())
	// Cleaned before comparing: getconf answers with a trailing slash and
	// os.TempDir does not, so the raw strings differ for the same directory.
	if darwin := darwinUserTempDir(); darwin != "" && filepath.Clean(darwin) != filepath.Clean(os.TempDir()) {
		readWrite = append(readWrite, darwin)
	}
	readWrite = append(readWrite, "/dev")
	// pi's transcripts live under relay's own directory, which no template
	// can name portably (it moves with `relay --config-dir`), so the one leaf a
	// pi session writes its transcript into is granted here, for that kind
	// only. It means one sandboxed pi session can read another's transcripts
	// (documented tradeoff, not fixed here).
	if kind == KindPi {
		readWrite = append(readWrite, sessionPiSessionsDir())
	}

	var read, readFiles, readWriteFiles, deny []string
	if dev := developerTools(); dev != "" {
		read = append(read, dev)
	}
	if tmpl != nil {
		var err error
		if read, readFiles, err = addTemplateGrants(read, readFiles, "read", tmpl.Read, home); err != nil {
			return sandbox.Spec{}, fmt.Errorf("template %q: %w", tmpl.ID, err)
		}
		if readWrite, readWriteFiles, err = addTemplateGrants(readWrite, readWriteFiles, "read_write", tmpl.ReadWrite, home); err != nil {
			return sandbox.Spec{}, fmt.Errorf("template %q: %w", tmpl.ID, err)
		}
		// A denied file needs no separate spelling: a subtree rule on a
		// regular file matches that file.
		denyDirs, denyFiles, err := addTemplateGrants(nil, nil, "deny", tmpl.Deny, home)
		if err != nil {
			return sandbox.Spec{}, fmt.Errorf("template %q: %w", tmpl.ID, err)
		}
		deny = append(denyDirs, denyFiles...)
	}
	ensureGrantDirs(readWrite)

	spec := sandbox.Spec{
		Read:           read,
		ReadFiles:      readFiles,
		ReadWrite:      readWrite,
		ReadWriteFiles: readWriteFiles,
		Deny:           deny,
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

// addTemplateGrants expands one of a template's folder lists and sorts each
// entry into directories and single files: an entry that exists as a regular
// file is a file grant (a read-write one gets its atomic-write siblings), and
// anything else, including a path that does not exist yet, is a subtree. An
// entry relay cannot place refuses the launch: a grant it cannot enforce is
// one it must not drop quietly, or the operator believes a folder is reachable
// when it is not.
func addTemplateGrants(dirs, files []string, field string, entries []string, home string) ([]string, []string, error) {
	for _, raw := range entries {
		p := raw
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("%s %q is not an absolute path or a ~ path", field, raw)
		}
		p = filepath.Clean(p)
		if p == "/" {
			return nil, nil, fmt.Errorf("%s %q grants the whole filesystem", field, raw)
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			files = append(files, p)
			continue
		}
		dirs = append(dirs, p)
	}
	return dirs, files, nil
}

// ensureGrantDirs creates any read-write directory that does not exist yet.
//
// A (subpath …) rule lets the session create that directory itself but not an
// ancestor of it: on a machine with no ~/go, a grant on ~/go/pkg leaves `go
// build` unable to create the module cache at all (measured). Creating them
// here is what makes each grant real, and it is exactly what the toolchain
// would do if it were allowed to. A base name with an extension is skipped: it
// is a file that has not been written yet (~/.claude.json), and a directory
// made under that name would break the tool that expects a file.
func ensureGrantDirs(dirs []string) {
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		if strings.Contains(strings.TrimPrefix(filepath.Base(dir), "."), ".") {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Warn("session sandbox: could not create a granted directory", "dir", dir, "error", err)
		}
	}
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
