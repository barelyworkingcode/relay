package bridge

// Tests for handleSandboxAttach's second, independent refusal: the peer's own
// current Seatbelt confinement (docs/sandbox-command.md, "Refused from inside
// a relay session"). The ancestry-based membership check is covered by
// sandbox_test.go; these tests fix membership at false throughout except
// where a case explicitly needs both checks in play, and fake the
// confinement answer via BridgeServer.peerConfined (SetPeerConfinedForTest's
// underlying field) so no real Seatbelt profile is needed to exercise the
// gate.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// fakeConfined is a PeerConfinedFunc whose answer a test fixes and whose call
// count a test can read back, to pin how often handleConn consults it.
type fakeConfined struct {
	confined bool
	ok       bool
	calls    atomic.Int32
}

func (f *fakeConfined) fn(peertoken.Token) (bool, bool) {
	f.calls.Add(1)
	return f.confined, f.ok
}

// startConfinedSandboxBridge is startSandboxBridge (sandbox_test.go) with an
// additional, independently controllable PeerConfinedFunc: the two checks in
// handleSandboxAttach are meant to be independent, so a test of the
// confinement gate must be able to fix each one without the other.
func startConfinedSandboxBridge(t *testing.T, router ToolRouter, member bool, confined *fakeConfined) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sbc")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &BridgeServer{
		router: router, listener: ln, sockPath: sock, ctx: ctx, cancel: cancel,
		membership:   sandboxMembership{member: member},
		peerConfined: confined.fn,
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	return sock
}

// --- member=false, confined=false: an ordinary operator's own terminal ----

// This is the regression case the story's fix must not break: an ordinary
// caller is never a session member and never confined, so the new gate must
// let the request through exactly as it did before the confinement check
// existed. It mirrors sandbox_test.go's forwarding test but fixes
// confinement explicitly via the fake rather than relying on whatever the
// real, unfaked PeerConfined happens to answer for the test binary itself.
func TestHandleSandboxAttach_NeitherCheckFires_Proceeds(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: false, ok: true}
	sock := startConfinedSandboxBridge(t, router, false, confined)
	conn, r := dialSandbox(t, sock)

	args := SandboxAttachRequest{Template: "claude-code", Cwd: "/tmp/proj/sub", Cols: 100, Rows: 30}
	_, _ = conn.Write(sandboxRequestLine(t, args))
	ack := readResponse(t, r)
	if ack.Type != RespAttached {
		t.Fatalf("ack = %+v, want Attached", ack)
	}
	if got := router.requests(); len(got) != 1 || got[0] != args {
		t.Fatalf("router saw %+v, want exactly %+v", got, args)
	}

	// The stream itself must still be wired both ways: a gate that quietly
	// swallowed bytes instead of refusing would not show up as an Error
	// response, only as a broken pump.
	_, _ = conn.Write([]byte(`{"type":"input","data":"bHM="}` + "\n"))
	var out StreamFrame
	if err := json.Unmarshal(readLine(t, r), &out); err != nil || out.Type != StreamOutput || string(out.Data) != "ls" {
		t.Fatalf("output frame = %+v (%v)", out, err)
	}
}

// --- member=true, confined=false: existing behaviour must still hold ------

func TestHandleSandboxAttach_MemberRefusedRegardlessOfConfinement(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: false, ok: true}
	sock := startConfinedSandboxBridge(t, router, true, confined)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	if ref := refusalOf(t, resp); ref.Reason != SandboxReasonInsideSession || ref.Message == "" {
		t.Fatalf("refusal = %+v, want reason %q with a message", ref, SandboxReasonInsideSession)
	}
	if n := len(router.requests()); n != 0 {
		t.Fatalf("router was asked to launch %d time(s) for a session member", n)
	}
}

// --- member=false, confined=true: the new, independent refusal ------------

func TestHandleSandboxAttach_ConfinedPeerRefusedWithDistinctReason(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: true, ok: true}
	sock := startConfinedSandboxBridge(t, router, false, confined)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	ref := refusalOf(t, resp)
	if ref.Reason != SandboxReasonPeerConfined || ref.Message == "" {
		t.Fatalf("refusal = %+v, want reason %q with a message", ref, SandboxReasonPeerConfined)
	}
	if ref.Reason == SandboxReasonInsideSession {
		t.Fatal("a confined, non-member peer was refused with the membership reason")
	}
	if n := len(router.requests()); n != 0 {
		t.Fatalf("router was asked to launch %d time(s) for a confined peer", n)
	}
}

// --- member=true, confined=true: refused, membership reason takes ---------
// precedence (handleSandboxAttach checks membership first and returns before
// the confinement check ever runs; this is a property of the current, plain
// sequential code, not a documented contract, but it is what a caller
// observes today).

func TestHandleSandboxAttach_MemberAndConfined_Refused(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: true, ok: true}
	sock := startConfinedSandboxBridge(t, router, true, confined)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	if resp.Type != RespError {
		t.Fatalf("response = %+v, want an Error for a member who is also confined", resp)
	}
	ref := refusalOf(t, resp)
	if ref.Reason != SandboxReasonInsideSession && ref.Reason != SandboxReasonPeerConfined {
		t.Fatalf("reason = %q, want one of %q or %q", ref.Reason, SandboxReasonInsideSession, SandboxReasonPeerConfined)
	}
	if n := len(router.requests()); n != 0 {
		t.Fatalf("router was asked to launch %d time(s) for a confined member", n)
	}
}

// --- fail-closed: the confinement check could not run at all --------------

// !ok is the shape peer_confined_darwin.go's PeerConfined and
// peer_confined_other.go's stand-in both use to mean "could not determine" —
// an unreadable peer token, an unresolved dlsym symbol, or (off darwin) no
// implementation at all. The contract in sandbox.go's PeerConfinedFunc doc is
// explicit: a caller must treat !ok exactly like confined == true, because
// there is no safe reading of "could not verify".
func TestHandleSandboxAttach_ConfinementUnknown_FailsClosedAsConfined(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: false, ok: false}
	sock := startConfinedSandboxBridge(t, router, false, confined)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	ref := refusalOf(t, resp)
	if ref.Reason != SandboxReasonPeerConfined {
		t.Fatalf("reason = %q, want %q for an undetermined confinement answer", ref.Reason, SandboxReasonPeerConfined)
	}
	if n := len(router.requests()); n != 0 {
		t.Fatalf("router was asked to launch %d time(s) when confinement could not be determined", n)
	}
}

// --- computed once per connection, not per request -------------------------

// handleConn resolves peerConfined exactly once, at accept time, and stashes
// the answer on the context for every later request on the same connection
// to read back (server.go). A regression to computing it per request would
// still pass every refusal test above; only counting calls on one connection
// across several requests can tell the two shapes apart.
func TestHandleSandboxAttach_ConfinementIsComputedOnceRegardlessOfRequestCount(t *testing.T) {
	router := newAttachingRouter()
	confined := &fakeConfined{confined: false, ok: true}
	sock := startConfinedSandboxBridge(t, router, false, confined)
	conn, r := dialSandbox(t, sock)

	for i := 0; i < 3; i++ {
		_, _ = conn.Write([]byte(`{"type":"ListTools","token":"t"}` + "\n"))
		if resp := readResponse(t, r); resp.Type == RespError {
			t.Fatalf("ListTools #%d: %+v", i, resp)
		}
	}
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	if ack := readResponse(t, r); ack.Type != RespAttached {
		t.Fatalf("sandbox attach = %+v, want Attached", ack)
	}

	if n := confined.calls.Load(); n != 1 {
		t.Fatalf("PeerConfinedFunc called %d time(s) on one connection across 4 requests, want exactly 1", n)
	}
}
