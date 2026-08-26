//go:build !windows

package main

// Issue #39, defect 1: an MCP response longer than bridge.MaxMessageSize used
// to be fatal to the whole connection. bufio.Scanner returns bufio.ErrTooLong
// and cannot resync, so readLoop exited, every pending call failed, and the
// connection — shared by every access profile that names the MCP — stayed dead.
// One over-long fs_read on one profile took four unrelated enrolments down.
//
// A frame that is too long is one bad answer. It fails the one call it belongs
// to and nothing else.

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"relaygo/bridge"
)

// newTestBufReader wraps a string in the same reader readLoop uses, so the
// frame tests exercise the production buffer size.
func newTestBufReader(s string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(s), mcpReadBufferSize)
}

func TestStdioConn_OversizedFrameFailsOnlyItsOwnCall(t *testing.T) {
	conn := newTestMcpConn(t)
	ctx := context.Background()

	_, err := conn.SendRequest(ctx, "oversize", nil)
	if err == nil {
		t.Fatal("an over-long response must fail its call")
	}
	// The caller has to be told what to do differently. "token too long" is a
	// fact about relay's scanner, not about the request that produced it.
	if !strings.Contains(err.Error(), "exceeds relay's") {
		t.Errorf("error = %q, want it to name the size limit it broke", err)
	}
	if !strings.Contains(err.Error(), "page") {
		t.Errorf("error = %q, want it to say what the MCP should do instead", err)
	}

	// The connection is the shared one. It must still be there.
	select {
	case <-conn.readerDone:
		t.Fatal("the connection died with the frame: one over-long line is still a permanent outage")
	default:
	}

	res, err := conn.SendRequest(ctx, "echo", map[string]any{"marker": "unaffected"})
	if err != nil {
		t.Fatalf("a later call on the same connection failed: %v", err)
	}
	if got := markerOf(t, res); got != "unaffected" {
		t.Errorf("marker = %q, want unaffected", got)
	}
}

// Resync has to land on a frame boundary. Reading the tail of a discarded
// oversized frame as if it were a message would hand a fragment to
// json.Unmarshal — and the frame after it is a real response someone is
// waiting for.
func TestStdioConn_ResyncsToTheNextFrameBoundary(t *testing.T) {
	conn := newTestMcpConn(t)
	ctx := context.Background()

	// Three concurrent calls: the oversized one fails, both neighbours are
	// answered correctly, and none of them is answered with a fragment.
	type result struct {
		marker string
		err    error
	}
	results := make(chan result, 2)
	for _, marker := range []string{"before", "after"} {
		go func(marker string) {
			res, err := conn.SendRequest(ctx, "echo", map[string]any{"marker": marker, "delayMs": 40})
			if err != nil {
				results <- result{err: err}
				return
			}
			var p struct {
				Marker string `json:"marker"`
			}
			results <- result{marker: p.Marker, err: json.Unmarshal(res, &p)}
		}(marker)
	}
	if _, err := conn.SendRequest(ctx, "oversize", nil); err == nil {
		t.Fatal("the oversized call should have failed")
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("neighbouring call failed: %v", r.err)
		}
		seen[r.marker] = true
	}
	for _, want := range []string{"before", "after"} {
		if !seen[want] {
			t.Errorf("call %q was not answered correctly across the oversized frame", want)
		}
	}
}

// readMcpFrame is the piece that makes the above possible: it must count and
// discard an over-long frame rather than buffer it, and leave the reader on the
// next frame.
func TestReadMcpFrame_DiscardsAndResyncs(t *testing.T) {
	big := strings.Repeat("A", bridge.MaxMessageSize+1024)
	input := "{\"a\":1}\n" + big + "\n{\"b\":2}\n"
	r := newTestBufReader(input)

	first, err := readMcpFrame(r)
	if err != nil || string(first.line) != `{"a":1}` {
		t.Fatalf("first frame = %q err %v", first.line, err)
	}

	over, err := readMcpFrame(r)
	if err != nil {
		t.Fatalf("oversized frame returned err %v, want it handled in-band", err)
	}
	if !over.oversized {
		t.Fatal("an over-long frame was not reported as oversized")
	}
	if over.line != nil {
		t.Error("an over-long frame must not be delivered as a message")
	}
	if over.size != len(big)+1 {
		t.Errorf("reported size %d, want %d", over.size, len(big)+1)
	}

	third, err := readMcpFrame(r)
	if err != nil || string(third.line) != `{"b":2}` {
		t.Fatalf("did not resync to the next frame: got %q err %v", third.line, err)
	}
}

// The id is recovered from the frame's prefix so the one waiting call can be
// failed. It must never be recovered from inside the payload — failing an
// unrelated in-flight call is worse than failing none.
func TestPeekFrameResponseID(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   int64
		ok     bool
	}{
		{"conventional order", `{"jsonrpc":"2.0","id":7,"result":"AAAA`, 7, true},
		{"id first", `{"id":12,"jsonrpc":"2.0","result":{"x":`, 12, true},
		{"nested id is not the frame's id", `{"jsonrpc":"2.0","result":{"id":99,"data":"AAA`, 0, false},
		{"id after a truncated payload is an honest miss", `{"jsonrpc":"2.0","result":"AAAAAAA`, 0, false},
		{"notification has none", `{"jsonrpc":"2.0","method":"notifications/progress","params":{"x":1}}`, 0, false},
		{"not an object", `["a","b"`, 0, false},
		{"empty", ``, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := peekFrameResponseID([]byte(tc.prefix))
			if ok != tc.ok || got != tc.want {
				t.Errorf("peekFrameResponseID(%q) = %d,%v want %d,%v", tc.prefix, got, ok, tc.want, tc.ok)
			}
		})
	}
}
