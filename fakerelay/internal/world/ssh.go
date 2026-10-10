package world

import "strconv"

// SSHArgv is the ready-to-exec ssh prefix relay derives for a host
// (docs/ssh-hosts.md, "ssh_argv"). controlDir holds the ControlPath socket.
func (h Host) SSHArgv(controlDir string) []string {
	argv := []string{"ssh",
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
	return append(argv, h.Target)
}
