package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterPersistentSessionRoutes serves a hosted project's persistent
// terminals (docs/ssh-hosts.md, Persistent terminals). Listing is read
// class; killing ends remote work, so it is configure, like the host record.
func RegisterPersistentSessionRoutes(rr *control.RouteRegistrar, ops *PersistentSessionOps) {
	rr.Handle(control.ClassRead, "GET /api/projects/{id}/persistent-sessions", func(w http.ResponseWriter, r *http.Request) {
		sessions, err := ops.List(r.Context(), r.PathValue("id"))
		if err != nil {
			writePersistSessionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sessions)
	})

	rr.Handle(control.ClassConfigure, "DELETE /api/projects/{id}/persistent-sessions/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.Kill(r.Context(), r.PathValue("id"), r.PathValue("name")); err != nil {
			writePersistSessionError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writePersistSessionError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errPersistProjectNotFound), errors.Is(err, errPersistSessionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errPersistNameInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, errPersistNoTmux):
		status = http.StatusConflict
	case errors.Is(err, errPersistHostUnreachable):
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// liveTerminalNamesTimeout bounds the relay-sessions round trip behind
// attached_here; it runs inside a list request that already waits on ssh.
const liveTerminalNamesTimeout = 5 * time.Second

// liveTerminalNamesFn builds PersistentSessionOps.ActiveNames from the
// session host. It ignores projectID: a persist terminal's name already
// carries its project's prefix, and List only looks up names of its own
// project. An unwired or unreachable session host reads as nothing attached
// here, which leaves the list itself intact.
func liveTerminalNamesFn(d sessionRouteDeps) func(projectID string) map[string]bool {
	if !d.ready() {
		return nil
	}
	host := d.host()
	return func(string) map[string]bool {
		ctx, cancel := context.WithTimeout(context.Background(), liveTerminalNamesTimeout)
		defer cancel()
		names, err := host.LiveTerminalNames(ctx)
		if err != nil {
			slog.Warn("persistent sessions: live terminal list failed", "error", err)
			return nil
		}
		return names
	}
}
