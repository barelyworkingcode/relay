package sshhost

import (
	"context"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"time"
)

// RunOutside executes a fixed observation script through the existing supervised
// SSH path. The caller never runs it against a Windows host.
func RunOutside(ctx context.Context, h config.Host, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dir, err := ControlDir()
	if err != nil {
		return nil, err
	}
	hostOS := ""
	if h.Probe != nil {
		hostOS = h.Probe.OS
	}
	argv := append(SSHArgv(h, dir), "-T", "--", RemoteCommandForOS(hostOS, "", []string{"sh", "-c", script}, nil))
	stdout, _, err := runner(ctx, argv[0], argv[1:])
	if err != nil {
		return nil, fmt.Errorf("outside host observation failed: %w", err)
	}
	if len(stdout) > 8<<20 {
		return nil, fmt.Errorf("outside host observation exceeded bound")
	}
	return stdout, nil
}
