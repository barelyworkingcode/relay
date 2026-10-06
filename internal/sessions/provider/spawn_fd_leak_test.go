package provider

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const fdLeakStarts = 5

// fdIdentity names the file behind a descriptor. The kernel reuses the lowest
// free number, so a number alone cannot tell a leaked descriptor from an
// unrelated one that closed and was reopened.
type fdIdentity struct{ dev, ino uint64 }

// openFDs maps each open descriptor to its file's identity.
func openFDs(t *testing.T) map[int]fdIdentity {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	open := make(map[int]fdIdentity, len(entries))
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		// Subtle: the listing includes the descriptor ReadDir itself used,
		// which is already closed by now. Its fstat fails with EBADF and the
		// entry is skipped; it must not count as an open descriptor.
		if err := syscall.Fstat(n, &st); err != nil {
			continue
		}
		open[n] = fdIdentity{dev: uint64(st.Dev), ino: st.Ino}
	}
	return open
}

// newFDs returns, sorted, the descriptors in after that are absent from
// before or now name a different file.
func newFDs(before, after map[int]fdIdentity) []int {
	var fresh []int
	for n, id := range after {
		if prev, ok := before[n]; !ok || prev != id {
			fresh = append(fresh, n)
		}
	}
	sort.Ints(fresh)
	return fresh
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

	before := openFDs(t)
	for i := 0; i < fdLeakStarts; i++ {
		if err := start(); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("start %d: Start error = %v, want the missing shim binary's ErrNotExist", i+1, err)
		}
	}
	if leaked := newFDs(before, openFDs(t)); len(leaked) > 0 {
		t.Fatalf("fds %v opened by %d failed starts and still open", leaked, fdLeakStarts)
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

func TestFDLeakCheck_UnrelatedCloseIsNotALeak(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	first := true
	assertFailedStartsLeakNoFDs(t, func() error {
		if first {
			first = false
			_ = r.Close()
			_ = w.Close()
		}
		return fs.ErrNotExist
	})
}

func TestFDLeakCheck_ReusedNumberCountsAsNew(t *testing.T) {
	aR, aW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe A: %v", err)
	}
	defer aW.Close()
	// aR owns descriptor n after the Dup2 below; keep it reachable so a
	// finalizer cannot close that number mid-test.
	defer aR.Close()
	n := int(aR.Fd())
	before := openFDs(t)

	bR, bW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe B: %v", err)
	}
	defer bW.Close()
	if err := syscall.Dup2(int(bR.Fd()), n); err != nil {
		t.Fatalf("dup2 onto %d: %v", n, err)
	}
	_ = bR.Close()

	got := newFDs(before, openFDs(t))
	for _, fd := range got {
		if fd == n {
			return
		}
	}
	t.Fatalf("newFDs = %v, want it to include reused descriptor %d", got, n)
}
