// Package cmdlookup resolves a program name to an absolute path on this Mac or
// on a host, the way an interactive login shell on that machine would.
package cmdlookup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

var (
	// ErrNotFound means the machine answered and has no executable by that name.
	ErrNotFound = errors.New("command not found")
	// ErrUnreachable means the machine could not be asked; it is wrapped with
	// the ssh text.
	ErrUnreachable = errors.New("machine unreachable")
)

const lookupTimeout = 15 * time.Second

const (
	sentinelLogin = "@@RELAY_LOOKUP_LOGIN@@"
	sentinelPlain = "@@RELAY_LOOKUP_PLAIN@@"
	sentinelEnd   = "@@RELAY_LOOKUP_END@@"
)

// quote single-quotes s for POSIX sh.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// script prints the login-shell answer and the plain-PATH answer, each after
// its own sentinel, so a lookup that prints nothing never shifts the other.
func script(name string) string {
	var b strings.Builder
	b.WriteString("echo " + sentinelLogin + "\n")
	b.WriteString(sshhost.LoginShellLookup(name) + "\n")
	b.WriteString("echo " + sentinelPlain + "\n")
	b.WriteString("command -v " + quote(name) + " 2>/dev/null\n")
	b.WriteString("echo " + sentinelEnd)
	return b.String()
}

// parse takes the first line that starts with "/" in the login section, else
// in the plain section. Other lines are skipped: a startup file can print a
// banner before the lookup answer, and command -v prints builtin, alias and
// function names without a path.
func parse(output string) (string, error) {
	sections := map[string]string{}
	current := ""
	for _, l := range strings.Split(output, "\n") {
		l = strings.TrimSpace(l)
		switch l {
		case sentinelLogin, sentinelPlain, sentinelEnd:
			current = l
			continue
		}
		if current == "" || !strings.HasPrefix(l, "/") {
			continue
		}
		if _, seen := sections[current]; !seen {
			sections[current] = l
		}
	}
	line := sections[sentinelLogin]
	if line == "" {
		line = sections[sentinelPlain]
	}
	if line == "" {
		return "", ErrNotFound
	}
	return line, nil
}

// Local resolves name with relay's own environment.
func Local(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	// The outer shell is always sh: the script is POSIX, and $SHELL may be
	// fish. The login lookup inside the script still reads $SHELL.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script(name))
	cmd.Env = os.Environ()
	if os.Getenv("SHELL") == "" {
		cmd.Env = append(cmd.Env, "SHELL=/bin/sh")
	}
	cmd.Stdin = nil // exec reads /dev/null
	// The login shell is a grandchild that can hold the stdout pipe after sh
	// is killed; without a bound, Run would wait past the timeout.
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("looking up %s: %w", name, err)
	}
	return parse(out.String())
}

// OnHost resolves name on h over ssh.
func OnHost(ctx context.Context, h config.Host, name string) (string, error) {
	if name == "" {
		return "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	controlDir, err := sshhost.ControlDir()
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreachable, err.Error())
	}
	hostOS := ""
	if h.Probe != nil {
		hostOS = h.Probe.OS
	}
	argv := sshhost.SSHArgv(h, controlDir)
	argv = append(argv, "-T", "--", sshhost.RemoteCommandForOS(hostOS, "", []string{"sh", "-c", script(name)}, nil))

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	// ssh's ControlPersist master can hold inherited pipes after the client
	// exits; without a bound, Run would wait past the timeout.
	cmd.WaitDelay = 2 * time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreachable, failureText(err, errOut.Bytes()))
	}
	return parse(out.String())
}

// failureText prefers ssh's own stderr over Go's generic exit-status error.
func failureText(runErr error, stderr []byte) string {
	for _, l := range strings.Split(string(stderr), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return runErr.Error()
}
