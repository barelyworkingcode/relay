package terminal

import (
	"fmt"

	"github.com/barelyworkingcode/relay/internal/sshhost"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// buildHostTargetArgv assembles the argv the shim spawns as its target for
// an SSH-backed terminal: sshArgv + "-tt" (allocate a remote tty; "-T" would
// leave the interactive shell with none) + "--" + a single RemoteCommand
// line. There is no branching on template id here: relay resolves the host
// template before calling /launch and hands this package either a concrete
// argv, wrapped verbatim by RemoteCommandForOS, or an empty one, meaning the
// template has no command and the host's own login shell runs. That shell
// is only knowable on the host, so it is expanded there ("$SHELL" -l), never
// resolved against the console.
func buildHostTargetArgv(host *sessionstypes.HostSpec, dir string, argv []string, env map[string]string) (name string, args []string, err error) {
	if len(host.SSHArgv) == 0 {
		return "", nil, fmt.Errorf("host %q has no ssh_argv", host.Name)
	}
	var remote string
	if len(argv) == 0 {
		remote = sshhost.LoginShellCommandForOS(host.OS, dir, env)
	} else {
		remote = sshhost.RemoteCommandForOS(host.OS, dir, argv, env)
	}
	name = host.SSHArgv[0]
	args = append(append([]string{}, host.SSHArgv[1:]...), "-tt", "--", remote)
	return name, args, nil
}
