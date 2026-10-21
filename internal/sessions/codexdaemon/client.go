// Package codexdaemon is relay's one client of the per-user codex app-server
// daemon. Two processes use it (the tray's discovery and the session host's
// provider), so the socket-safety rule and the method allowlist live here once.
//
// Relay attaches to a daemon it did not start. The package therefore sends
// nothing that changes the daemon: no settings, no stop, no restart. It
// refuses a socket another account could have replaced, and a method outside a
// short allowlist.
package codexdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// maxMessageBytes bounds one message read from the daemon.
const maxMessageBytes = 16 << 20

// closeWait bounds the courtesy close frame; the socket closes either way.
const closeWait = time.Second

var (
	// ErrNoDaemon means the default control socket does not exist or nobody
	// listens on it.
	ErrNoDaemon = errors.New("codexdaemon: no codex daemon is running")
	// ErrSocketUnsafe means the control socket or its folder could be replaced
	// by another account, so relay does not talk to it.
	ErrSocketUnsafe = errors.New("codexdaemon: the codex control socket is not private to this user")
	// ErrMethodNotAllowed means relay refused to send a method it has no reason
	// to send.
	ErrMethodNotAllowed = errors.New("codexdaemon: method is not on relay's allowlist")
)

// Identity is the daemon's lifetime evidence: a restarted daemon binds a new
// socket, so a changed identity means a different daemon.
type Identity struct {
	Physical string
	Dev, Ino uint64
	ModTime  time.Time
}

// Equal reports whether two identities name the same socket binding.
func (i Identity) Equal(o Identity) bool {
	return i.Physical == o.Physical && i.Dev == o.Dev && i.Ino == o.Ino && i.ModTime.Equal(o.ModTime)
}

// String is the identity as one stable token, for hashing and comparing.
func (i Identity) String() string {
	return fmt.Sprintf("%s|%d|%d|%d", i.Physical, i.Dev, i.Ino, i.ModTime.UnixNano())
}

// allowed is every request method relay sends. Deliberate: nothing here stops,
// restarts, updates, deletes or archives, and no method changes a setting.
var allowed = map[string]bool{
	"initialize":         true,
	"initialized":        true,
	"thread/loaded/list": true,
	"thread/read":        true,
	"thread/turns/list":  true,
	"thread/resume":      true,
	"turn/start":         true,
	"turn/interrupt":     true,
	"thread/unsubscribe": true,
}

// socketPath is the only socket relay reads.
func socketPath(home string) string {
	return filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
}

func owner(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func unsafe(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSocketUnsafe, fmt.Sprintf(format, args...))
}

// Resolve checks the default control socket under home and returns its
// identity. The link and its folder are owned by the effective uid. The target
// is a socket owned by the effective uid with no group or other permission
// bits, in a folder owned by the effective uid or root that no other account
// can write to (unless sticky).
func Resolve(home string) (Identity, error) {
	if !filepath.IsAbs(home) {
		return Identity{}, fmt.Errorf("%w: no home folder", ErrNoDaemon)
	}
	path := socketPath(home)
	euid := uint32(os.Geteuid())

	link, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return Identity{}, ErrNoDaemon
		}
		return Identity{}, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	if uid, ok := owner(link); !ok || uid != euid {
		return Identity{}, unsafe("the socket link is not owned by this user")
	}
	folder, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	if uid, ok := owner(folder); !ok || uid != euid {
		return Identity{}, unsafe("the socket folder is not owned by this user")
	}

	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	target, err := os.Stat(physical)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	uid, ok := owner(target)
	switch {
	case target.Mode()&os.ModeSocket == 0:
		return Identity{}, unsafe("the target is not a socket")
	case !ok || uid != euid:
		return Identity{}, unsafe("the socket is not owned by this user")
	case target.Mode().Perm()&0o077 != 0:
		return Identity{}, unsafe("the socket is open to group or others")
	}
	dir, err := os.Stat(filepath.Dir(physical))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	dirUID, ok := owner(dir)
	switch {
	case !ok || (dirUID != euid && dirUID != 0):
		return Identity{}, unsafe("the socket's folder belongs to another account")
	case dir.Mode().Perm()&0o022 != 0 && dir.Mode()&os.ModeSticky == 0:
		return Identity{}, unsafe("the socket's folder is writable by others")
	}
	st := target.Sys().(*syscall.Stat_t)
	return Identity{Physical: physical, Dev: uint64(st.Dev), Ino: uint64(st.Ino), ModTime: target.ModTime()}, nil
}

// Conn is one initialized connection to the daemon. Send is safe to call
// concurrently with Recv or Call; Recv and Call share the read side and
// serialize.
type Conn struct {
	ws   *websocket.Conn
	id   Identity
	next atomic.Int64

	wmu  sync.Mutex // serializes writes
	rmu  sync.Mutex // serializes reads
	once sync.Once
}

// Dial resolves the socket, upgrades on path "/" and runs the initialize
// handshake with exactly the client name and version, never a capability: a
// capability can change the daemon for later connections.
func Dial(ctx context.Context, home string) (*Conn, error) {
	id, err := Resolve(home)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", id.Physical)
		},
	}
	ws, _, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %v", ErrNoDaemon, err)
		}
		return nil, fmt.Errorf("codexdaemon: connect: %w", err)
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &Conn{ws: ws, id: id}

	// The path was checked before the dial; a different binding now is a
	// different daemon than the one that was judged.
	if again, err := Resolve(home); err != nil || !again.Equal(id) {
		_ = c.Close()
		return nil, unsafe("the socket changed while connecting")
	}
	if _, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "relay", "version": "1"},
	}); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Send(ctx, json.RawMessage(`{"method":"initialized"}`)); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Identity is the daemon identity this connection was judged against.
func (c *Conn) Identity() Identity { return c.id }

// checkLine refuses a line that carries a method outside the allowlist. A line
// with no method is a reply to a server request and passes.
func checkLine(line json.RawMessage) error {
	var head struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return fmt.Errorf("codexdaemon: not a JSON-RPC line: %w", err)
	}
	if head.Method != "" && !allowed[head.Method] {
		return fmt.Errorf("%w: %q", ErrMethodNotAllowed, head.Method)
	}
	return nil
}

// Send writes one line as one text frame.
func (c *Conn) Send(ctx context.Context, line json.RawMessage) error {
	if err := checkLine(line); err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.ws.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = c.ws.SetWriteDeadline(deadline)
	if err := c.ws.WriteMessage(websocket.TextMessage, line); err != nil {
		return fmt.Errorf("codexdaemon: send: %w", err)
	}
	return nil
}

// Recv returns the next line from the daemon. A context that ends makes the
// read fail, and a connection whose read failed is finished.
func (c *Conn) Recv(ctx context.Context) (json.RawMessage, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return c.recvLocked(ctx)
}

func (c *Conn) recvLocked(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.ws.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = c.ws.SetReadDeadline(deadline)
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("codexdaemon: receive: %w", err)
		}
		if kind == websocket.TextMessage {
			return json.RawMessage(data), nil
		}
	}
}

// Call sends one request and waits for the reply with its id. Notifications
// and requests the daemon sends meanwhile are dropped, so Call suits a short
// exchange on a connection nothing else reads.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if !allowed[method] {
		return nil, fmt.Errorf("%w: %q", ErrMethodNotAllowed, method)
	}
	id := c.next.Add(1)
	line, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{id, method, params})
	if err != nil {
		return nil, fmt.Errorf("codexdaemon: %s: %w", method, err)
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if err := c.Send(ctx, line); err != nil {
		return nil, err
	}
	want := strconv.FormatInt(id, 10)
	for {
		raw, err := c.recvLocked(ctx)
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.Method != "" || string(reply.ID) != want {
			continue
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("codexdaemon: %s: %s", method, reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// Close ends the connection and only the connection: the daemon and every
// thread in it keep running.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(closeWait))
		err = c.ws.Close()
	})
	return err
}
