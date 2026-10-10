package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const (
	smallBody    = 64 << 10
	largeBody    = 16 << 20
	maxFileBytes = 10 << 20
)

// fileErr is the coded file error. Size is set on TOO_LARGE when known.
type fileErr struct {
	Status int
	Code   string
	Msg    string
	Size   int64
}

func (e *fileErr) Error() string { return e.Code + ": " + e.Msg }

func ferr(status int, code, msg string) *fileErr {
	return &fileErr{Status: status, Code: code, Msg: msg}
}

func errInvalid(msg string) *fileErr { return ferr(400, "INVALID", msg) }

func errTooLarge(msg string, size int64) *fileErr {
	e := ferr(413, "TOO_LARGE", msg)
	e.Size = size
	return e
}

var (
	errTraversal = ferr(403, "TRAVERSAL", "Path traversal not allowed")
	errSymlink   = ferr(403, "SYMLINK", "Symbolic links are not opened")
	errReadOnly  = ferr(403, "READ_ONLY", "This project is read-only")
	errHostDown  = ferr(503, "HOST_UNREACHABLE", "host is not connected")
	errAudit     = ferr(503, "AUDIT_UNAVAILABLE", "audit log unavailable; the change was not made")
	errTimeout   = ferr(504, "TIMEOUT", "timed out")
)

// osErr maps an OS error to its coded file error, worded as relay words it:
// the console's file plane and the host agent differ on a missing path.
func (c *call) osErr(err error) *fileErr { return osErrFor(c.t != nil && c.t.hostID != "", err) }

func osErrFor(host bool, err error) *fileErr {
	var fe *fileErr
	switch {
	case errors.As(err, &fe):
		return fe
	case errors.Is(err, fs.ErrNotExist):
		if host {
			return ferr(404, "ENOENT", "No such file or directory")
		}
		return ferr(404, "ENOENT", "Not found")
	case errors.Is(err, fs.ErrPermission):
		return ferr(403, "EACCES", "Permission denied")
	case errors.Is(err, fs.ErrExist):
		return ferr(409, "EEXIST", "Already exists")
	case errors.Is(err, syscall.EISDIR):
		return ferr(400, "EISDIR", "Path is a directory")
	case errors.Is(err, syscall.ENOTDIR):
		return ferr(400, "ENOTDIR", "Not a directory")
	}
	return ferr(500, "ERROR", "file operation failed")
}

func writeFileErr(w http.ResponseWriter, e *fileErr) {
	body := map[string]any{"error": e.Msg, "code": e.Code}
	if e.Code == "TOO_LARGE" && e.Size > 0 {
		body["size"] = e.Size
	}
	server.WriteJSON(w, e.Status, body)
}

// eventOutcome maps a file error to the event status and reason.
func eventOutcome(e *fileErr) (status, reason string) {
	switch e.Code {
	case "READ_ONLY":
		return "denied", "read_only"
	case "SYMLINK":
		return "denied", "symlink"
	case "AUDIT_UNAVAILABLE":
		return "denied", "audit_unavailable"
	case "EACCES":
		return "denied", "not_granted"
	case "PROJECT_NOT_FOUND", "HOST_NOT_FOUND", "ENOENT":
		return "error", "not_found"
	case "EEXIST":
		return "error", "conflict"
	case "HOST_UNREACHABLE":
		return "error", "unavailable"
	case "TIMEOUT":
		return "error", "timeout"
	case "INVALID", "TRAVERSAL", "NOT_AVAILABLE", "TOO_LARGE", "EISDIR", "ENOTDIR":
		return "error", "invalid"
	}
	return "error", "internal"
}

// target is a project as the file plane sees it, read fresh on every request.
type target struct {
	p      world.Project
	root   string
	hostID string
}

func (s *service) target(id string) (*target, *fileErr) {
	var t *target
	s.d.State.Read(func(m *state.Model) {
		for _, p := range m.Projects {
			if p.ID == id {
				t = &target{p: p, root: state.RootOf(m, p), hostID: p.HostID}
			}
		}
	})
	if t == nil {
		return nil, ferr(404, "PROJECT_NOT_FOUND", "project not found")
	}
	if t.p.Kind == "remote" || t.p.Path == "" {
		return nil, ferr(403, "NOT_AVAILABLE", "files are not available for this project")
	}
	return t, nil
}

// call is one file request in flight.
type call struct {
	s       *service
	r       *http.Request
	t       *target
	tool    string
	mutates bool
	body    []byte
	hostOK  bool
	effects []fsEvent
}

// decode reads the JSON request body into v.
func (c *call) decode(v any) *fileErr {
	if err := json.Unmarshal(c.body, v); err != nil {
		return errInvalid("invalid JSON body")
	}
	return nil
}

// path cleans a request path. On a mutating op a traversal also writes the
// refusal row, with the path as sent.
func (c *call) path(p string) (string, *fileErr) {
	rel, e := cleanRel(p)
	if e == errTraversal && c.mutates {
		c.refuse(e, map[string]any{"path": p, "host_id": c.t.hostID})
	}
	return rel, e
}

// abs connects the host if needed, then resolves rel under the root. Symlinks
// refuse with SYMLINK, or ENOTDIR for list and search, which never follow one.
func (c *call) abs(rel string, symErr *fileErr) (string, *fileErr) {
	if e := c.s.hub.ensure(c.t); e != nil {
		return "", e
	}
	return resolve(c.t.root, rel, symErr)
}

// mutate runs a mutating op: read-only, containment, the intent row, the work
// and the completion row. checks are the root-relative paths to contain.
func (c *call) mutate(args map[string]any, checks []string, run func() (any, *fileErr)) (any, *fileErr) {
	if c.t.p.FilesReadOnly {
		c.refuse(errReadOnly, args)
		return nil, errReadOnly
	}
	for _, rel := range checks {
		if _, e := resolve(c.t.root, rel, errSymlink); e == errSymlink {
			c.refuse(e, args)
			return nil, e
		}
	}
	id, start, err := c.s.intent(c, args)
	if err != nil {
		return nil, errAudit
	}
	var resp any
	e := c.s.hub.ensure(c.t)
	if e == nil {
		resp, e = run()
	}
	c.s.completion(c, id, start, args, e)
	return resp, e
}

func (c *call) refuse(e *fileErr, args map[string]any) {
	c.s.refusal(c, args, e)
}

// handle wraps one POST file route: project, kind, body, the op, the event,
// the answer, then the fs_event for what the op changed.
func (s *service) handle(tool string, mutates bool, limit int64, op func(*call) (any, *fileErr)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ev := s.d.Events.Begin(r.Context(), "file."+tool).Set("project_id", id)
		t, e := s.target(id)
		var resp any
		if e == nil {
			c := &call{s: s, r: r, t: t, tool: tool, mutates: mutates}
			if c.body, e = readBody(w, r, limit); e == nil {
				resp, e = op(c)
			}
			defer func() { s.hub.emit(t.p.ID, c.effects, false) }()
		}
		if e != nil {
			status, reason := eventOutcome(e)
			ev.End(status, reason, e)
			writeFileErr(w, e)
			return
		}
		ev.End("ok", "", nil)
		server.WriteJSON(w, http.StatusOK, resp)
	}
}

// readBody reads a request body of at most limit bytes.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *fileErr) {
	if r.ContentLength > limit {
		return nil, errTooLarge("request body too large", r.ContentLength)
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return nil, errTooLarge("request body too large", 0)
	}
	if err != nil {
		return nil, errInvalid("invalid request body")
	}
	return b, nil
}

// fsEvent is one change a mutating op reports to watchers.
type fsEvent struct{ path, kind string }

func (s *service) now() time.Time { return s.d.Clock.Now() }

// actor is the audit actor for a file request.
func (s *service) actor(c *call) map[string]any {
	a := map[string]any{"kind": "control", "auth": "token", "cred_id": server.CallerFrom(c.r.Context()).CredID}
	if c.t.p.ID != "" {
		a["project_id"] = c.t.p.ID
		a["project_name"] = c.t.p.Name
	}
	return a
}

func (s *service) row(c *call, id, phase string, dur int64, args map[string]any, outcome string, e *fileErr) map[string]any {
	row := map[string]any{
		"id": id, "ts": s.now().UTC().Format("2006-01-02T15:04:05.000Z"), "dur_ms": dur,
		"event": "file_op", "actor": s.actor(c), "tool": c.tool, "args": args, "outcome": outcome, "scope": nil,
	}
	if b, err := json.Marshal(args); err == nil {
		row["args_bytes"] = len(b)
	}
	if c.t.p.ID != "" {
		row["mcp_root"] = c.t.p.Path
	}
	if phase != "" {
		row["phase"] = phase
	}
	if e != nil {
		row["error"] = e.Code
	}
	return row
}

// intent writes the pending row. A failure refuses the operation before it
// runs. With no audit log the operation runs and writes nothing.
func (s *service) intent(c *call, args map[string]any) (string, time.Time, error) {
	id, start := events.NewUUID(), time.Now()
	if s.d.Audit == nil {
		return id, start, nil
	}
	if err := s.d.Audit.Append(s.row(c, id, "intent", 0, args, "pending", nil)); err != nil {
		return "", start, fmt.Errorf("audit: %w", err)
	}
	return id, start, nil
}

func (s *service) completion(c *call, id string, start time.Time, args map[string]any, e *fileErr) {
	if s.d.Audit == nil {
		return
	}
	outcome := "ok"
	if e != nil {
		outcome = "error"
	}
	_ = s.d.Audit.Append(s.row(c, id, "completion", time.Since(start).Milliseconds(), args, outcome, e))
}

func (s *service) refusal(c *call, args map[string]any, e *fileErr) {
	if s.d.Audit == nil {
		return
	}
	_ = s.d.Audit.Append(s.row(c, events.NewUUID(), "", 0, args, "denied", e))
}

// cleanRel normalises a root-relative request path.
func cleanRel(p string) (string, *fileErr) {
	if strings.ContainsRune(p, 0) {
		return "", errInvalid("path contains a NUL byte")
	}
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			return "", errTraversal
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/"), nil
}

func validName(n string) *fileErr {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
		return errInvalid("invalid name")
	}
	return nil
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func statDir(p string) (os.FileInfo, error) {
	fi, err := os.Stat(p)
	if err == nil && !fi.IsDir() {
		return nil, syscall.ENOTDIR
	}
	return fi, err
}
