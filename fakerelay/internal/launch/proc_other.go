//go:build !linux

package launch

import "os/exec"

func setProcAttr(*exec.Cmd) {}
