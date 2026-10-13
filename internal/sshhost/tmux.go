package sshhost

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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

// ErrTmuxViewUnsupported refuses a tmux too old for attach -f (before 3.2).
var ErrTmuxViewUnsupported = errors.New("tmux read-only viewing is unsupported")
var ErrTmuxViewNotFound = errors.New("tmux session not found")

// TmuxViewTarget is server-derived; names are checked before an ID is used.
type TmuxViewTarget struct {
	ID        string
	Directory string
	Cols      uint16
	Rows      uint16
}

// ConfirmTmuxView checks the actual binary and server immediately before launch.
// A nil host selects the console; there is deliberately no psmux fallback.
func ConfirmTmuxView(ctx context.Context, host *config.Host, tmuxPath, name string) (TmuxViewTarget, error) {
	run := func(args ...string) ([]byte, []byte, error) {
		if host != nil {
			return runTmux(ctx, *host, tmuxPath, args...)
		}
		bounded, cancel := context.WithTimeout(ctx, tmuxTimeout)
		defer cancel()
		return runner(bounded, tmuxPath, args)
	}
	version, _, err := run("-V")
	if err != nil {
		return TmuxViewTarget{}, err
	}
	v := strings.Fields(string(version))
	if len(v) != 2 || v[0] != "tmux" {
		return TmuxViewTarget{}, ErrTmuxViewUnsupported
	}
	parts := strings.SplitN(v[1], ".", 2)
	if len(parts) != 2 {
		return TmuxViewTarget{}, ErrTmuxViewUnsupported
	}
	major, e := strconv.Atoi(parts[0])
	minorText := strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	minor, e2 := strconv.Atoi(minorText)
	if e != nil || e2 != nil || major < 3 || (major == 3 && minor < 2) {
		return TmuxViewTarget{}, ErrTmuxViewUnsupported
	}
	out, stderr, err := run("list-sessions", "-F", "#{session_id}|#{session_name}|#{pane_current_path}")
	if err != nil {
		if isNoTmuxServer(stderr) {
			return TmuxViewTarget{}, ErrTmuxViewNotFound
		}
		return TmuxViewTarget{}, err
	}
	found := TmuxViewTarget{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSuffix(line, "\r"), "|", 3)
		if len(fields) != 3 || fields[1] != name {
			continue
		}
		if found.ID != "" || !validTmuxSessionID(fields[0]) || !filepath.IsAbs(fields[2]) || strings.ContainsAny(fields[2], "\x00\r\n") {
			return TmuxViewTarget{}, ErrTmuxViewUnsupported
		}
		found = TmuxViewTarget{ID: fields[0], Directory: fields[2]}
	}
	if found.ID == "" {
		return TmuxViewTarget{}, ErrTmuxViewNotFound
	}
	geometry, _, err := run("display-message", "-p", "-t", found.ID, TmuxGeometryFormat)
	if err != nil {
		return TmuxViewTarget{}, ErrTmuxViewUnsupported
	}
	if found.Cols, found.Rows, err = ParseTmuxGeometry(string(geometry)); err != nil {
		return TmuxViewTarget{}, err
	}
	return found, nil
}

// TmuxGeometryFormat reads a session's current window size and status lines.
const TmuxGeometryFormat = "#{window_width}|#{window_height}|#{status}"

// ParseTmuxGeometry turns one TmuxGeometryFormat line into the terminal size a
// client needs to show the whole window: its width, and its height plus the
// status lines.
func ParseTmuxGeometry(line string) (cols, rows uint16, err error) {
	fields := strings.Split(strings.TrimSpace(line), "|")
	if len(fields) != 3 {
		return 0, 0, ErrTmuxViewUnsupported
	}
	w, e1 := strconv.Atoi(fields[0])
	h, e2 := strconv.Atoi(fields[1])
	status := fields[2]
	switch status {
	case "off":
		status = "0"
	case "on":
		status = "1"
	}
	lines, e3 := strconv.Atoi(status)
	if e1 != nil || e2 != nil || e3 != nil || w < 1 || w > 65535 || h < 1 || lines < 0 || lines > 5 || h > 65535-lines {
		return 0, 0, ErrTmuxViewUnsupported
	}
	return uint16(w), uint16(h + lines), nil
}

func validTmuxSessionID(id string) bool {
	if len(id) < 2 || id[0] != '$' {
		return false
	}
	for _, r := range id[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
