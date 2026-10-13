package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"unicode"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
	"github.com/google/uuid"
)

type tmuxViewRequest struct {
	HostID      string `json:"hostId,omitempty"`
	SessionName string `json:"sessionName"`
	Cols        int    `json:"cols,omitempty"`
	Rows        int    `json:"rows,omitempty"`
}

type tmuxViewError struct {
	status        int
	code, message string
}

func (e *tmuxViewError) Error() string                 { return e.message }
func viewError(status int, code, message string) error { return &tmuxViewError{status, code, message} }

// TmuxViewOps launches only an attach client. Inventory confers no authority.
type TmuxViewOps struct{ deps sessionRouteDeps }

func (o *TmuxViewOps) Create(ctx context.Context, req tmuxViewRequest) (json.RawMessage, error) {
	if req.SessionName == "" || strings.ContainsAny(req.SessionName, "|:") || strings.IndexFunc(req.SessionName, unicode.IsControl) >= 0 {
		return nil, viewError(http.StatusBadRequest, "invalid_request", "an unambiguous session name is required")
	}
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols < 1 || req.Rows < 1 || req.Cols > 65535 || req.Rows > 65535 {
		return nil, viewError(http.StatusBadRequest, "invalid_request", "terminal dimensions must be between 1 and 65535")
	}
	if !o.deps.ready() {
		return nil, viewError(http.StatusBadGateway, "session_host_unavailable", "session host is unavailable")
	}
	var h *config.Host
	var hostJSON json.RawMessage
	tmuxPath := ""
	if req.HostID != "" {
		settings := config.FreshSettings(o.deps.store)
		h, _ = config.FindHostByID(settings, req.HostID)
		if h == nil || !h.ShowAllTmuxSessions {
			return nil, viewError(http.StatusForbidden, "outside_view_disabled", "outside viewing is disabled for this host")
		}
		tmuxPath = h.EffectiveTmuxPath()
		controlDir, err := sshhost.ControlDir()
		if err != nil {
			return nil, viewError(http.StatusBadGateway, "host_unreachable", "host connection unavailable")
		}
		host := sessiontypes.HostSpec{ID: h.ID, Name: h.Name, SSHArgv: sshhost.SSHArgv(*h, controlDir)}
		if h.Probe != nil {
			host.OS = h.Probe.OS
			host.Shell = h.Probe.Shell
		}
		hostJSON, _ = json.Marshal(host)
	} else {
		tmuxPath, _ = exec.LookPath("tmux")
	}
	if tmuxPath == "" {
		return nil, viewError(http.StatusConflict, "tmux_view_unsupported", "tmux read-only viewing is unavailable")
	}
	target, err := sshhost.ConfirmTmuxView(ctx, h, tmuxPath, req.SessionName)
	if err != nil {
		switch {
		case errors.Is(err, sshhost.ErrTmuxViewNotFound):
			return nil, viewError(http.StatusNotFound, "tmux_session_not_found", "the exact tmux session was not found")
		case errors.Is(err, sshhost.ErrTmuxViewUnsupported):
			return nil, viewError(http.StatusConflict, "tmux_view_unsupported", "tmux 3.2 or later with read-only client flags is required")
		default:
			code := "tmux_view_unsupported"
			status := http.StatusConflict
			if h != nil {
				code = "host_unreachable"
				status = http.StatusBadGateway
			}
			return nil, viewError(status, code, "tmux server could not be reached")
		}
	}
	spec := hostapi.LaunchRequest{V: 1, SessionID: uuid.NewString(), Kind: "pty", Name: req.SessionName, Directory: target.Directory,
		ViewTarget: &hostapi.ViewTarget{TmuxPath: tmuxPath, SessionID: target.ID}, Host: hostJSON,
		Env: map[string]string{"TERM": "xterm-256color", "TMUX": ""}, PTY: &hostapi.PTYSpec{Cols: int(target.Cols), Rows: int(target.Rows)}, ReadOnly: true, Outside: true}
	resp, refusal, err := o.deps.host().Launch(ctx, spec)
	if err != nil || refusal != nil {
		return nil, viewError(http.StatusBadGateway, "session_host_unavailable", "session host could not start the viewer")
	}
	return resp.Body, nil
}
