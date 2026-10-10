package harness

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 fixes SHA-1 for the accept key; it is not a security use here.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// WSConn is a WebSocket client connection (RFC 6455, text frames, client
// masking) over the frontend socket.
type WSConn struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
}

const (
	wsGUID          = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsHandshakeWait = 60 * time.Second
	wsMaxFrame      = 16 << 20

	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// WebSocket opens path on ready.json sockets.frontend with c's bearer token and
// any ReqOpts headers (X-Relay-Scope, X-Trace-Id). It fails t unless relay
// answers 101.
func (i *Instance) WebSocket(path string, c Credential, o ...ReqOpts) *WSConn {
	i.t.Helper()
	sock := i.Ready.Sockets["frontend"]
	if sock == "" {
		i.t.Fatalf("ready.json has no sockets.frontend")
	}
	conn, err := net.DialTimeout("unix", sock, wsHandshakeWait)
	if err != nil {
		i.t.Fatalf("dialing the frontend socket: %v", err)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		_ = conn.Close()
		i.t.Fatalf("reading random bytes for the WebSocket key: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	req, err := http.NewRequest("GET", "http://relay"+path, nil)
	if err != nil {
		_ = conn.Close()
		i.t.Fatalf("building the WebSocket request for %s: %v", path, err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for _, opt := range o {
		for k, vs := range opt.Header {
			req.Header[k] = vs
		}
		if opt.Trace != "" {
			req.Header.Set("X-Trace-Id", opt.Trace)
		}
	}
	if req.Header.Get("X-Trace-Id") == "" {
		req.Header.Set("X-Trace-Id", NewTrace(i.t))
	}
	if err := conn.SetDeadline(time.Now().Add(wsHandshakeWait)); err != nil {
		_ = conn.Close()
		i.t.Fatalf("setting the WebSocket handshake deadline: %v", err)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		i.t.Fatalf("writing the WebSocket handshake for %s: %v", path, err)
	}
	rd := bufio.NewReaderSize(conn, 64*1024)
	resp, err := http.ReadResponse(rd, req)
	if err != nil {
		_ = conn.Close()
		i.t.Fatalf("reading the WebSocket handshake answer for %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		_ = resp.Body.Close()
		_ = conn.Close()
		i.t.Fatalf("WebSocket %s answered %d, want 101\n%s", path, resp.StatusCode, body)
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the import
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), base64.StdEncoding.EncodeToString(sum[:]); got != want {
		_ = conn.Close()
		i.t.Fatalf("WebSocket %s answered a wrong Sec-WebSocket-Accept", path)
	}
	_ = conn.SetDeadline(time.Time{})
	w := &WSConn{t: i.t, conn: conn, rd: rd}
	i.t.Cleanup(w.Close)
	return w
}

// Send writes v as one masked text frame, JSON-encoded unless it is already
// json.RawMessage or []byte.
func (w *WSConn) Send(v any) {
	w.t.Helper()
	var payload []byte
	switch b := v.(type) {
	case json.RawMessage:
		payload = b
	case []byte:
		payload = b
	default:
		enc, err := json.Marshal(v)
		if err != nil {
			w.t.Fatalf("encoding the WebSocket frame: %v", err)
		}
		payload = enc
	}
	if err := w.writeFrame(wsOpText, payload); err != nil {
		w.t.Fatalf("writing a WebSocket frame: %v", err)
	}
}

func (w *WSConn) writeFrame(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(hdr[len(hdr)-2:], uint16(n))
	default:
		hdr = append(hdr, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[len(hdr)-8:], uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsHandshakeWait))
	_, err := w.conn.Write(append(append(hdr, mask...), masked...))
	return err
}

// Next returns the next text frame. Pings are answered and skipped. It fails t
// when the deadline passes or relay closes the connection first.
func (w *WSConn) Next(deadline time.Duration) json.RawMessage {
	w.t.Helper()
	if err := w.conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		w.t.Fatalf("setting the WebSocket read deadline: %v", err)
	}
	var message []byte
	for {
		op, fin, payload, err := w.readFrame()
		if err != nil {
			w.t.Fatalf("no WebSocket text frame within %s: %v", deadline, err)
		}
		switch op {
		case wsOpPing:
			_ = w.writeFrame(wsOpPong, payload)
		case wsOpPong:
		case wsOpClose:
			w.t.Fatalf("relay closed the WebSocket before a text frame arrived")
		case wsOpText, wsOpContinuation:
			message = append(message, payload...)
			if fin {
				return json.RawMessage(message)
			}
		default:
			w.t.Fatalf("unexpected WebSocket opcode %#x", op)
		}
	}
}

func (w *WSConn) readFrame() (op byte, fin bool, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.rd, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.rd, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.rd, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > wsMaxFrame {
		err = fmt.Errorf("a WebSocket frame of %d bytes exceeds the %d byte limit", n, wsMaxFrame)
		return
	}
	var mask [4]byte
	masked := h[1]&0x80 != 0
	if masked {
		if _, err = io.ReadFull(w.rd, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.rd, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close sends a close frame and closes the connection. It is idempotent.
func (w *WSConn) Close() {
	_ = w.writeFrame(wsOpClose, []byte{0x03, 0xE8})
	_ = w.conn.Close()
}
