package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// Request body caps. A write carries its content base64-encoded, so its cap
// is the 10 MiB limit plus the encoding's overhead and the JSON around it.
const (
	fileBodyLimit     int64 = 64 << 10
	fileWriteBodyMax  int64 = 16 << 20
	filePasteBodyMax  int64 = 16 << 20
	fileStreamChunkSz       = 64 << 10
)

// fileErrorStatus maps a file-plane code to its HTTP status.
func fileErrorStatus(code string) int {
	switch code {
	case projectfs.CodeProjectNotFound, projectfs.CodeHostNotFound, projectfs.CodeENOENT:
		return http.StatusNotFound
	case projectfs.CodeNotAvailable, projectfs.CodeTraversal, projectfs.CodeSymlink,
		projectfs.CodeReadOnly, projectfs.CodeEACCES:
		return http.StatusForbidden
	case projectfs.CodeInvalid, projectfs.CodeEISDIR, projectfs.CodeENOTDIR:
		return http.StatusBadRequest
	case projectfs.CodeEEXIST:
		return http.StatusConflict
	case projectfs.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case projectfs.CodeHostUnreachable, projectfs.CodeAuditUnavailable:
		return http.StatusServiceUnavailable
	case projectfs.CodeTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// writeFileError answers with the contract's error body. An error that is not
// a *projectfs.Error is an internal failure whose text stays in the log.
func writeFileError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *projectfs.Error
	if !errors.As(err, &fe) {
		slog.ErrorContext(r.Context(), "files: operation failed", "path", r.URL.Path, "error", err.Error())
		fe = projectfs.Errf(projectfs.CodeError, "file operation failed")
	}
	body := map[string]any{"error": fe.Msg, "code": fe.Code}
	if fe.Code == projectfs.CodeTooLarge && fe.Size > 0 {
		body["size"] = fe.Size
	}
	writeJSON(w, fileErrorStatus(fe.Code), body)
}

// decodeFileBody reads a JSON body under limit. It answers the error itself
// and reports false when the request is unusable.
func decodeFileBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			e := &projectfs.Error{Code: projectfs.CodeTooLarge, Msg: "request body too large"}
			if r.ContentLength > 0 {
				e.Size = r.ContentLength
			}
			writeFileError(w, r, e)
			return false
		}
		writeFileError(w, r, projectfs.Errf(projectfs.CodeInvalid, "invalid JSON body"))
		return false
	}
	return true
}

func fileActor(o *FileOps, r *http.Request) audit.AuditActor {
	return callerAuditActor(resolveLaunchCaller(r, o.Store))
}

// RegisterFileRoutes registers every file route as an execute-class route:
// the loopback TCP listener serves none of them. Each route is a literal so
// the route inventory and eve's pins can match it by text.
func RegisterFileRoutes(rr *control.RouteRegistrar, o *FileOps) {
	if o == nil {
		return
	}
	// session opens the project for a request and answers the refusal itself.
	session := func(w http.ResponseWriter, r *http.Request) *fileSession {
		fs, err := o.open(fileActor(o, r), r.PathValue("id"))
		if err != nil {
			writeFileError(w, r, err)
			return nil
		}
		return fs
	}

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/list", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path       string `json:"path"`
			ShowHidden bool   `json:"show_hidden"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		entries, err := fs.List(r.Context(), body.Path, body.ShowHidden)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/stat", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		info, err := fs.Stat(r.Context(), body.Path)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path     string `json:"path"`
			MaxBytes int64  `json:"max_bytes"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		content, size, err := fs.Read(r.Context(), body.Path, body.MaxBytes)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"content": content, "size": size})
	})

	rr.Handle(control.ClassExecute, "GET /api/projects/{id}/files/stream", func(w http.ResponseWriter, r *http.Request) {
		fs := session(w, r)
		if fs == nil {
			return
		}
		rc, _, err := fs.Open(r.Context(), r.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		// A console file is an *os.File, so Range, Content-Length and
		// Accept-Ranges come from the standard library. A host body is a
		// forward-only stream and goes out chunked.
		if f, ok := rc.(*os.File); ok {
			http.ServeContent(w, r, "", time.Time{}, f)
			return
		}
		buf := make([]byte, fileStreamChunkSz)
		// Read the first chunk before the header goes out, so an agent error
		// still reaches the client as an error.
		n, rerr := rc.Read(buf)
		if n == 0 && rerr != nil && rerr != io.EOF {
			writeFileError(w, r, rerr)
			return
		}
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for {
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
			}
			if rerr != nil {
				return
			}
			n, rerr = rc.Read(buf)
		}
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/write", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path       string `json:"path"`
			Content    string `json:"content"`
			Encoding   string `json:"encoding"`
			CreateOnly bool   `json:"create_only"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileWriteBodyMax, &body) {
			return
		}
		data := []byte(body.Content)
		switch body.Encoding {
		case "", "utf8":
			body.Encoding = "utf8"
		case "base64":
			var err error
			if data, err = base64.StdEncoding.DecodeString(body.Content); err != nil {
				writeFileError(w, r, projectfs.Errf(projectfs.CodeInvalid, "content is not base64"))
				return
			}
		default:
			writeFileError(w, r, projectfs.Errf(projectfs.CodeInvalid, "encoding must be utf8 or base64"))
			return
		}
		rel, err := fs.Write(r.Context(), body.Path, data, body.Encoding, body.CreateOnly)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": rel})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		rel, err := fs.Mkdir(r.Context(), body.Parent, body.Name)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": rel})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/rename", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path    string `json:"path"`
			NewName string `json:"new_name"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		rel, err := fs.Rename(r.Context(), body.Path, body.NewName)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": rel})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/move", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path    string `json:"path"`
			DestDir string `json:"dest_dir"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		rel, err := fs.Move(r.Context(), body.Path, body.DestDir)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": rel})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		trashed, err := fs.Delete(r.Context(), body.Path)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"trashed": trashed})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/search", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query         string   `json:"query"`
			Regex         bool     `json:"regex"`
			Word          bool     `json:"word"`
			CaseSensitive *bool    `json:"case_sensitive"`
			Globs         []string `json:"globs"`
			MaxMatches    int      `json:"max_matches"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		matches, truncated, err := fs.Search(r.Context(), projectfs.SearchOpts{
			Query: body.Query, Regex: body.Regex, Word: body.Word, CaseSensitive: body.CaseSensitive,
			Globs: body.Globs, MaxMatches: body.MaxMatches,
		})
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"matches": matches, "truncated": truncated})
	})

	rr.Handle(control.ClassExecute, "POST /api/projects/{id}/files/git", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cwd      string   `json:"cwd"`
			Args     []string `json:"args"`
			MaxBytes int64    `json:"max_bytes"`
		}
		fs := session(w, r)
		if fs == nil {
			return
		}
		if !decodeFileBody(w, r, fileBodyLimit, &body) {
			return
		}
		res, err := fs.Git(r.Context(), body.Cwd, body.Args, body.MaxBytes)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"exit_code":  res.ExitCode,
			"stdout_b64": base64.StdEncoding.EncodeToString(res.Stdout),
			"stderr":     res.Stderr,
		})
	})

	rr.Handle(control.ClassExecute, "POST /api/hosts/{id}/pastetmp", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    string `json:"name"`
			DataB64 string `json:"data_b64"`
		}
		if !decodeFileBody(w, r, filePasteBodyMax, &body) {
			return
		}
		p, err := o.PasteTmp(r.Context(), fileActor(o, r), r.PathValue("id"), body.Name, body.DataB64)
		if err != nil {
			writeFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": p})
	})

	rr.Handle(control.ClassExecute, "GET /ws/files", func(w http.ResponseWriter, r *http.Request) {
		o.serveFilesWS(w, r)
	})
}
