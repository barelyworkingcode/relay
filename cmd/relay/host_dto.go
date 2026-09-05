package main

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// hostView is the record plus two derived, read-only fields (docs/ssh-hosts.md):
// status (computed from the last probe and whether a ControlMaster is
// currently live) and ssh_argv (decision 2's ready-to-exec prefix). Unlike
// projectView there is no secret to strip -- a host record carries no
// token, ssh authenticates with the operator's own identity.
type hostView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Target       string            `json:"target"`
	Port         int               `json:"port,omitempty"`
	IdentityFile string            `json:"identity_file,omitempty"`
	CreatedAt    string            `json:"created_at"`
	Probe        *config.HostProbe `json:"probe,omitempty"`
	Status       string            `json:"status"`
	SSHArgv      []string          `json:"ssh_argv"`
}

// hostStatus classifies a host for the tray and eve's host chip
// (docs/ssh-hosts.md): connected (a ControlMaster answers right now), idle
// (last probe ok, no live master), unreachable (last probe failed), unknown
// (never probed). Check dials the host, so this is not free -- callers that
// list many hosts pay one ssh -O check per host, which is the same "tens of
// milliseconds over an existing connection" cost decision 3 already accepts
// elsewhere.
func hostStatus(h config.Host) string {
	if h.Probe == nil {
		return "unknown"
	}
	if !h.Probe.OK {
		return "unreachable"
	}
	if sshhost.Check(h) {
		return "connected"
	}
	return "idle"
}

func hostToView(h config.Host) hostView {
	controlDir, _ := sshhost.ControlDir()
	return hostView{
		ID:           h.ID,
		Name:         h.Name,
		Target:       h.Target,
		Port:         h.Port,
		IdentityFile: h.IdentityFile,
		CreatedAt:    h.CreatedAt,
		Probe:        h.Probe,
		Status:       hostStatus(h),
		SSHArgv:      sshhost.SSHArgv(h, controlDir),
	}
}

func hostsToView(hs []config.Host) []hostView {
	out := make([]hostView, 0, len(hs))
	for _, h := range hs {
		out = append(out, hostToView(h))
	}
	return out
}
