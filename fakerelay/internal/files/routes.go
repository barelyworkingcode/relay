package files

import (
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

// stream serves a file as raw bytes. A console project honours Range; a host
// project answers a plain chunked 200 and ignores it.
func (s *service) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := s.d.Events.Begin(r.Context(), "file.stream").Set("project_id", id)
	fail := func(e *fileErr) {
		status, reason := eventOutcome(e)
		ev.End(status, reason, e)
		writeFileErr(w, e)
	}
	t, e := s.target(id)
	if e != nil {
		fail(e)
		return
	}
	c := &call{s: s, r: r, t: t, tool: "stream"}
	rel, e := c.path(r.URL.Query().Get("path"))
	if e != nil {
		fail(e)
		return
	}
	p, e := c.abs(rel, errSymlink)
	if e != nil {
		fail(e)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(osErr(err))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(osErr(err))
		return
	}
	if fi.IsDir() {
		fail(ferr(400, "EISDIR", "illegal operation on a directory"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if t.hostID == "" {
		http.ServeContent(w, r, "", time.Time{}, f)
		ev.End("ok", "", nil)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	_, err = io.Copy(w, f)
	if err != nil {
		ev.End("error", "internal", err)
		return
	}
	ev.End("ok", "", nil)
}

var pasteName = regexp.MustCompile(`^eve-paste-[0-9]+-[0-9a-f]+\.(png|jpg|gif|webp)$`)

// pastetmp writes a pasted image under the host's temp directory.
func (s *service) pastetmp(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("id")
	ev := s.d.Events.Begin(r.Context(), "host.pastetmp").Set("host_id", hostID)
	c := &call{s: s, r: r, tool: "pastetmp", mutates: true, t: &target{hostID: hostID}}
	resp, e := s.paste(c, w)
	if e != nil {
		status, reason := eventOutcome(e)
		ev.End(status, reason, e)
		writeFileErr(w, e)
		return
	}
	ev.End("ok", "", nil)
	server.WriteJSON(w, http.StatusOK, resp)
}

func (s *service) paste(c *call, w http.ResponseWriter) (any, *fileErr) {
	root := s.hub.hostRoot(c.t.hostID)
	body, e := readBody(w, c.r, largeBody)
	if e != nil {
		return nil, e
	}
	c.body = body
	var req struct {
		Name    string `json:"name"`
		DataB64 string `json:"data_b64"`
	}
	if e := c.decode(&req); e != nil {
		return nil, e
	}
	if !pasteName.MatchString(req.Name) {
		return nil, errInvalid("invalid paste file name")
	}
	data, err := base64.StdEncoding.DecodeString(req.DataB64)
	if err != nil {
		return nil, errInvalid("data_b64 is not base64")
	}
	if len(data) > maxFileBytes {
		return nil, errTooLarge("File too large", int64(len(data)))
	}
	if root == "" {
		return nil, ferr(404, "HOST_NOT_FOUND", "host not found")
	}
	args := map[string]any{"host_id": c.t.hostID, "name": req.Name, "bytes": len(data)}
	return c.mutatePaste(args, func() (any, *fileErr) {
		dir := filepath.Join(root, "tmp")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, osErr(err)
		}
		if err := os.WriteFile(filepath.Join(dir, req.Name), data, 0o644); err != nil {
			return nil, osErr(err)
		}
		return map[string]string{"path": "/tmp/" + req.Name}, nil
	})
}

// mutatePaste is mutate for a route with no project: no read-only check and
// no containment, and the audit rows carry no project fields.
func (c *call) mutatePaste(args map[string]any, run func() (any, *fileErr)) (any, *fileErr) {
	id, start, err := c.s.intent(c, args)
	if err != nil {
		return nil, errAudit
	}
	e := c.s.hub.ensure(c.t)
	var resp any
	if e == nil {
		resp, e = run()
	}
	c.s.completion(c, id, start, args, e)
	return resp, e
}
