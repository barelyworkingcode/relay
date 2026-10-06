package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type fakeDropInAttachment struct{ fakeAttachment }

func (a *fakeDropInAttachment) Result() DropInAttachResult {
	return DropInAttachResult{SessionID: "a-1", ClaudeSessionID: "c-1", TerminalID: "t-1", Cols: 100, Rows: 30}
}

// dropInRouter is a router that can take sessions over; it records what it is
// asked and answers err when set.
type dropInRouter struct {
	*stubRouter
	mu   sync.Mutex
	reqs []DropInAttachRequest
	err  error
	att  *fakeDropInAttachment
}

func (r *dropInRouter) DropInAttach(_ context.Context, req DropInAttachRequest) (DropInAttachment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return nil, r.err
	}
	return r.att, nil
}

func (r *dropInRouter) asked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func newDropInRouter() *dropInRouter {
	return &dropInRouter{
		stubRouter: &stubRouter{listToolsResponse: json.RawMessage(`[]`)},
		att:        &fakeDropInAttachment{fakeAttachment{ended: make(chan struct{})}},
	}
}

func dropInRequestLine(t *testing.T, args any) []byte {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line, _ := json.Marshal(BridgeRequest{Type: ReqDropInAttach, Arguments: raw})
	return append(line, '\n')
}

func TestDropInAttach_IsAnOrdinaryUngatedEntryOfTheBridgeTable(t *testing.T) {
	h, ok := bridgeHandlers[ReqDropInAttach]
	if !ok {
		t.Fatal("ReqDropInAttach has no bridge handler")
	}
	if h.requireAdmin {
		t.Fatal("drop-in demands an admin bearer; the command carries none")
	}
}

// The caller checks run before the arguments are looked at, so a sandboxed
// caller learns nothing about whether a session id exists.
func TestDropInAttach_RefusesSessionMembersAndConfinedPeersBeforeParsing(t *testing.T) {
	cases := []struct {
		name     string
		member   bool
		confined *fakeConfined
		reason   string
	}{
		{"member of a relay session", true, &fakeConfined{ok: true}, SandboxReasonInsideSession},
		{"confined peer", false, &fakeConfined{confined: true, ok: true}, SandboxReasonPeerConfined},
		{"confinement unknown", false, &fakeConfined{ok: false}, SandboxReasonPeerConfined},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router := newDropInRouter()
			sock := startConfinedSandboxBridge(t, router, c.member, c.confined)
			conn, r := dialSandbox(t, sock)

			// Unparseable arguments: only a check that runs first can answer with a reason.
			_, _ = conn.Write([]byte(`{"type":"DropInAttach","arguments":"not an object"}` + "\n"))
			ref := refusalOf(t, readResponse(t, r))
			if ref.Reason != c.reason || ref.Message == "" {
				t.Fatalf("refusal = %+v, want reason %q with a message", ref, c.reason)
			}
			if n := router.asked(); n != 0 {
				t.Fatalf("router was asked to drop in %d time(s) for a refused caller", n)
			}
		})
	}
}

func TestDropInAttach_AcksWithTheAttachmentAndRelaysARouterRefusal(t *testing.T) {
	router := newDropInRouter()
	sock := startConfinedSandboxBridge(t, router, false, &fakeConfined{ok: true})
	conn, r := dialSandbox(t, sock)

	args := DropInAttachRequest{SessionID: "a-1", Cols: 100, Rows: 30}
	_, _ = conn.Write(dropInRequestLine(t, args))
	ack := readResponse(t, r)
	if ack.Type != RespAttached {
		t.Fatalf("ack = %+v, want Attached", ack)
	}
	var res DropInAttachResult
	if err := json.Unmarshal(ack.Data, &res); err != nil || res.SessionID != "a-1" || res.ClaudeSessionID != "c-1" || res.TerminalID != "t-1" {
		t.Fatalf("ack data = %s (%v)", ack.Data, err)
	}
	router.mu.Lock()
	got := append([]DropInAttachRequest(nil), router.reqs...)
	router.mu.Unlock()
	if len(got) != 1 || got[0] != args {
		t.Fatalf("router saw %+v, want exactly %+v", got, args)
	}

	router2 := newDropInRouter()
	router2.err = &SandboxRefusal{Reason: "tool_running", Message: "a tool is running (Bash)"}
	sock2 := startConfinedSandboxBridge(t, router2, false, &fakeConfined{ok: true})
	conn2, r2 := dialSandbox(t, sock2)
	_, _ = conn2.Write(dropInRequestLine(t, args))
	if ref := refusalOf(t, readResponse(t, r2)); ref.Reason != "tool_running" || ref.Message != "a tool is running (Bash)" {
		t.Fatalf("refusal = %+v, want the router's reason and message relayed", ref)
	}
}

func TestDropInAttach_RejectsAnEmptySessionIDWithoutAskingTheRouter(t *testing.T) {
	router := newDropInRouter()
	sock := startConfinedSandboxBridge(t, router, false, &fakeConfined{ok: true})
	conn, r := dialSandbox(t, sock)
	_, _ = conn.Write(dropInRequestLine(t, DropInAttachRequest{Cols: 80, Rows: 24}))
	if resp := readResponse(t, r); resp.Type != RespError {
		t.Fatalf("response = %+v, want an Error", resp)
	}
	if n := router.asked(); n != 0 {
		t.Fatalf("router asked %d time(s) with no session id", n)
	}
}
