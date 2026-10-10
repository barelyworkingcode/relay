package files

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

// controlFSWrite writes a file the way a process outside relay would: it
// ignores files_read_only, writes no audit row or file.* event and does not
// connect a host agent. Containment still holds. It answers once the fs_event
// is on the wire of every watching connection.
func (s *service) controlFSWrite(w http.ResponseWriter, r *http.Request) {
	rel, data, t, e := s.externalWrite(w, r)
	if e != nil {
		writeFileErr(w, e)
		return
	}
	p, e := resolve(t.root, rel, errSymlink)
	if e != nil {
		writeFileErr(w, e)
		return
	}
	kind := "change"
	if _, err := os.Lstat(p); err != nil {
		kind = "rename"
	}
	host := t.hostID != ""
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		writeFileErr(w, osErrFor(host, err))
		return
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeFileErr(w, osErrFor(host, err))
		return
	}
	n := s.hub.emit(t.p.ID, []fsEvent{{rel, kind}}, true)
	server.WriteJSON(w, http.StatusOK, map[string]any{"path": rel, "kind": kind, "delivered": n})
}

// externalWrite checks the request in the order the file routes do: body,
// encoding, size, project, then the path.
func (s *service) externalWrite(w http.ResponseWriter, r *http.Request) (rel string, data []byte, t *target, e *fileErr) {
	body, e := readBody(w, r, largeBody)
	if e != nil {
		return "", nil, nil, e
	}
	var req struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if json.Unmarshal(body, &req) != nil || req.Path == "" {
		return "", nil, nil, errInvalid("send {path, content, encoding}")
	}
	data = []byte(req.Content)
	switch req.Encoding {
	case "", "utf8":
	case "base64":
		var err error
		if data, err = base64.StdEncoding.DecodeString(req.Content); err != nil {
			return "", nil, nil, errInvalid("content is not base64")
		}
	default:
		return "", nil, nil, errInvalid("encoding must be utf8 or base64")
	}
	if len(data) > maxFileBytes {
		return "", nil, nil, errTooLarge("File too large", int64(len(data)))
	}
	if t, e = s.target(r.PathValue("id")); e != nil {
		return "", nil, nil, e
	}
	if rel, e = cleanRel(req.Path); e != nil {
		return "", nil, nil, e
	}
	if rel == "" {
		return "", nil, nil, ferr(400, "EISDIR", "Path is a directory")
	}
	return rel, data, t, nil
}
