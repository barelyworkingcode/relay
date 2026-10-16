package features

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// A message refused because a turn is running must leave the running
// message's reply intact.
func TestRefusedSecondMessageKeepsFirstReply(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-refused-second", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	// A local session runs sandboxed and reads only its project folder.
	fifo := filepath.Join(g1tDir(i, "acme-refused-second"), "hold")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("creating the fifo: %v", err)
	}
	// The open returns only when the agent reads the fifo: the turn is running.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			close(opened)
			return
		}
		opened <- f
	}()

	first := i.StartCLI(harness.CLIOpts{Deadline: 30 * time.Second},
		"session", "message", "--id", sid, "--text", "!sh read x < "+fifo, "--json")

	var w *os.File
	select {
	case f, ok := <-opened:
		if !ok {
			t.Fatal("opening the fifo failed")
		}
		w = f
	case <-time.After(30 * time.Second):
		t.Fatal("the agent never read the fifo, so the first turn did not start")
	}
	defer w.Close()

	if r := i.CLI("session", "message", "--id", sid, "--text", "ping", "--json"); r.Code == 0 || !strings.Contains(string(r.Stderr), "409") {
		t.Fatalf("a message during a running turn exited %d, want the 409 refusal: %s%s", r.Code, r.Stdout, r.Stderr)
	}

	w.Close()
	res := first.Wait()
	if res.Code != 0 {
		t.Fatalf("the first message exited %d after its turn ended, want 0: %s", res.Code, res.Stdout)
	}
	var reply struct {
		Text string `json:"text"`
	}
	res.JSON(t, &reply)
	if reply.Text == "" {
		t.Fatalf("the first message printed no reply text: %s", res.Stdout)
	}
}
