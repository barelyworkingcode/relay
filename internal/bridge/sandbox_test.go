package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// sandboxMembership is a MembershipResolver whose answer the test fixes.
type sandboxMembership struct{ member bool }

func (m sandboxMembership) ResolveMembership(peertoken.Token, time.Time) (MemberSession, bool) {
	return MemberSession{SessionID: "sess-inside", ProjectID: "p1"}, m.member
}

func (m sandboxMembership) RefreshMembership(s MemberSession) (MemberSession, bool) {
	return s, m.member
}

// fakeAttachment records what the bridge does to it. Serve echoes every input
// frame back as output, so a test can see the stream is wired both ways.
type fakeAttachment struct {
	mu      sync.Mutex
	served  bool
	aborted int
	ended   chan struct{}
}

func (a *fakeAttachment) Result() SandboxAttachResult {
	return SandboxAttachResult{SessionID: "s-1", ProjectName: "Widget", TemplateID: "shell", Cols: 80, Rows: 24}
}

func (a *fakeAttachment) Serve(_ context.Context, fc *FrameConn) {
	a.mu.Lock()
	a.served = true
	a.mu.Unlock()
	defer close(a.ended)
	for {
		var f StreamFrame
		if err := fc.ReadValue(&f); err != nil {
			return
		}
		if f.Type == StreamInput {
			_ = fc.WriteValue(StreamFrame{Type: StreamOutput, Data: f.Data})
		}
	}
}

func (a *fakeAttachment) Abort() {
	a.mu.Lock()
	a.aborted++
	a.mu.Unlock()
}

// attachingRouter is a router that can host sandbox sessions.
type attachingRouter struct {
	*stubRouter
	mu   sync.Mutex
	reqs []SandboxAttachRequest
	att  *fakeAttachment
	err  error
}

func (r *attachingRouter) SandboxAttach(_ context.Context, req SandboxAttachRequest) (SandboxAttachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return nil, r.err
	}
	return r.att, nil
}

func (r *attachingRouter) requests() []SandboxAttachRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SandboxAttachRequest(nil), r.reqs...)
}

func newAttachingRouter() *attachingRouter {
	return &attachingRouter{
		stubRouter: &stubRouter{listToolsResponse: json.RawMessage(`[]`)},
		att:        &fakeAttachment{ended: make(chan struct{})},
	}
}

// startSandboxBridge serves router on a Unix socket with the membership answer
// fixed before the first connection can exist.
func startSandboxBridge(t *testing.T, router ToolRouter, member bool) (*BridgeServer, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sb")
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
		membership: sandboxMembership{member: member},
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	return srv, sock
}

func sandboxRequestLine(t *testing.T, args any) []byte {
	t.Helper()
	var raw json.RawMessage
	switch a := args.(type) {
	case nil:
	case json.RawMessage:
		raw = a
	default:
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	line, err := json.Marshal(BridgeRequest{Type: ReqSandboxAttach, Arguments: raw})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return append(line, '\n')
}

func validSandboxArgs() SandboxAttachRequest {
	return SandboxAttachRequest{Template: "shell", Cwd: "/tmp/proj", Cols: 80, Rows: 24}
}

func dialSandbox(t *testing.T, sock string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, bufio.NewReader(c)
}

func readResponse(t *testing.T, r *bufio.Reader) BridgeResponse {
	t.Helper()
	var resp BridgeResponse
	if err := json.Unmarshal(readLine(t, r), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

func refusalOf(t *testing.T, resp BridgeResponse) SandboxRefusal {
	t.Helper()
	if resp.Type != RespError {
		t.Fatalf("response = %+v, want an Error", resp)
	}
	var ref SandboxRefusal
	if err := json.Unmarshal(resp.Data, &ref); err != nil {
		t.Fatalf("refusal data %q: %v", resp.Data, err)
	}
	return ref
}

func TestSandboxAttach_ForwardsRequestAndTurnsConnectionIntoAStream(t *testing.T) {
	router := newAttachingRouter()
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)

	args := SandboxAttachRequest{Template: "claude-code", Cwd: "/tmp/proj/sub", Project: "Widget", Cols: 100, Rows: 30}
	_, _ = conn.Write(sandboxRequestLine(t, args))
	ack := readResponse(t, r)
	if ack.Type != RespAttached {
		t.Fatalf("ack = %+v, want Attached", ack)
	}
	var res SandboxAttachResult
	if err := json.Unmarshal(ack.Data, &res); err != nil || res.SessionID != "s-1" {
		t.Fatalf("ack data = %s (%v)", ack.Data, err)
	}
	if got := router.requests(); len(got) != 1 || got[0] != args {
		t.Fatalf("router saw %+v, want exactly %+v", got, args)
	}

	// Bytes flow both ways on the same connection after the ack.
	_, _ = conn.Write([]byte(`{"type":"input","data":"bHM="}` + "\n"))
	var out StreamFrame
	if err := json.Unmarshal(readLine(t, r), &out); err != nil || out.Type != StreamOutput || string(out.Data) != "ls" {
		t.Fatalf("output frame = %+v (%v)", out, err)
	}
}

func TestSandboxAttach_ClientDisconnectEndsTheStream(t *testing.T) {
	router := newAttachingRouter()
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	readResponse(t, r)

	_ = conn.Close()
	select {
	case <-router.att.ended:
	case <-time.After(streamTestTimeout):
		t.Fatal("attachment kept serving after the client closed the connection")
	}
}

func TestSandboxAttach_ConnectionClosesWhenTheAttachmentReturns(t *testing.T) {
	router := newAttachingRouter()
	router.att = &fakeAttachment{ended: make(chan struct{})}
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	readResponse(t, r)

	// Closing the write side is an EOF for the attachment; the server then has
	// to close its side too, so the client sees the stream end.
	_ = conn.(*net.UnixConn).CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(streamTestTimeout))
	if _, err := r.ReadBytes('\n'); err == nil {
		t.Fatal("read succeeded, want the server to close the stream")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("server never closed the connection after the attachment returned")
	}
}

func TestSandboxAttach_ServerCloseDoesNotHangOnAnAttachedStream(t *testing.T) {
	router := newAttachingRouter()
	srv, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	readResponse(t, r)

	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(streamTestTimeout):
		t.Fatal("BridgeServer.Close hung on an attached stream")
	}
}

// The membership answer is the kernel's, so this refusal holds whatever the
// client did or did not check for itself: nothing here sets RELAY_SESSION_ID.
func TestSandboxAttach_RefusedServerSideWhenConnectionIsASessionMember(t *testing.T) {
	router := newAttachingRouter()
	_, sock := startSandboxBridge(t, router, true)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	if ref := refusalOf(t, resp); ref.Reason != SandboxReasonInsideSession || ref.Message == "" {
		t.Fatalf("refusal = %+v, want reason %q with a message", ref, SandboxReasonInsideSession)
	}
	if resp.Type == RespAttached {
		t.Fatal("a session member was attached")
	}
	if n := len(router.requests()); n != 0 {
		t.Fatalf("router was asked to launch %d time(s) for a session member", n)
	}
	router.att.mu.Lock()
	defer router.att.mu.Unlock()
	if router.att.served {
		t.Fatal("a session member's connection was taken over")
	}
}

func TestSandboxAttach_MemberRefusalPrecedesEveryOtherCheck(t *testing.T) {
	// A router that cannot host sandboxes and an unparseable body would each
	// produce a different error; the membership refusal has to win, so a
	// caller inside a session learns nothing else and reaches nothing else.
	_, sock := startSandboxBridge(t, &stubRouter{}, true)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, json.RawMessage(`"not an object"`)))
	if ref := refusalOf(t, readResponse(t, r)); ref.Reason != SandboxReasonInsideSession {
		t.Fatalf("reason = %q, want %q", ref.Reason, SandboxReasonInsideSession)
	}
}

func TestSandboxAttach_RefusedRequestLeavesTheConnectionRequestResponse(t *testing.T) {
	router := newAttachingRouter()
	_, sock := startSandboxBridge(t, router, true)
	conn, r := dialSandbox(t, sock)

	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	readResponse(t, r)
	_, _ = conn.Write([]byte(`{"type":"ListTools","token":"t"}` + "\n"))
	if resp := readResponse(t, r); resp.Type == RespError {
		t.Fatalf("a refused sandbox request broke the connection: %+v", resp)
	}
}

func TestSandboxAttach_InvalidRequestsNeverReachTheRouter(t *testing.T) {
	cases := map[string]any{
		"no arguments":   nil,
		"malformed body": json.RawMessage(`"not an object"`),
		"empty template": SandboxAttachRequest{Cwd: "/tmp/proj"},
		"empty cwd":      SandboxAttachRequest{Template: "shell"},
		"relative cwd":   SandboxAttachRequest{Template: "shell", Cwd: "proj"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			router := newAttachingRouter()
			_, sock := startSandboxBridge(t, router, false)
			conn, r := dialSandbox(t, sock)
			_, _ = conn.Write(sandboxRequestLine(t, args))
			resp := readResponse(t, r)
			if resp.Type != RespError {
				t.Fatalf("response = %+v, want an Error", resp)
			}
			if n := len(router.requests()); n != 0 {
				t.Fatalf("router saw %d request(s) for an invalid one", n)
			}
		})
	}
}

func TestSandboxAttach_RouterThatCannotHostSandboxesRefuses(t *testing.T) {
	_, sock := startSandboxBridge(t, &stubRouter{}, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	if resp.Type != RespError || resp.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("response = %+v, want a method-not-found error", resp)
	}
}

func TestSandboxAttach_StructuredRefusalReachesTheClientUnchanged(t *testing.T) {
	router := newAttachingRouter()
	want := &SandboxRefusal{
		Reason: SandboxReasonProjectAmbiguous, Projects: []string{"Alpha (a)", "Beta (b)"},
		Message: "more than one project; use --project",
	}
	router.err = want
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))

	resp := readResponse(t, r)
	got := refusalOf(t, resp)
	if got.Reason != want.Reason || got.Message != want.Message || strings.Join(got.Projects, ",") != strings.Join(want.Projects, ",") {
		t.Fatalf("refusal = %+v, want %+v", got, *want)
	}
	if resp.Message == "" {
		t.Fatal("the Error response carries no plain message for a client that ignores Data")
	}
}

func TestSandboxAttach_WrappedRefusalIsStillRecognised(t *testing.T) {
	router := newAttachingRouter()
	router.err = errors.Join(errors.New("context"), &SandboxRefusal{Reason: SandboxReasonNoProject, Message: "nowhere"})
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	if ref := refusalOf(t, readResponse(t, r)); ref.Reason != SandboxReasonNoProject {
		t.Fatalf("reason = %q, want %q", ref.Reason, SandboxReasonNoProject)
	}
}

func TestSandboxAttach_UnstructuredFailureDoesNotLeakItsDetail(t *testing.T) {
	router := newAttachingRouter()
	router.err = errors.New("dial unix /Users/someone/secret/path: refused")
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(sandboxRequestLine(t, validSandboxArgs()))
	resp := readResponse(t, r)
	if resp.Type != RespError {
		t.Fatalf("response = %+v, want an Error", resp)
	}
	if strings.Contains(resp.Message, "secret") || strings.Contains(string(resp.Data), "secret") {
		t.Fatalf("internal error detail leaked to the client: %+v", resp)
	}
}

// handleSandboxAttach is called with a context that did not come from Serve, so
// the takeover cannot be installed; the launched session must not be left
// running with nobody to end it.
func TestSandboxAttach_AbortsTheSessionWhenNoStreamCanBeCarried(t *testing.T) {
	router := newAttachingRouter()
	req := &BridgeRequest{Type: ReqSandboxAttach}
	req.Arguments, _ = json.Marshal(validSandboxArgs())

	resp := handleSandboxAttach(context.Background(), req, router)
	if resp.Type == RespAttached {
		t.Fatal("acked a stream that could not be carried")
	}
	router.att.mu.Lock()
	defer router.att.mu.Unlock()
	if router.att.aborted != 1 || router.att.served {
		t.Fatalf("aborted=%d served=%v, want the attachment aborted once and never served", router.att.aborted, router.att.served)
	}
}

func TestSandboxAttach_IsAnOrdinaryUngatedEntryOfTheBridgeTable(t *testing.T) {
	h, ok := bridgeHandlers[ReqSandboxAttach]
	if !ok {
		t.Fatal("ReqSandboxAttach has no bridge handler")
	}
	if h.requireAdmin {
		t.Fatal("sandbox attach demands an admin bearer; the command is ungated and carries none")
	}
}

func TestSandboxAttach_OtherBridgeRequestsAreUnaffectedByTheTakeoverSeam(t *testing.T) {
	router := newAttachingRouter()
	_, sock := startSandboxBridge(t, router, false)
	conn, r := dialSandbox(t, sock)
	for i := 0; i < 3; i++ {
		_, _ = conn.Write([]byte(`{"type":"ListTools","token":"t"}` + "\n"))
		if resp := readResponse(t, r); resp.Type == RespError {
			t.Fatalf("ListTools #%d: %+v", i, resp)
		}
	}
	if n := len(router.listToolsTokens); n != 3 {
		t.Fatalf("router served %d ListTools, want 3 on one connection", n)
	}
}

func TestStreamFrame_WireShape(t *testing.T) {
	cases := []struct {
		frame StreamFrame
		want  string
	}{
		{StreamFrame{Type: StreamInput, Data: []byte("hi")}, `{"type":"input","data":"aGk=","code":0}`},
		{StreamFrame{Type: StreamResize, Cols: 120, Rows: 40}, `{"type":"resize","cols":120,"rows":40,"code":0}`},
		{StreamFrame{Type: StreamOutput, Data: []byte("x")}, `{"type":"output","data":"eA==","code":0}`},
		{StreamFrame{Type: StreamExit, Code: 0}, `{"type":"exit","code":0}`},
		{StreamFrame{Type: StreamExit, Code: 7}, `{"type":"exit","code":7}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.frame)
		if err != nil {
			t.Fatal(err)
		}
		// Compared as decoded maps for the fields the protocol defines: an
		// extra zero "code" on non-exit frames is harmless, a missing one on an
		// exit frame is not.
		var got, want map[string]any
		_ = json.Unmarshal(b, &got)
		_ = json.Unmarshal([]byte(c.want), &want)
		for k, v := range want {
			if k == "code" && c.frame.Type != StreamExit {
				continue
			}
			if got[k] != v {
				t.Errorf("%s frame field %q = %v, want %v (wire %s)", c.frame.Type, k, got[k], v, b)
			}
		}
	}
	if StreamInput != "input" || StreamResize != "resize" || StreamOutput != "output" || StreamExit != "exit" {
		t.Fatal("stream frame type literals changed; they are the CLI/relay wire protocol")
	}
}
