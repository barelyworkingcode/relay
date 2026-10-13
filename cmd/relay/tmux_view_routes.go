package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
)

func RegisterTmuxViewRoutes(rr *control.RouteRegistrar, ops *TmuxViewOps) {
	rr.Handle(control.ClassRead, "POST /api/tmux/views", func(w http.ResponseWriter, r *http.Request) {
		ev := logging.BeginEvent(r.Context(), "session.outside.view")
		var req tmuxViewRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&req)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = io.ErrUnexpectedEOF
			}
		}
		if err != nil {
			err = viewError(http.StatusBadRequest, "invalid_request", "invalid viewer request")
			endEvent(ev, err)
			writeTmuxViewError(w, err)
			return
		}
		body, err := ops.Create(r.Context(), req)
		endEvent(ev, err)
		if err != nil {
			writeTmuxViewError(w, err)
			return
		}
		writeCreatedBody(w, body)
	})
}

func writeTmuxViewError(w http.ResponseWriter, err error) {
	e, ok := err.(*tmuxViewError)
	if !ok {
		e = &tmuxViewError{http.StatusBadGateway, "session_host_unavailable", "viewer unavailable"}
	}
	writeJSON(w, e.status, map[string]string{"error": e.code, "message": e.message})
}
