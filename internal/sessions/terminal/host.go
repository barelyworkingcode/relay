package terminal

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/sshhost"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// buildHostTargetArgv assembles the argv the shim spawns as its target for
// an SSH-backed terminal: sshArgv + "-tt" (allocate a remote tty; "-T" would
// leave the interactive shell with none) + "--" + a single RemoteCommand
// line. Unlike relayLLM's old per-template buildHostTerminalExec, there is
// no branching on template id here: C5 already hands this package a
// concrete, resolved argv (relay resolved it from the template before
// calling /launch), so wrapping it for ssh is the one RemoteCommand call —
// no "$SHELL" indirection, no claude-path special case.
func buildHostTargetArgv(host *sessionstypes.HostSpec, dir string, argv []string, env map[string]string) (name string, args []string, err error) {
	if len(host.SSHArgv) == 0 {
		return "", nil, fmt.Errorf("host %q has no ssh_argv", host.Name)
	}
	remote := sshhost.RemoteCommand(dir, argv, env)
	name = host.SSHArgv[0]
	args = append(append([]string{}, host.SSHArgv[1:]...), "-tt", "--", remote)
	return name, args, nil
}
