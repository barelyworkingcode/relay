package bridge

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"time"
)

// peerWatch watches for the peer closing its end while a handler runs, and
// cancels that request's ctx when it does. It only peeks, never consumes, so
// Serve stays the connection's single reader and the takeover seam never sees
// two. It applies to any connection whose net.Conn is a syscall.Conn: the
// Unix bridge and the raw-TCP enrolment-request listener, whose handler
// ignores its ctx, so a cancel there changes nothing. A tls.Conn (the remote
// listener) is not one, so there it is a no-op.
type peerWatch struct {
	fc   *FrameConn
	done chan struct{}
}

// startPeerWatch returns nil when the connection cannot be watched.
func (c *FrameConn) startPeerWatch(cancel context.CancelFunc) *peerWatch {
	sc, ok := c.conn.(syscall.Conn)
	if !ok {
		return nil
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil
	}
	w := &peerWatch{fc: c, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var buf [1]byte
		closed := false
		_ = raw.Read(func(fd uintptr) bool {
			n, _, rerr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
			switch {
			case errors.Is(rerr, syscall.EAGAIN):
				return false
			case rerr != nil:
				return true
			case n == 0:
				closed = true
				return true
			default:
				// A pipelined frame is waiting. Its bytes hide any later EOF
				// from a peek, so the watch ends here rather than spin.
				return true
			}
		})
		if closed {
			slog.Debug(c.name + ": peer disconnected, cancelling in-flight request")
			cancel()
		}
	}()
	return w
}

// stop ends the watch and restores the connection's read deadline, so the
// next Scan or a takeover run reads with the deadline it would have had.
func (w *peerWatch) stop() {
	if w == nil {
		return
	}
	_ = w.fc.conn.SetReadDeadline(time.Now())
	<-w.done
	if w.fc.idle <= 0 {
		_ = w.fc.conn.SetReadDeadline(time.Time{})
		return
	}
	w.fc.touch()
}
