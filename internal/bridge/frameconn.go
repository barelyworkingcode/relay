package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

// FrameConn is the newline-delimited-JSON transport shared by BridgeServer,
// RemoteServer and EnrolmentRequestServer, and nothing else: ADR-010
// decision 1 wants dispatch tables visibly distinct — a request type
// reachable from a VM must be a line someone deliberately added to a
// two-entry list — while the framing underneath has no security content and
// is exactly the code that rots when copied. Serve takes the per-line
// handler as a parameter and knows nothing about request types, tokens, or
// identity.
type FrameConn struct {
	conn    net.Conn
	scanner *bufio.Scanner
	name    string

	// idle is the inactivity timeout applied to reads and writes. Zero means
	// no deadlines, which is what the Unix socket uses: the peer there is
	// same-user and a stalled local client is not an attack. On a network
	// listener a peer that opens a connection and never speaks is exactly the
	// slowloris vector decision 9 calls out, so RemoteServer sets it.
	idle time.Duration

	// maxMessage is the frame-size ceiling this connection was built with —
	// MaxMessageSize by default, or a caller-chosen smaller value from
	// NewFrameConnWithLimit. Recorded per-instance, not read from the
	// package constant at report time, so reportReadEnd's message names the
	// bound that actually applied rather than always naming the tool
	// plane's 10 MiB ceiling.
	maxMessage int

	// writeMu serializes writes: progress frames are emitted from the
	// external-MCP reader goroutine while the main goroutine is blocked inside
	// the in-flight call, so the terminal response and any progress frames
	// would otherwise race on conn.Write.
	writeMu sync.Mutex
}

// NewFrameConn wraps an accepted connection. idle <= 0 disables deadlines.
func NewFrameConn(conn net.Conn, name string, idle time.Duration) *FrameConn {
	return newFrameConn(conn, name, idle, MaxMessageSize)
}

// NewFrameConnWithLimit is NewFrameConn with a caller-chosen frame-size
// ceiling smaller than MaxMessageSize. The enrolment-request listener uses
// this to refuse an oversized frame at the scanner itself — before a CSR's
// PEM bytes are even handed to a request handler — rather than sharing the
// tool-plane listeners' 10 MiB ceiling, which is sized for legitimate tool
// results, not an unauthenticated peer's opening frame.
func NewFrameConnWithLimit(conn net.Conn, name string, idle time.Duration, maxMessageBytes int) *FrameConn {
	return newFrameConn(conn, name, idle, maxMessageBytes)
}

func newFrameConn(conn net.Conn, name string, idle time.Duration, maxMessageBytes int) *FrameConn {
	scanner := bufio.NewScanner(conn)
	initial := 64 * 1024
	if maxMessageBytes < initial {
		initial = maxMessageBytes
	}
	scanner.Buffer(make([]byte, initial), maxMessageBytes)
	c := &FrameConn{conn: conn, scanner: scanner, name: name, idle: idle, maxMessage: maxMessageBytes}
	c.touch()
	return c
}

// touch pushes the deadline out by idle, treating it as an INACTIVITY bound
// rather than a cap on call duration — the same treatment bridge/client.go
// gives its own deadline, and for the same reason: a tool call that
// legitimately runs for minutes must not be severed just because it is slow.
func (c *FrameConn) touch() {
	if c.idle <= 0 {
		return
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.idle))
}

func (c *FrameConn) WriteFrame(resp BridgeResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(ErrorResponse(jsonrpc.CodeInternalError, err.Error()))
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.touch()
	_, werr := c.conn.Write(data)
	return werr
}

func (c *FrameConn) Serve(ctx context.Context, handle func(ctx context.Context, line string) BridgeResponse) {
	for c.scanner.Scan() {
		c.touch()
		line := c.scanner.Text()
		reqCtx := WithProgress(ctx, func(u ProgressUpdate) {
			_ = c.WriteFrame(BridgeResponse{Type: RespProgress, Progress: &u})
		})
		slot := &takeoverSlot{}
		reqCtx = context.WithValue(reqCtx, takeoverCtxKey{}, slot)
		err := c.WriteFrame(handle(reqCtx, line))
		if run := slot.fn; run != nil {
			if err != nil {
				// The handler already committed to a stream. Closing makes
				// the takeover's first read fail, so it cleans up rather
				// than waiting on a peer that never saw the ack.
				_ = c.conn.Close()
			}
			run(ctx, c)
			return
		}
		if err != nil {
			return
		}
	}
	c.reportReadEnd(ctx)
}

// takeoverSlot is per request and touched only by that request's handler and
// then by Serve, on one goroutine, so it needs no lock.
type takeoverSlot struct {
	fn func(ctx context.Context, fc *FrameConn)
}

type takeoverCtxKey struct{}

// SetTakeover is called by a handler that has decided the connection stops
// being request/response: after Serve writes the handler's response it runs fn
// on the same goroutine and returns when fn does, so the connection is never
// read by two loops. It reports false for a context that did not come from
// Serve. Only a handler that calls it changes anything; every other request
// takes exactly the path it always did.
func SetTakeover(ctx context.Context, fn func(ctx context.Context, fc *FrameConn)) bool {
	slot, ok := ctx.Value(takeoverCtxKey{}).(*takeoverSlot)
	if !ok || fn == nil {
		return false
	}
	slot.fn = fn
	return true
}

// WriteValue writes v as one newline-delimited JSON frame. It shares the write
// lock with WriteFrame, so a takeover may write from several goroutines.
func (c *FrameConn) WriteValue(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.touch()
	_, err = c.conn.Write(data)
	return err
}

// Close closes the underlying connection, which unblocks a ReadValue in
// progress.
func (c *FrameConn) Close() error { return c.conn.Close() }

// ReadValue reads the next frame into v, through the scanner Serve was using,
// so bytes it had already buffered are not lost. io.EOF means the peer closed.
func (c *FrameConn) ReadValue(v any) error {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	c.touch()
	return json.Unmarshal(c.scanner.Bytes(), v)
}

// reportReadEnd classifies why the read loop ended. An oversized line is told
// to the peer before the connection drops — the scanner cannot resync past
// it, so without this the client sees only a generic read failure.
func (c *FrameConn) reportReadEnd(ctx context.Context) {
	switch err := c.scanner.Err(); {
	case err == nil:
	case errors.Is(err, bufio.ErrTooLong):
		_ = c.WriteFrame(ErrorResponse(jsonrpc.CodeInvalidParams, fmt.Sprintf("message exceeds maximum size of %d bytes", c.maxMessage)))
		slog.Warn(c.name+": dropping connection, message exceeds size limit", "max_bytes", c.maxMessage)
	case ctx.Err() != nil:
		// Closed by shutdown (the close-on-cancel goroutine in each server's
		// handler); the resulting read error is expected, not a failure.
		slog.Debug(c.name+" connection closed during shutdown", "error", err)
	default:
		slog.Warn(c.name+" connection read error", "error", err)
	}
}

func ErrorResponse(code int, msg string) BridgeResponse {
	return BridgeResponse{Type: RespError, Code: code, Message: msg}
}

// ErrorCode extracts a JSON-RPC error code from an error chain. Router
// methods wrap auth/permission errors with jsonrpc.CodedError so a listener
// can classify them without fragile string matching.
func ErrorCode(err error) int {
	var coded *jsonrpc.CodedError
	if errors.As(err, &coded) {
		return coded.RPCCode
	}
	return jsonrpc.CodeInternalError
}
