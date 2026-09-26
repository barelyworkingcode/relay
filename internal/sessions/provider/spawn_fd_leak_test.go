package provider

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const fdLeakStarts = 5

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	return len(entries)
}

func assertFailedStartsLeakNoFDs(t *testing.T, start func() error) {
	t.Helper()
	// Deliberate: the first pipe registers with the runtime poller, which
	// opens its own fd once; open one here so the baseline already has it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("warm-up pipe: %v", err)
	}
	_ = r.Close()
	_ = w.Close()

	before := openFDCount(t)
	for i := 0; i < fdLeakStarts; i++ {
		if err := start(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("start %d: Start error = %v, want the missing shim binary's ErrNotExist", i+1, err)
		}
	}
	after := openFDCount(t)
	if after != before {
		t.Fatalf("open fds: %d before, %d after %d failed starts; leaked %d",
			before, after, fdLeakStarts, after-before)
	}
}

func missingShimBinary(t *testing.T) string {
	return filepath.Join(t.TempDir(), "no-such-relay-sessions")
}

func TestClaudeStart_ShimStartFails_LeaksNoFDs(t *testing.T) {
	session := &sessionstypes.Session{ID: "claude-fdleak-1", Model: "sonnet", Directory: t.TempDir()}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		Binary:       "/bin/echo",
		ShimBinary:   missingShimBinary(t),
		BridgeSocket: filepath.Join(t.TempDir(), "bridge.sock"),
		Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("a", 64)},
	}, nil)
	defer p.Kill()

	assertFailedStartsLeakNoFDs(t, p.Start)
}

func TestPiStart_ShimStartFails_LeaksNoFDs(t *testing.T) {
	session := &sessionstypes.Session{ID: "pi-fdleak-1", Model: "claude-sonnet-4", Directory: t.TempDir()}
	p := NewPiProvider(session, func(string, json.RawMessage) {}, PiConfig{
		Binary:       "/bin/echo",
		DataDir:      t.TempDir(),
		ShimBinary:   missingShimBinary(t),
		BridgeSocket: filepath.Join(t.TempDir(), "bridge.sock"),
		Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("b", 64)},
	})
	defer p.Kill()

	assertFailedStartsLeakNoFDs(t, p.Start)
}
