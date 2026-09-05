// Package sshhost is the one place relay turns a config.Host into ssh
// arguments and a remote command line (docs/ssh-hosts.md, decision 2).
// relayLLM and eve receive SSHArgv's result as a ready-to-exec prefix; they
// never derive it themselves.
package sshhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// controlPathBudget is sun_path's 104-byte limit on macOS minus the ~40
// bytes OpenSSH's %C hash and separator can add to ControlPath, leaving 90
// bytes of headroom for the directory component (docs/ssh-hosts.md).
const controlPathBudget = 90

// ControlDir returns the directory every ssh invocation shares as its
// ControlMaster socket directory, creating it 0700 if needed. It prefers
// "<relay data dir>/run/ssh"; when that path would leave too little room in
// sun_path for OpenSSH's %C hash, it falls back to a per-uid temp directory,
// which is short by construction.
func ControlDir() (string, error) {
	dir := controlDirFor(bridge.ConfigDir(), os.Getuid())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create ssh control dir %q: %w", dir, err)
	}
	return dir, nil
}

// controlDirFor picks the control-socket directory. The relay data dir is
// preferred, but a path with whitespace is unusable: ssh's -o parser splits
// "ControlPath=/a b/%C" at the space and refuses the option ("extra
// arguments at end of line"), and the macOS data dir lives under
// "Application Support".
func controlDirFor(configDir string, uid int) string {
	dir := filepath.Join(configDir, "run", "ssh")
	if len(dir) >= controlPathBudget || strings.ContainsAny(dir, " \t\n\"'") {
		return fmt.Sprintf("/tmp/relay-ssh-%d", uid)
	}
	return dir
}

// SSHArgv returns the ssh argv prefix for h, ending in the destination.
// Callers append -T or -tt, then --, then a remote command (RemoteCommand's
// result). The option order is fixed so every consumer's argv is
// byte-identical for the same host (docs/ssh-hosts.md).
func SSHArgv(h config.Host, controlDir string) []string {
	argv := []string{
		"ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlDir + "/%C",
		"-o", "ControlPersist=600",
	}
	if h.Port != 0 {
		argv = append(argv, "-p", strconv.Itoa(h.Port))
	}
	if h.IdentityFile != "" {
		argv = append(argv, "-i", h.IdentityFile)
	}
	argv = append(argv, h.Target)
	return argv
}

// shQuote single-quotes s for a POSIX sh command line: every ' becomes
// '\” (close the quote, an escaped literal quote, reopen the quote), the
// one escaping rule every Bourne-family shell agrees on (decision 8).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildScript renders the decoded form RemoteCommand's launcher carries:
// `cd '<cwd>' && exec env 'K'='v' … '<argv0>' '<arg1>' …`, with the cd
// clause omitted entirely when cwd is empty. env is walked in sorted key
// order so the same input always produces the same bytes — load-bearing for
// the fixture tests this shares with relayLLM's vendored copy and eve's
// ssh-command.js (docs/ssh-hosts.md Fixtures).
func buildScript(cwd string, argv []string, env map[string]string) string {
	var b strings.Builder
	if cwd != "" {
		b.WriteString("cd ")
		b.WriteString(shQuote(cwd))
		b.WriteString(" && ")
	}
	b.WriteString("exec env")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(shQuote(k))
		b.WriteString("=")
		b.WriteString(shQuote(env[k]))
	}
	for _, a := range argv {
		b.WriteString(" ")
		b.WriteString(shQuote(a))
	}
	return b.String()
}

// launcherFor wraps script in decision 8's fixed, shell-agnostic launcher:
// the only characters the destination's login shell ever parses are
// [A-Za-z0-9+/=] inside a single-quoted string, which every Bourne-family
// shell treats identically regardless of whether it is sh, bash, zsh or
// fish.
func launcherFor(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return `sh -c 'eval "$(printf %s ` + encoded + ` | base64 -d)"'`
}

// RemoteCommand builds the one remote command line every consumer (relay,
// relayLLM, eve) execs after ssh_argv + ["-T"|"-tt", "--"]: cd into cwd (if
// set), then exec argv with env set, all inside decision 8's base64+eval
// launcher so no login shell's quoting rules can misparse it.
func RemoteCommand(cwd string, argv []string, env map[string]string) string {
	return launcherFor(buildScript(cwd, argv, env))
}

// runner execs name with args and returns combined-separated stdout/stderr.
// A func var, not exec.Command called directly, so the hermetic test tier
// can stub it out — Probe/Check/Disconnect never touch a real socket in
// `go test ./...` (see the //go:build live test for the real path).
var runner = func(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// SetRunnerForTest overrides the exec seam every Probe/Check/Disconnect call
// goes through, and returns a func that restores the previous one (call it
// via t.Cleanup). Exported, not just package-internal, so a caller of this
// package — cmd/relay's route/IPC tests included — can keep its own suite
// hermetic (docs/ssh-hosts.md: "Do not shell out to ssh in hermetic
// tests") without duplicating a second exec seam of its own.
func SetRunnerForTest(fn func(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)) (restore func()) {
	prev := runner
	runner = fn
	return func() { runner = prev }
}

// probeTimeout bounds Probe's whole round trip (docs/ssh-hosts.md decision
// 10): a host that would prompt for a password must fail loudly rather than
// hang a headless caller forever.
const probeTimeout = 30 * time.Second

// probe script sentinels. Each marks the start of the next section's output
// so parsing does not depend on line counts — a `command -v` that finds
// nothing prints no line at all, which would desynchronize a purely
// positional parse.
const (
	sentinelOS          = "@@RELAY_PROBE_OS@@"
	sentinelArch        = "@@RELAY_PROBE_ARCH@@"
	sentinelHome        = "@@RELAY_PROBE_HOME@@"
	sentinelShell       = "@@RELAY_PROBE_SHELL@@"
	sentinelLoginNode   = "@@RELAY_PROBE_LOGIN_NODE@@"
	sentinelLoginClaude = "@@RELAY_PROBE_LOGIN_CLAUDE@@"
	sentinelPlainNode   = "@@RELAY_PROBE_PLAIN_NODE@@"
	sentinelPlainClaude = "@@RELAY_PROBE_PLAIN_CLAUDE@@"
	sentinelEnd         = "@@RELAY_PROBE_END@@"
)

// probeScript prints uname -s / uname -m / $HOME / $SHELL, then the
// interactive login shell's idea of where node and claude live (decision 9),
// then the plain-PATH fallback for each — one command per sentinel so a tool
// that isn't found (silent, no line printed) never shifts what a later
// sentinel's line means.
func probeScript() string {
	var b strings.Builder
	line := func(sentinel, cmd string) {
		b.WriteString("echo ")
		b.WriteString(sentinel)
		b.WriteString("\n")
		b.WriteString(cmd)
		b.WriteString("\n")
	}
	line(sentinelOS, "uname -s 2>/dev/null")
	line(sentinelArch, "uname -m 2>/dev/null")
	line(sentinelHome, `printf '%s\n' "$HOME"`)
	line(sentinelShell, `printf '%s\n' "$SHELL"`)
	line(sentinelLoginNode, `if [ -n "$SHELL" ]; then "$SHELL" -lic 'command -v node' 2>/dev/null; fi`)
	line(sentinelLoginClaude, `if [ -n "$SHELL" ]; then "$SHELL" -lic 'command -v claude' 2>/dev/null; fi`)
	line(sentinelPlainNode, "command -v node 2>/dev/null")
	line(sentinelPlainClaude, "command -v claude 2>/dev/null")
	b.WriteString("echo ")
	b.WriteString(sentinelEnd)
	return b.String()
}

// parseProbeSections splits probeScript's output into the text following
// each sentinel line, up to (not including) the next sentinel. Robust to
// extra noise before the first sentinel (a login MOTD) and to a section
// producing zero, one, or multiple lines.
func parseProbeSections(output string) map[string]string {
	sentinels := []string{
		sentinelOS, sentinelArch, sentinelHome, sentinelShell,
		sentinelLoginNode, sentinelLoginClaude, sentinelPlainNode, sentinelPlainClaude,
		sentinelEnd,
	}
	sections := make(map[string]string, len(sentinels))
	lines := strings.Split(output, "\n")
	isSentinel := func(l string) (string, bool) {
		l = strings.TrimSpace(l)
		for _, s := range sentinels {
			if l == s {
				return s, true
			}
		}
		return "", false
	}
	current := ""
	var buf []string
	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	for _, l := range lines {
		if s, ok := isSentinel(l); ok {
			flush()
			current = s
			continue
		}
		if current != "" {
			buf = append(buf, l)
		}
	}
	flush()
	return sections
}

// firstLine returns s's first non-empty line, trimmed — command -v may print
// a trailing newline or, over some shells, extra blank lines.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			return l
		}
	}
	return ""
}

// Probe discovers h's OS/arch/home/shell and the absolute paths to node and
// claude (decision 9), then the version each reports, capped at 30s total.
// The login-shell discovery is preferred; the plain-PATH fallback fills in
// whichever of node/claude it didn't find. A transport failure (host
// unreachable, auth refused) returns ok:false with Error set, never a Go
// error — Probe's contract is "always a HostProbe to store", matching how a
// probe result is persisted on the host record regardless of outcome.
func Probe(ctx context.Context, h config.Host) (config.HostProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	result := config.HostProbe{At: time.Now().UTC().Format(time.RFC3339)}

	controlDir, err := ControlDir()
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-T", "--", launcherFor(probeScript()))

	stdout, stderr, runErr := runner(ctx, argv[0], argv[1:])
	if runErr != nil {
		result.Error = sshFailureMessage(runErr, stderr)
		return result, nil
	}

	sections := parseProbeSections(string(stdout))
	result.OS = firstLine(sections[sentinelOS])
	result.Arch = firstLine(sections[sentinelArch])
	result.Home = firstLine(sections[sentinelHome])
	result.Shell = firstLine(sections[sentinelShell])

	nodePath := firstLine(sections[sentinelLoginNode])
	if nodePath == "" {
		nodePath = firstLine(sections[sentinelPlainNode])
	}
	claudePath := firstLine(sections[sentinelLoginClaude])
	if claudePath == "" {
		claudePath = firstLine(sections[sentinelPlainClaude])
	}
	result.NodePath = nodePath
	result.ClaudePath = claudePath

	if nodePath != "" {
		if v, err := runVersion(ctx, h, controlDir, nodePath); err == nil {
			result.NodeVersion = v
		}
	}
	if claudePath != "" {
		if v, err := runVersion(ctx, h, controlDir, claudePath); err == nil {
			result.ClaudeVersion = v
		}
	}

	result.OK = true
	return result, nil
}

// runVersion execs "<path> --version" over ssh and returns its first line —
// the ordinary shape of both `node --version` (e.g. "v24.7.0") and Claude
// Code's own `claude --version` banner.
func runVersion(ctx context.Context, h config.Host, controlDir, path string) (string, error) {
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-T", "--", RemoteCommand("", []string{path, "--version"}, nil))
	stdout, _, err := runner(ctx, argv[0], argv[1:])
	if err != nil {
		return "", err
	}
	return firstLine(string(stdout)), nil
}

// sshFailureMessage prefers ssh's own stderr (it names the real cause —
// "Permission denied", "Connection timed out", "Host key verification
// failed") over Go's generic exit-status error.
func sshFailureMessage(runErr error, stderr []byte) string {
	if msg := firstLine(string(stderr)); msg != "" {
		return msg
	}
	return runErr.Error()
}

// Check reports whether a live ControlMaster answers for h — the liveness
// signal hostView's "connected" status is built from. false on any error
// (no master, host down, ssh missing): the caller only needs yes/no.
func Check(h config.Host) bool {
	controlDir, err := ControlDir()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-O", "check")
	_, _, err = runner(ctx, argv[0], argv[1:])
	return err == nil
}

// Disconnect tears down h's ControlMaster with `ssh -O exit`. A missing
// master is not an error from ssh's own perspective in most cases, but
// Disconnect does not special-case it either way — the caller wants "no
// master after this returns" and doesn't need to know whether one existed.
func Disconnect(h config.Host) error {
	controlDir, err := ControlDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-O", "exit")
	_, stderr, err := runner(ctx, argv[0], argv[1:])
	if err != nil {
		return fmt.Errorf("ssh -O exit: %s", sshFailureMessage(err, stderr))
	}
	return nil
}
