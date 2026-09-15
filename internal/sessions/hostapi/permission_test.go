package hostapi_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// startServerForPermission is startServer, minus the /launch-specific
// RelayPID plumbing this file's tests don't exercise.
func startServerForPermission(t *testing.T) (*hostapi.Server, string) {
	t.Helper()
	srv, _, hookSock, _ := startServer(t, os.Getpid())
	return srv, hookSock
}

func permissionBody(sessionID string) map[string]any {
	return map[string]any{
		"sessionId": sessionID,
		"toolName":  "Bash",
		"toolInput": `{"command":"ls"}`,
		"toolUseId": "tu1",
	}
}

// TestPermission_Member_Accepted covers C6's hook admission rule using the
// real internal/membership.Resolve walk (not a fake stand-in). The peer must
// be a genuine, separate process — hostapi.Server runs in-process in this
// test, so its HostPID() is this test binary's own pid, and the walk stops
// dead the instant it reaches that (membership.go's pid<=1/HostPID rule) —
// so a curl child process is the peer here, registered as session "s1"'s own
// root, making it trivially (depth 1) a C3 member of its own session.
func TestPermission_Member_Accepted(t *testing.T) {
	srv, hookSock := startServerForPermission(t)

	pid, wait := postAsChildProcess(t, hookSock, "/permission", permissionBody("s1"))
	srv.RegisterSessionForTest("s1", pid)
	status := wait()

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestPermission_NonMember_Refused: the peer is a genuine, separate process
// (a curl child, like TestPermission_Member_Accepted's peer) that is NOT a
// descendant of the registered root, so the real membership.Resolve walk
// must actually run — from curl, up to its parent (this test binary, the
// host — membership.go's pid<=1/HostPID stop) — and correctly fail, rather
// than being dismissed for free at depth 1 the way a self-dialing test
// process would be (that was this test's original, dead-code-hiding bug:
// with the test process as its own peer, HostPID() equals the peer's own
// pid, so Resolve rejects before RootByPID is ever consulted, and whatever
// RegisterSessionForTest set up here is never actually exercised). pid 1 is
// used as the (unrelated) root precisely because Resolve's walk stops at
// pid<=1 as a hard boundary before ever consulting Roots for it, so it is
// guaranteed never to match anything, deterministically.
func TestPermission_NonMember_Refused(t *testing.T) {
	srv, hookSock := startServerForPermission(t)
	srv.RegisterSessionForTest("s1", 1)

	_, wait := postAsChildProcess(t, hookSock, "/permission", permissionBody("s1"))
	status := wait()
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

// TestPermission_WrongSession_Refused: the peer IS a genuine member of a
// live session, but the request names a different one. Resolve returns the
// session the peer actually belongs to; the handler must compare it against
// the requested id rather than treating any successful resolution as
// sufficient.
func TestPermission_WrongSession_Refused(t *testing.T) {
	srv, hookSock := startServerForPermission(t)
	srv.RegisterSessionForTest("s2", 1) // some other, unrelated live session

	pid, wait := postAsChildProcess(t, hookSock, "/permission", permissionBody("s2"))
	srv.RegisterSessionForTest("s1", pid) // the peer is really a member of s1...
	status := wait()

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (peer is a member of s1, not s2)", status)
	}
}

// TestPermission_UnknownSession_Refused: naming a session id nothing
// registered must be refused the same way, not treated as an error class of
// its own (C3's own rule: any failure resolves to "not a member"). The peer
// is again a genuine child process — see TestPermission_NonMember_Refused's
// comment for why a self-dialing test process would make this pass for the
// wrong reason.
func TestPermission_UnknownSession_Refused(t *testing.T) {
	_, hookSock := startServerForPermission(t)
	_, wait := postAsChildProcess(t, hookSock, "/permission", permissionBody("no-such-session"))
	status := wait()
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}
