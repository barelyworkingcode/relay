package launch

import (
	"os/exec"
	"syscall"
)

// setProcAttr makes a killed fakerelay take its services with it.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
