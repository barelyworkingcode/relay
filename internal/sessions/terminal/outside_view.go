package terminal

import (
	"context"
	"errors"
	"os/exec"
	"path"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// viewPollInterval is how often an outside viewer rereads its tmux window's
// size. tmux sends an ignore-size client no event when the owner resizes.
const viewPollInterval = 3 * time.Second

// viewQueryTimeout bounds one size read; over ssh it includes the connect.
const viewQueryTimeout = 15 * time.Second

// ViewTarget is the tmux session an outside viewer attaches to.
type ViewTarget struct {
	TmuxPath  string
	SessionID string // tmux's own $N, never a name: a name can be a prefix match
}

// attachArgv is the only command an outside viewer runs. read-only drops the
// client's keys; ignore-size keeps its size out of the window's.
func (v *ViewTarget) attachArgv() ([]string, error) {
	if !path.IsAbs(v.TmuxPath) || !validViewSessionID(v.SessionID) {
		return nil, errors.New("terminal: an outside viewer needs an absolute tmux path and a $N session id")
	}
	return []string{v.TmuxPath, "attach", "-f", "read-only,ignore-size", "-t", v.SessionID}, nil
}

func validViewSessionID(id string) bool {
	if len(id) < 2 || len(id) > 22 || id[0] != '$' {
		return false
	}
	for _, c := range id[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// followViewSize keeps relay's own PTY at the target window's size. It only
// reads from tmux: it sets no option, resizes no window and sends no
// refresh-client, so the owner's session is never changed.
func (s *Session) followViewSize(v ViewTarget, host *sessionstypes.HostSpec) {
	for {
		select {
		case <-s.waitDone:
			return
		case <-s.clock.After(viewPollInterval):
		}
		cols, rows, err := readViewSize(v, host)
		if err != nil {
			continue
		}
		if c, r := s.Size(); c != cols || r != rows {
			_ = s.setSize(cols, rows)
		}
	}
}

func readViewSize(v ViewTarget, host *sessionstypes.HostSpec) (cols, rows uint16, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), viewQueryTimeout)
	defer cancel()
	argv := []string{v.TmuxPath, "display-message", "-p", "-t", v.SessionID, sshhost.TmuxGeometryFormat}
	var cmd *exec.Cmd
	if host != nil {
		if len(host.SSHArgv) == 0 {
			return 0, 0, errors.New("terminal: host has no ssh argv")
		}
		args := append(append([]string{}, host.SSHArgv[1:]...), "-T", "--", sshhost.RemoteCommandForOS(host.OS, "", argv, nil))
		cmd = exec.CommandContext(ctx, host.SSHArgv[0], args...)
	} else {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(childBaseEnv(), "TMUX=")
	}
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	return sshhost.ParseTmuxGeometry(string(out))
}
