package sshhost

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// tmuxTimeout bounds one tmux round trip over ssh. A tmux command answers in
// milliseconds once connected; the rest is ConnectTimeout's 10s plus margin,
// so an unreachable host fails a list or kill instead of hanging the caller.
const tmuxTimeout = 15 * time.Second

// tmuxListFormat is `tmux ls -F`'s per-session line. | cannot appear in a
// name relay creates, and the two numeric fields never contain it.
const tmuxListFormat = "#{session_name}|#{session_created}|#{session_attached}"

// TmuxSession is one line of a host's `tmux ls`: the session name, its
// creation time in unix seconds, and how many clients are attached to it
// from anywhere.
type TmuxSession struct {
	Name     string
	Created  int64
	Attached int
}

// ListTmuxSessions runs `<tmuxPath> ls` on h over ssh. A host with no tmux
// server, or a server with no sessions, is an empty list: tmux exits
// non-zero there, and that is the ordinary state of a host nobody has
// persisted anything on yet.
func ListTmuxSessions(ctx context.Context, h config.Host, tmuxPath string) ([]TmuxSession, error) {
	stdout, stderr, err := runTmux(ctx, h, tmuxPath, "ls", "-F", tmuxListFormat)
	if err != nil {
		if isNoTmuxServer(stderr) {
			return []TmuxSession{}, nil
		}
		return nil, fmt.Errorf("tmux ls on %s: %s", h.Name, sshFailureMessage(err, stderr))
	}
	return parseTmuxList(string(stdout)), nil
}

// KillTmuxSession runs `<tmuxPath> kill-session -t <name>` on h over ssh.
func KillTmuxSession(ctx context.Context, h config.Host, tmuxPath, name string) error {
	_, stderr, err := runTmux(ctx, h, tmuxPath, "kill-session", "-t", name)
	if err != nil {
		return fmt.Errorf("tmux kill-session %s on %s: %s", name, h.Name, sshFailureMessage(err, stderr))
	}
	return nil
}

func runTmux(ctx context.Context, h config.Host, tmuxPath string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	defer cancel()
	controlDir, err := ControlDir()
	if err != nil {
		return nil, nil, err
	}
	hostOS := ""
	if h.Probe != nil {
		hostOS = h.Probe.OS
	}
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-T", "--", RemoteCommandForOS(hostOS, "", append([]string{tmuxPath}, args...), nil))
	return runner(ctx, argv[0], argv[1:])
}

// isNoTmuxServer recognises the stderr tmux (and psmux) print when there is
// nothing to list: "no server running on /tmp/tmux-501/default", "error
// connecting to … (No such file or directory)", "no sessions". Matching is
// on lowercased substrings so a server's socket path or a port's own wording
// around these phrases does not matter.
func isNoTmuxServer(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	for _, phrase := range []string{"no server running", "error connecting to", "no sessions"} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

// parseTmuxList reads tmuxListFormat lines. A line without both separators
// (a login banner, a warning) is skipped; an unparseable number reads as 0
// rather than dropping a session the operator can still see and kill.
func parseTmuxList(output string) []TmuxSession {
	out := []TmuxSession{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		name, rest, ok := strings.Cut(line, "|")
		if !ok || name == "" {
			continue
		}
		createdText, attachedText, ok := strings.Cut(rest, "|")
		if !ok {
			continue
		}
		created, _ := strconv.ParseInt(strings.TrimSpace(createdText), 10, 64)
		attached, _ := strconv.Atoi(strings.TrimSpace(attachedText))
		out = append(out, TmuxSession{Name: name, Created: created, Attached: attached})
	}
	return out
}
