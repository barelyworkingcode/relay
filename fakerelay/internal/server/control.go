package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// controlRoutes installs the endpoints T1 owns. GET /v1/state and the
// host and fs-event endpoints come from the domain packages through Control.
func (s *Server) controlRoutes() {
	s.Control("PUT /v1/presence", func(w http.ResponseWriter, r *http.Request) {
		var script map[string]string
		if err := decodeStrict(r, &script); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := s.gate.Set(script); err != nil {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.Control("POST /v1/faults", func(w http.ResponseWriter, r *http.Request) {
		var f world.Fault
		if err := decodeStrict(r, &f); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		id, err := s.faults.Add(f)
		if err != nil {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
	})
	s.Control("DELETE /v1/faults", func(w http.ResponseWriter, r *http.Request) {
		s.faults.Clear("")
		w.WriteHeader(http.StatusNoContent)
	})
	s.Control("DELETE /v1/faults/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.faults.Clear(r.PathValue("id")) {
			WriteError(w, http.StatusNotFound, "no such fault")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.Control("POST /v1/faults/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		if !s.faults.Release(r.PathValue("id")) {
			WriteError(w, http.StatusNotFound, "no such fault")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s.Control("GET /v1/clock", func(w http.ResponseWriter, r *http.Request) { s.writeClock(w) })
	s.Control("POST /v1/clock", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Set       string `json:"set"`
			AdvanceMS *int64 `json:"advance_ms"`
		}
		if err := decodeStrict(r, &body); err != nil || (body.Set == "") == (body.AdvanceMS == nil) {
			WriteError(w, http.StatusBadRequest, "send exactly one of set and advance_ms")
			return
		}
		if body.AdvanceMS != nil {
			s.clock.Advance(time.Duration(*body.AdvanceMS) * time.Millisecond)
		} else {
			t, err := time.Parse(time.RFC3339Nano, body.Set)
			if err != nil {
				WriteError(w, http.StatusBadRequest, "set must be an RFC 3339 time")
				return
			}
			s.clock.Set(t)
		}
		s.writeClock(w)
	})
	s.Control("POST /v1/verb", s.serveVerb)
	s.Control("GET /v1/follow", s.serveFollow)
}

func (s *Server) writeClock(w http.ResponseWriter) {
	WriteJSON(w, http.StatusOK, map[string]string{"now": s.clock.Now().UTC().Format(time.RFC3339Nano)})
}

func (s *Server) expired(expires string) bool {
	t, err := time.Parse(time.RFC3339, expires)
	return err != nil || !s.clock.Now().Before(t)
}

type verbRequest struct {
	Argv  []string `json:"argv"`
	Trace string   `json:"trace"`
	Body  []byte   `json:"body"`
}

type verbResponse struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

func (s *Server) serveVerb(w http.ResponseWriter, r *http.Request) {
	var req verbRequest
	if err := decodeStrict(r, &req); err != nil || len(req.Argv) == 0 {
		WriteError(w, http.StatusBadRequest, "send {argv, trace, body}")
		return
	}
	s.mu.Lock()
	name, h := "", VerbHandler(nil)
	for n := 2; n >= 1 && h == nil; n-- {
		if len(req.Argv) >= n {
			name = strings.Join(req.Argv[:n], " ")
			h = s.verbs[name]
		}
	}
	s.mu.Unlock()
	if h == nil {
		WriteJSON(w, http.StatusOK, verbResponse{Code: 2, Stderr: "error: fakerelay does not implement `" + strings.Join(req.Argv, " ") + "`; see docs/fakerelay.md\n"})
		return
	}
	ctx := events.WithTrace(r.Context(), req.Trace)
	ctx = withCaller(ctx, Caller{Kind: "operator", CredID: "cli"})
	ctx = WithVerbBody(ctx, req.Body)
	res := h(ctx, req.Argv[len(strings.Fields(name)):])
	WriteJSON(w, http.StatusOK, verbResponse{Code: res.Code, Stdout: string(res.Stdout), Stderr: string(res.Stderr)})
}

// serveFollow streams one NDJSON notice per log append. The subscription is
// made before the headers go out, so a client that has its response has
// missed nothing after that point.
func (s *Server) serveFollow(w http.ResponseWriter, r *http.Request) {
	ch, cancel := s.log.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl := http.NewResponseController(w)
	_ = fl.Flush()
	for {
		select {
		case n := <-ch:
			var buf bytes.Buffer
			_ = json.NewEncoder(&buf).Encode(n)
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			_ = fl.Flush()
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
}
