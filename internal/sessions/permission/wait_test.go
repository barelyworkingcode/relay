package permission

import (
	"context"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

type waitResult struct {
	d  PermissionDecision
	ok bool
}

func startWait(ctx context.Context, m *PermissionManager) (PermissionRequest, <-chan waitResult) {
	req, ch := m.CreateRequest("s1", "mcp__relay__fs_list", `{}`, "tu-1")
	done := make(chan waitResult, 1)
	go func() {
		d, ok := m.WaitForDecisionContext(ctx, req.ID, ch)
		done <- waitResult{d, ok}
	}()
	return req, done
}

func awaitResult(t *testing.T, done <-chan waitResult) waitResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForDecisionContext never returned")
		return waitResult{}
	}
}

func TestWaitForDecisionContext(t *testing.T) {
	t.Run("user decision returned as given", func(t *testing.T) {
		m := NewPermissionManager()
		m.SetClock(testutil.NewFakeClock(time.Time{}))
		req, done := startWait(context.Background(), m)

		want := PermissionDecision{Decision: "deny", Reason: "not that folder"}
		if !m.Resolve(req.ID, want) {
			t.Fatal("Resolve found no pending request")
		}
		r := awaitResult(t, done)
		if !r.ok || r.d != want {
			t.Fatalf("got (%+v, %v), want (%+v, true)", r.d, r.ok, want)
		}
	})

	t.Run("60s without an answer denies and cleans up", func(t *testing.T) {
		m := NewPermissionManager()
		clock := testutil.NewFakeClock(time.Time{})
		m.SetClock(clock)
		req, done := startWait(context.Background(), m)

		testutil.WaitFor(t, 2*time.Second, func() bool { return clock.Waiters() == 1 })
		clock.Advance(59 * time.Second)
		select {
		case r := <-done:
			t.Fatalf("decided after 59s: (%+v, %v), want still waiting", r.d, r.ok)
		case <-time.After(50 * time.Millisecond):
		}
		if ids := m.PendingIDs(); len(ids) != 1 || ids[0] != req.ID {
			t.Fatalf("PendingIDs after 59s = %v, want [%s]", ids, req.ID)
		}
		clock.Advance(1 * time.Second)

		r := awaitResult(t, done)
		want := PermissionDecision{Decision: "deny", Reason: "No response"}
		if !r.ok || r.d != want {
			t.Fatalf("got (%+v, %v), want (%+v, true)", r.d, r.ok, want)
		}
		if n := m.PendingCount(); n != 0 {
			t.Fatalf("PendingCount = %d, want 0 after the timeout", n)
		}
	})

	t.Run("ended context returns not ok", func(t *testing.T) {
		m := NewPermissionManager()
		m.SetClock(testutil.NewFakeClock(time.Time{}))
		ctx, cancel := context.WithCancel(context.Background())
		_, done := startWait(ctx, m)

		cancel()
		if r := awaitResult(t, done); r.ok {
			t.Fatalf("got (%+v, true), want ok=false once the context ends", r.d)
		}
		if n := m.PendingCount(); n != 0 {
			t.Fatalf("PendingCount = %d, want 0 after the context ends", n)
		}
	})
}
