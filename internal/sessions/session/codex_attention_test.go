package session_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/attention"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

var (
	buildTestCodexOnce sync.Once
	testCodexBin       string
	buildTestCodexErr  error
)

// buildTestCodex builds cmd/testcodex once per test run into a short /tmp dir.
func buildTestCodex(t *testing.T) string {
	t.Helper()
	buildTestCodexOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "rh-testcodex-")
		if err != nil {
			buildTestCodexErr = err
			return
		}
		testCodexBin = filepath.Join(dir, "codex")
		cmd := exec.Command("go", "build", "-o", testCodexBin, "./cmd/testcodex")
		cmd.Dir = filepath.Join("..", "..", "..")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildTestCodexErr = fmt.Errorf("build testcodex: %w", err)
		}
	})
	if buildTestCodexErr != nil {
		t.Fatalf("build testcodex: %v", buildTestCodexErr)
	}
	return testCodexBin
}

// newCodexAttnHarness runs the real Codex provider against testcodex under a
// real Manager and a fake clock.
func newCodexAttnHarness(t *testing.T, fixture string, env map[string]string) (*attnHarness, string) {
	t.Helper()
	bin := buildTestCodex(t)
	fx, err := filepath.Abs(filepath.Join("..", "provider", "testdata", "codex", fixture))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTCODEX_FIXTURE", fx)
	for k, v := range env {
		t.Setenv(k, v)
	}
	h := &attnHarness{rec: &attnRec{}, clk: testutil.NewFakeClock(time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC))}
	h.mgr = session.NewManager(
		session.Config{Clock: h.clk, Codex: provider.CodexConfig{Binary: bin}},
		session.NewStore(t.TempDir()), nil)
	h.mgr.SetAttentionSink(h.rec)
	// The provider closes its wait channel before it delivers process_exited,
	// so StopAll can return while the manager's exit-time persist is still
	// about to write into the store dir. Wait for that event so TempDir
	// removal never races it.
	exited := make(chan struct{})
	var exitOnce sync.Once
	h.mgr.SetExitHandler(func(string, int) { exitOnce.Do(func() { close(exited) }) })
	t.Cleanup(func() {
		h.mgr.StopAll()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("provider exit event never arrived")
		}
	})

	const id = "55555555-0000-0000-0000-000000000001"
	spec := session.CreateSpec{
		SessionID: id, ProjectID: "11111111-0000-0000-0000-000000000001",
		Kind: session.KindCodex, Model: "codex/gpt-6-luna", Directory: t.TempDir(),
	}
	if _, err := h.mgr.Create(spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return h, id
}

func (h *attnHarness) waitSeq(t *testing.T, want string) {
	t.Helper()
	testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.seq() == want })
}

// Codex reports six of the seven states; asking is Claude-only.
func TestCodexAttention_StatesFollowTheTurn(t *testing.T) {
	t.Run("completed turn", func(t *testing.T) {
		h, id := newCodexAttnHarness(t, "turn_ok.jsonl", nil)
		h.send(t, id)
		h.waitSeq(t, "starting,idle,running,turn_done,idle")
	})
	t.Run("failed turn", func(t *testing.T) {
		h, id := newCodexAttnHarness(t, "turn_failed.jsonl", nil)
		h.send(t, id)
		testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.count("errored") == 1 })
		if got := h.rec.seq(); got[len(got)-len("errored"):] != "errored" {
			t.Errorf("sequence = %s, want it to end errored", got)
		}
	})
	t.Run("killed child mid-turn", func(t *testing.T) {
		h, id := newCodexAttnHarness(t, "turn_ok.jsonl", map[string]string{"TESTCODEX_DIE": "1"})
		h.send(t, id)
		testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.count("errored") == 1 })
		if row, ok := h.row(id); ok && row.Attention != nil && row.Attention.State != attention.Errored {
			t.Errorf("row state = %s, want errored", row.Attention.State)
		}
	})
	t.Run("hung turn stalls after the stall window", func(t *testing.T) {
		h, id := newCodexAttnHarness(t, "turn_ok.jsonl", map[string]string{"TESTCODEX_HANG": "1"})
		h.send(t, id)
		testutil.WaitFor(t, 2*time.Second, func() bool { return h.clk.Waiters() >= 1 })
		h.clk.Advance(attention.StallAfter)
		testutil.WaitFor(t, 2*time.Second, func() bool { return h.rec.count("stalled") == 1 })
	})
	t.Run("ended", func(t *testing.T) {
		h, id := newCodexAttnHarness(t, "turn_ok.jsonl", nil)
		h.mgr.EndSession(id)
		testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.count("ended") == 1 })
		if row, ok := h.row(id); ok && row.Attention != nil {
			t.Errorf("ended row carries attention %+v", row.Attention)
		}
	})
}
