package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"relaygo/jsonrpc"
)

// FrameConn is the newline-delimited-JSON plumbing both listeners sit on: the
// scanner setup, the serialized write path, the read loop, and the end-of-
// stream classification. It exists so that BridgeServer and RemoteServer share
// the *transport*, and nothing else.
//
// That split is the point. ADR-010 decision 1 wants the two dispatch tables
// visibly distinct — a request type reachable from a VM must be a line someone
// deliberately added to a two-entry list — while the framing underneath has no
// security content at all and is exactly the code that rots when it is copied.
// Serve therefore takes the per-line handler as a parameter and knows nothing
// about request types, tokens, or identity.
type FrameConn struct {
	conn    net.Conn
	scanner *bufio.Scanner
	// name labels this connection in logs ("bridge", "remote"), so a read
	// error says which listener it came from without threading a logger.
	name string
	// idle is the inactivity timeout applied to reads and writes. Zero means
	// no deadlines at all, which is what the Unix socket uses: the peer there
	// is same-user and a stalled local client is not an attack. On a network
	// listener a peer that opens a connection and never speaks is exactly the
	// slowloris vector decision 9 calls out, so RemoteServer sets it.
	idle time.Duration

	// writeMu serializes writes: progress frames are emitted from the
	// external-MCP reader goroutine while the main goroutine is blocked inside
	// the in-flight call, so the terminal response and any progress frames
	// would otherwise race on conn.Write.
	writeMu sync.Mutex
}

// NewFrameConn wraps an accepted connection. idle <= 0 disables deadlines.
func NewFrameConn(conn net.Conn, name string, idle time.Duration) *FrameConn {
	c := &FrameConn{conn: conn, scanner: NewScanner(conn), name: name, idle: idle}
	c.touch()
	return c
}

// touch pushes the read/write deadline out by the idle timeout. Called after
// every frame read and before every frame written, which makes the timeout an
// INACTIVITY bound rather than a cap on how long a call may take — the same
// treatment bridge/client.go gives its own deadline, and for the same reason: a
// tool call that legitimately runs for minutes (LLM inference, a large mail
// search) must not be severed just because it is slow.
func (c *FrameConn) touch() {
	if c.idle <= 0 {
		return
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.idle))
}

// WriteFrame marshals and writes one response frame, serialized against every
// other writer on this connection.
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

// Serve reads frames until the peer disconnects, the connection errors, or a
// write fails, invoking handle for each line and writing what it returns. Each
// call gets a context carrying a progress sink so a long-running CallTool can
// stream RespProgress frames before its terminal response; handlers that don't
// stream ignore it.
func (c *FrameConn) Serve(ctx context.Context, handle func(ctx context.Context, line string) BridgeResponse) {
	for c.scanner.Scan() {
		c.touch()
		line := c.scanner.Text()
		reqCtx := WithProgress(ctx, func(u ProgressUpdate) {
			_ = c.WriteFrame(BridgeResponse{Type: RespProgress, Progress: &u})
		})
		if err := c.WriteFrame(handle(reqCtx, line)); err != nil {
			return
		}
	}
	c.reportReadEnd(ctx)
}

// reportReadEnd classifies why the read loop ended. An oversized line is told
// to the peer before the connection drops — the scanner cannot resync past it,
// so without this the client sees only a generic read failure.
func (c *FrameConn) reportReadEnd(ctx context.Context) {
	switch err := c.scanner.Err(); {
	case err == nil:
		// Clean EOF: peer closed the connection. Nothing to report.
	case errors.Is(err, bufio.ErrTooLong):
		_ = c.WriteFrame(ErrorResponse(jsonrpc.CodeInvalidParams, fmt.Sprintf("message exceeds maximum size of %d bytes", MaxMessageSize)))
		slog.Warn(c.name+": dropping connection, message exceeds size limit", "max_bytes", MaxMessageSize)
	case ctx.Err() != nil:
		// Connection was closed by shutdown (the close-on-cancel goroutine in
		// each server's handler); the resulting read error is expected.
		slog.Debug(c.name+" connection closed during shutdown", "error", err)
	default:
		slog.Warn(c.name+" connection read error", "error", err)
	}
}

// ErrorResponse builds an error frame with the given JSON-RPC code.
func ErrorResponse(code int, msg string) BridgeResponse {
	return BridgeResponse{Type: RespError, Code: code, Message: msg}
}

// ErrorCode extracts a JSON-RPC error code from an error chain. Router methods
// wrap auth/permission errors with jsonrpc.CodedError so a listener can
// classify them without fragile string matching; anything else is internal.
func ErrorCode(err error) int {
	var coded *jsonrpc.CodedError
	if errors.As(err, &coded) {
		return coded.RPCCode
	}
	return jsonrpc.CodeInternalError
}
