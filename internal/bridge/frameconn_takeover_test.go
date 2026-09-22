package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const streamTestTimeout = 5 * time.Second

// unixPair returns two connected Unix-socket ends. A socket pair, not
// net.Pipe: net.Pipe is unbuffered, so a writer blocks until the peer reads,
// which would make these tests deadlock where the real socket does not.
func unixPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pair")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, client
}

// deadlineRecorder counts every deadline a connection is given. A stream that
// must survive sleep has to leave the count at zero.
type deadlineRecorder struct {
	net.Conn
	calls atomic.Int32
}

func (d *deadlineRecorder) SetDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetDeadline(t)
}
func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetReadDeadline(t)
}
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.calls.Add(1)
	return d.Conn.SetWriteDeadline(t)
}

func readLine(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read line: %v", err)
	}
	return line
}

// serveWithTakeover runs Serve on the server end with a handler that counts
// requests and installs fn as the takeover for requests of type "take".
func serveWithTakeover(server net.Conn, fn func(ctx context.Context, fc *FrameConn), handled *atomic.Int32) <-chan struct{} {
	done := make(chan struct{})
	fc := NewFrameConn(server, "test", 0)
	go func() {
		defer close(done)
		fc.Serve(context.Background(), func(ctx context.Context, line string) BridgeResponse {
			handled.Add(1)
			var req BridgeRequest
			_ = json.Unmarshal([]byte(line), &req)
			if req.Type == "take" {
				SetTakeover(ctx, fn)
				return BridgeResponse{Type: RespAttached}
			}
			return BridgeResponse{Type: RespResult}
		})
	}()
	return done
}

func TestFrameConn_TakeoverAnswersFirstThenOwnsTheConnection(t *testing.T) {
	server, client := unixPair(t)
	var handled atomic.Int32
	got := make(chan StreamFrame, 4)
	done := serveWithTakeover(server, func(_ context.Context, fc *FrameConn) {
		for {
			var f StreamFrame
			if err := fc.ReadValue(&f); err != nil {
				return
			}
			got <- f
		}
	}, &handled)

	// One write carries the request and the first stream frame, so the frame
	// is already inside Serve's scanner buffer when the takeover starts.
	_, err := client.Write([]byte(`{"type":"take"}` + "\n" + `{"type":"input","data":"aGk="}` + "\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	r := bufio.NewReader(client)
	var ack BridgeResponse
	if err := json.Unmarshal(readLine(t, r), &ack); err != nil || ack.Type != RespAttached {
		t.Fatalf("ack = %+v (%v), want Attached", ack, err)
	}
	select {
	case f := <-got:
		if f.Type != StreamInput || string(f.Data) != "hi" {
			t.Fatalf("buffered frame lost or mangled: %+v", f)
		}
	case <-time.After(streamTestTimeout):
		t.Fatal("frame already buffered by Serve never reached the takeover")
	}

	// A line that looks like a request is stream data now, never dispatched.
	_, _ = client.Write([]byte(`{"type":"ListTools"}` + "\n"))
	select {
	case f := <-got:
		if f.Type != "ListTools" {
			t.Fatalf("second frame = %+v", f)
		}
	case <-time.After(streamTestTimeout):
		t.Fatal("takeover did not receive the follow-up line")
	}
	if n := handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1: the connection is read by two loops", n)
	}

	_ = client.Close()
	select {
	case <-done:
	case <-time.After(streamTestTimeout):
		t.Fatal("Serve did not return after the takeover returned")
	}
}

func TestFrameConn_ServeReturnsWhenTakeoverReturns(t *testing.T) {
	server, client := unixPair(t)
	var handled atomic.Int32
	done := serveWithTakeover(server, func(context.Context, *FrameConn) {}, &handled)

	_, _ = client.Write([]byte(`{"type":"take"}` + "\n"))
	select {
	case <-done:
	case <-time.After(streamTestTimeout):
		t.Fatal("Serve kept reading after the takeover returned")
	}
}

func TestFrameConn_RequestsThatDoNotTakeOverKeepOneRequestOneResponse(t *testing.T) {
	server, client := unixPair(t)
	var handled atomic.Int32
	done := serveWithTakeover(server, func(context.Context, *FrameConn) {
		t.Error("takeover ran for a request that did not ask for one")
	}, &handled)

	r := bufio.NewReader(client)
	for i := 0; i < 3; i++ {
		_, _ = client.Write([]byte(`{"type":"plain"}` + "\n"))
		var resp BridgeResponse
		if err := json.Unmarshal(readLine(t, r), &resp); err != nil || resp.Type != RespResult {
			t.Fatalf("request %d: response %+v (%v)", i, resp, err)
		}
	}
	if n := handled.Load(); n != 3 {
		t.Fatalf("handler ran %d times, want 3", n)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(streamTestTimeout):
		t.Fatal("Serve did not end on EOF")
	}
}

func TestSetTakeover_RefusesAContextNotFromServe(t *testing.T) {
	if SetTakeover(context.Background(), func(context.Context, *FrameConn) {}) {
		t.Fatal("SetTakeover accepted a context that did not come from Serve")
	}
}

func TestFrameConn_TakeoverInstalledWithNilFuncIsRefused(t *testing.T) {
	server, client := unixPair(t)
	res := make(chan bool, 1)
	fc := NewFrameConn(server, "test", 0)
	go fc.Serve(context.Background(), func(ctx context.Context, _ string) BridgeResponse {
		res <- SetTakeover(ctx, nil)
		return BridgeResponse{Type: RespResult}
	})
	_, _ = client.Write([]byte(`{"type":"x"}` + "\n"))
	if <-res {
		t.Fatal("SetTakeover(nil) reported success")
	}
}

func TestFrameConn_WriteValueFramesStayIntactUnderConcurrentWriters(t *testing.T) {
	server, client := unixPair(t)
	fc := NewFrameConn(server, "test", 0)

	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				payload := make([]byte, 2048)
				for j := range payload {
					payload[j] = byte('a' + w)
				}
				if err := fc.WriteValue(StreamFrame{Type: StreamOutput, Data: payload}); err != nil {
					t.Errorf("WriteValue: %v", err)
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); _ = server.Close() }()

	r := bufio.NewReaderSize(client, 1<<20)
	count := 0
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f StreamFrame
		if err := json.Unmarshal(line, &f); err != nil {
			t.Fatalf("interleaved or torn frame: %v", err)
		}
		for _, b := range f.Data {
			if b != f.Data[0] {
				t.Fatal("frame mixes bytes from two writers")
			}
		}
		count++
	}
	if count != writers*each {
		t.Fatalf("received %d frames, want %d", count, writers*each)
	}
}

func TestFrameConn_ReadValueReportsEOFWhenPeerCloses(t *testing.T) {
	server, client := unixPair(t)
	fc := NewFrameConn(server, "test", 0)
	_ = client.Close()
	var f StreamFrame
	if err := fc.ReadValue(&f); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadValue on a closed peer = %v, want io.EOF", err)
	}
}

func TestFrameConn_CloseUnblocksAReadValueInProgress(t *testing.T) {
	server, _ := unixPair(t)
	fc := NewFrameConn(server, "test", 0)
	res := make(chan error, 1)
	go func() {
		var f StreamFrame
		res <- fc.ReadValue(&f)
	}()
	_ = fc.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("ReadValue returned nil after Close")
		}
	case <-time.After(streamTestTimeout):
		t.Fatal("Close did not unblock ReadValue")
	}
}

// The attach stream has to survive a machine sleeping under it, so the
// transport must never apply a deadline of its own, however it is driven.
func TestFrameConn_ZeroIdleStreamNeverSetsADeadline(t *testing.T) {
	server, client := unixPair(t)
	rec := &deadlineRecorder{Conn: server}
	var handled atomic.Int32
	fc := NewFrameConn(rec, "test", 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fc.Serve(context.Background(), func(ctx context.Context, _ string) BridgeResponse {
			handled.Add(1)
			SetTakeover(ctx, func(_ context.Context, fc *FrameConn) {
				for {
					var f StreamFrame
					if err := fc.ReadValue(&f); err != nil {
						return
					}
					_ = fc.WriteValue(StreamFrame{Type: StreamOutput, Data: f.Data})
				}
			})
			return BridgeResponse{Type: RespAttached}
		})
	}()

	r := bufio.NewReader(client)
	_, _ = client.Write([]byte(`{"type":"take"}` + "\n"))
	readLine(t, r)
	for i := 0; i < 20; i++ {
		_, _ = client.Write([]byte(`{"type":"input","data":"eA=="}` + "\n"))
		readLine(t, r)
	}
	_ = client.Close()
	<-done
	if n := rec.calls.Load(); n != 0 {
		t.Fatalf("stream applied %d deadline(s); a stream must have none", n)
	}
}
