package main

import (
	"bufio"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPreambleThenConn_ReadTouchesIdleDeadlineOnEveryRead is item 5's read-side
// regression test: serveMount clears the connection's read deadline before
// handing it to p9.Server.Handle, and the p9 library never sets one of its
// own, so preambleThenConn.Read must re-arm the idle deadline itself on
// every call or a silent mount session hangs open forever.
func TestPreambleThenConn_ReadTouchesIdleDeadlineOnEveryRead(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var touches int32
	c := &preambleThenConn{
		Reader: bufio.NewReader(strings.NewReader("hello world")),
		Conn:   client,
		touch:  func() { atomic.AddInt32(&touches, 1) },
	}

	buf := make([]byte, 5)
	for i := 0; i < 3; i++ {
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&touches); got != 3 {
		t.Fatalf("expected touch to run on every Read (3 reads), got %d calls", got)
	}
}

// TestMountIdleWriter_WriteTouchesIdleDeadlineOnEveryWrite is the write-side
// counterpart: p9.Server.Handle's write half is a distinct parameter from
// its read half, so it needs its own deadline re-arm.
func TestMountIdleWriter_WriteTouchesIdleDeadlineOnEveryWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var touches int32
	w := &mountIdleWriter{Conn: client, touch: func() { atomic.AddInt32(&touches, 1) }}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 3)
		for i := 0; i < 2; i++ {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := w.Write([]byte("def")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	<-done
	if got := atomic.LoadInt32(&touches); got != 2 {
		t.Fatalf("expected touch to run on every Write (2 writes), got %d calls", got)
	}
}
