package projectfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// A watch registered before a Restart keeps delivering events from the
// replacement agent, and its stop func still works afterwards.
func TestHostRestartKeepsWatches(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not on PATH")
	}
	pool := NewHostPool(HostPoolOptions{
		Argv: func(_ config.Host, launcher string) ([]string, error) {
			return []string{"sh", "-c", launcher}, nil
		},
	})
	host := config.Host{ID: "h1", Name: "box", Target: "box.invalid", Probe: &config.HostProbe{NodePath: node}}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	events := make(chan Event, 16)
	stop, err := pool.Backend(host, root).Watch(ctx, func(ev Event) {
		select {
		case events <- ev:
		default:
		}
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	moved := host
	moved.Target = "other.invalid"
	pool.Restart(moved)

	if err := pool.Backend(moved, root).Write(ctx, "a.txt", []byte("x"), WriteOpts{}); err != nil {
		t.Fatalf("write after restart: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("no fs_event after Restart")
	}

	stop()
	pool.mu.Lock()
	a := pool.agents[host.ID]
	pool.mu.Unlock()
	a.watches.mu.Lock()
	left := len(a.watches.roots)
	a.watches.mu.Unlock()
	if left != 0 {
		t.Fatalf("stop left %d held roots", left)
	}
	pool.Drop(host.ID)
}

// stepClock is an injected clock that moves only when a test fires its
// timers. registered reports the duration of each After call, so a test waits
// for the timer it means to fire instead of guessing.
type stepClock struct {
	mu         sync.Mutex
	timers     []stepTimer
	registered chan time.Duration
}

type stepTimer struct {
	d  time.Duration
	ch chan time.Time
}

func newStepClock() *stepClock { return &stepClock{registered: make(chan time.Duration, 256)} }

func (c *stepClock) Now() time.Time                  { return time.Unix(1_800_000_000, 0) }
func (c *stepClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
func (c *stepClock) Sleep(d time.Duration)           { <-c.After(d) }
func (c *stepClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.timers = append(c.timers, stepTimer{d, ch})
	c.mu.Unlock()
	select {
	case c.registered <- d:
	default:
	}
	return ch
}

// fireUpTo fires every pending timer of at most max. The 10 s hello bound and
// the 30 s request bound stay pending when max is below them.
func (c *stepClock) fireUpTo(max time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := c.timers[:0]
	for _, tm := range c.timers {
		if tm.d <= max {
			tm.ch <- c.Now()
		} else {
			keep = append(keep, tm)
		}
	}
	c.timers = keep
}

// awaitTimerUpTo returns once an After of at most max has been registered.
func (c *stepClock) awaitTimerUpTo(ctx context.Context, t *testing.T, max time.Duration) {
	t.Helper()
	for {
		select {
		case d := <-c.registered:
			if d <= max {
				return
			}
		case <-ctx.Done():
			t.Fatal("no reconnect timer was registered")
		}
	}
}

func hostStatusFeed(pool *HostPool) (<-chan HostStatus, func()) {
	ch := make(chan HostStatus, 64)
	cancel := pool.Subscribe(func(s HostStatus) {
		select {
		case ch <- s:
		default:
		}
	})
	return ch, cancel
}

func nextStatus(ctx context.Context, t *testing.T, ch <-chan HostStatus) HostStatus {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-ctx.Done():
		t.Fatal("no host_status event")
		return HostStatus{}
	}
}

func nodeHost(t *testing.T) config.Host {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not on PATH")
	}
	return config.Host{ID: "h1", Name: "testbox", Target: "testbox.invalid", Probe: &config.HostProbe{NodePath: node}}
}

func TestHostStatusSequenceOnFirstUse(t *testing.T) {
	host := nodeHost(t)
	pool := NewHostPool(HostPoolOptions{
		Clock: newStepClock(),
		Argv: func(_ config.Host, launcher string) ([]string, error) {
			return []string{"/bin/sh", "-c", launcher}, nil
		},
	})
	t.Cleanup(func() { pool.Drop(host.ID) })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	feed, unsub := hostStatusFeed(pool)
	defer unsub()

	pool.Backend(host, t.TempDir())
	first := nextStatus(ctx, t, feed)
	second := nextStatus(ctx, t, feed)
	if first.Status != StatusConnecting || second.Status != StatusConnected {
		t.Fatalf("statuses = %q then %q, want connecting then connected", first.Status, second.Status)
	}
	if first.HostID != "h1" || first.Name != "testbox" {
		t.Errorf("status identifies %+v", first)
	}
	if st := pool.Statuses(); len(st) != 1 || st[0].Status != StatusConnected {
		t.Errorf("Statuses() = %+v", st)
	}
}

func TestHostWithoutNodeIsUnreachable(t *testing.T) {
	pool := NewHostPool(HostPoolOptions{Clock: newStepClock()})
	host := config.Host{ID: "h2", Name: "testbox", Target: "testbox.invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	feed, unsub := hostStatusFeed(pool)
	defer unsub()

	b := pool.Backend(host, "/srv/acme")
	s := nextStatus(ctx, t, feed)
	want := `host "testbox" has no node: run a probe`
	if s.Status != StatusUnreachable || s.Error != want {
		t.Errorf("status = %+v, want unreachable with %q", s, want)
	}
	_, err := b.Stat(ctx, "")
	if CodeOf(err) != CodeHostUnreachable || err == nil || err.Error() != want {
		t.Errorf("Stat = %v, want HOST_UNREACHABLE %q", err, want)
	}
}

func TestHostReconnectRearmsWatches(t *testing.T) {
	host := nodeHost(t)
	clk := newStepClock()
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	pool := NewHostPool(HostPoolOptions{
		Clock: clk,
		// exec keeps the shell's pid, so the pid file names the node process.
		Argv: func(_ config.Host, launcher string) ([]string, error) {
			return []string{"/bin/sh", "-c", "echo $$ > '" + pidFile + "'; exec " + launcher}, nil
		},
	})
	t.Cleanup(func() { pool.Drop(host.ID) })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	feed, unsub := hostStatusFeed(pool)
	defer unsub()

	events := make(chan Event, 64)
	stop, err := pool.Backend(host, root).Watch(ctx, func(ev Event) {
		select {
		case events <- ev:
		default:
		}
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer stop()

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for s := nextStatus(ctx, t, feed); s.Status != StatusUnreachable; s = nextStatus(ctx, t, feed) {
	}

	clk.awaitTimerUpTo(ctx, t, 5*time.Second)
	clk.fireUpTo(5 * time.Second)
	for s := nextStatus(ctx, t, feed); s.Status != StatusConnected; s = nextStatus(ctx, t, feed) {
	}

	// The status said connected, so the watch must already be armed on the
	// new agent: this write has to arrive without the caller watching again.
	if err := os.WriteFile(filepath.Join(root, "after.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-events:
			if ev.Path == "after.txt" {
				return
			}
		case <-ctx.Done():
			t.Fatal("no event after reconnect: the watch was not re-armed")
		}
	}
}
