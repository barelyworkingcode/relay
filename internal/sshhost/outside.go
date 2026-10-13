package sshhost

import (
	"bytes"
	"context"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"io"
	"os/exec"
	"time"
)

const maxOutsideOutput = 8 << 20

// outsideBuffer refuses a write that would pass the bound, so the SSH child is
// stopped while it is still producing rather than after it has been collected.
type outsideBuffer struct{ bytes.Buffer }

func (b *outsideBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxOutsideOutput {
		return 0, fmt.Errorf("outside host observation exceeded bound")
	}
	return b.Buffer.Write(p)
}

// RunOutside executes a fixed observation script through the existing supervised
// SSH path. The caller never runs it against a Windows host.
func RunOutside(ctx context.Context, h config.Host, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dir, err := ControlDir()
	if err != nil {
		return nil, err
	}
	argv := append(SSHArgv(h, dir), "-T", "--", RemoteCommand("", []string{"sh", "-c", script}, nil))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out outsideBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("outside host observation failed: %w", err)
	}
	return out.Bytes(), nil
}
