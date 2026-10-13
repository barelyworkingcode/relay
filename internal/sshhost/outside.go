package sshhost

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"time"
	"unicode/utf16"
)

// RunOutside executes a fixed observation script through the existing supervised
// SSH path. Script arguments are quoted by the same OS-specific command builder.
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
	args := []string{"sh", "-c", script}
	if IsWindowsOS(hostOS) || hostOS == "Windows" {
		u := utf16.Encode([]rune(script))
		b := make([]byte, len(u)*2)
		for j, v := range u {
			binary.LittleEndian.PutUint16(b[j*2:], v)
		}
		args = []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(b)}
	}
	argv := append(SSHArgv(h, dir), "-T", "--", RemoteCommandForOS(hostOS, "", args, nil))
	stdout, _, err := runner(ctx, argv[0], argv[1:])
	if err != nil {
		return nil, fmt.Errorf("outside host observation failed: %w", err)
	}
	if len(stdout) > 8<<20 {
		return nil, fmt.Errorf("outside host observation exceeded bound")
	}
	return stdout, nil
}
