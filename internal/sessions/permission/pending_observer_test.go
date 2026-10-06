package permission

import (
	"fmt"
	"testing"
)

func TestPendingObserver_ReportsRemainingCountPerSession(t *testing.T) {
	m := NewPermissionManager()
	var calls []string
	m.SetPendingObserver(func(sessionID string, pending int) {
		calls = append(calls, fmt.Sprintf("%s=%d", sessionID, pending))
	})
	step := func(want ...string) {
		t.Helper()
		if fmt.Sprint(calls) != fmt.Sprint(want) {
			t.Fatalf("observer calls = %v, want %v", calls, want)
		}
		calls = nil
	}

	r1, _ := m.CreateRequest("p1", "tool", `{}`, "u1")
	step("p1=1")
	r2, _ := m.CreateRequest("p1", "tool", `{}`, "u2")
	step("p1=2")
	_, _ = m.CreateRequest("p2", "tool", `{}`, "u3")
	step("p2=1")

	m.Resolve(r1.ID, PermissionDecision{Decision: "allow"})
	step("p1=1")
	m.Cleanup(r2.ID)
	step("p1=0")

	m.Resolve("no-such-id", PermissionDecision{Decision: "allow"})
	m.Cleanup("no-such-id")
	step()

	_, _ = m.CreateRequest("p1", "tool", `{}`, "u4")
	_, _ = m.CreateRequest("p1", "tool", `{}`, "u5")
	calls = nil
	m.DenyAllForSession("p1", "closed")
	step("p1=0")
	if n := m.PendingCount(); n != 1 {
		t.Fatalf("p2's request must survive: PendingCount = %d, want 1", n)
	}
}
