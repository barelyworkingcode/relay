package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

type serviceView struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"display_name"`
	Command          string            `json:"command"`
	Args             []string          `json:"args"`
	Env              map[string]string `json:"env"`
	WorkingDir       string            `json:"working_dir,omitempty"`
	Autostart        bool              `json:"autostart"`
	URL              string            `json:"url,omitempty"`
	FrontendConsumer *bool             `json:"frontend_consumer,omitempty"`
	Running          bool              `json:"running"`
	// Set when the record was written but starting or restarting the process
	// failed. The mutation still succeeded, so this rides on a 2xx.
	ProcessError string `json:"process_error,omitempty"`
}

func serviceViewOf(c ServiceConfig, running bool) serviceView {
	return serviceView{
		ID:               c.ID,
		DisplayName:      c.DisplayName,
		Command:          c.Command,
		Args:             c.Args,
		Env:              revealEnvForUI(c.Env),
		WorkingDir:       c.WorkingDir,
		Autostart:        c.Autostart,
		URL:              c.URL,
		FrontendConsumer: c.FrontendConsumer,
		Running:          running,
	}
}

func serviceHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errServiceNotFound):
		return http.StatusNotFound
	case errors.Is(err, errServiceInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func withProcessError(v serviceView, err error) serviceView {
	if err != nil {
		v.ProcessError = err.Error()
	}
	return v
}

func writeServiceError(w http.ResponseWriter, err error) {
	writeJSON(w, serviceHTTPStatus(err), map[string]string{"error": err.Error()})
}

// ops is the same instance the Services tab's IPC handlers use, so a service
// started from curl and one started from the tray share validation and registry.
func RegisterServiceRoutes(rr *RouteRegistrar, ops *ServiceOps) {
	rr.Handle(ClassRead, "GET /api/services", func(w http.ResponseWriter, r *http.Request) {
		list := ops.List()
		out := make([]serviceView, 0, len(list))
		for _, c := range list {
			out = append(out, serviceViewOf(c, ops.Registry.IsRunning(c.ID)))
		}
		writeJSON(w, http.StatusOK, out)
	})

	rr.Handle(ClassRead, "GET /api/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		svc, err := ops.Get(r.PathValue("id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, serviceViewOf(svc, ops.Registry.IsRunning(svc.ID)))
	})

	// execute: the body's `command` field is what relay will run — the
	// caller chooses it, not relay (ADR-015 decision 1).
	rr.Handle(ClassExecute, "POST /api/services", func(w http.ResponseWriter, r *http.Request) {
		var body serviceFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		created, err := ops.Create(r.Context(), body, auditViaHTTP, credIDOf(r))
		if err != nil && !errors.Is(err, errServiceProcess) {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, withProcessError(serviceViewOf(created, ops.Registry.IsRunning(created.ID)), err))
	})

	// execute: same reasoning as create — Update can rewrite `command`.
	rr.Handle(ClassExecute, "PUT /api/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body serviceFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		updated, err := ops.Update(r.Context(), r.PathValue("id"), body, auditViaHTTP, credIDOf(r))
		if err != nil && !errors.Is(err, errServiceProcess) {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, withProcessError(serviceViewOf(updated, ops.Registry.IsRunning(updated.ID)), err))
	})

	rr.Handle(ClassConfigure, "DELETE /api/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.Remove(r.Context(), r.PathValue("id"), auditViaHTTP, credIDOf(r)); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// configure, not execute: this launches an already-configured command —
	// the operator chose what runs when they created or updated the record
	// (ADR-015 decision 1).
	rr.Handle(ClassConfigure, "POST /api/services/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ops.Start(id); err != nil {
			writeServiceError(w, err)
			return
		}
		svc, _ := ops.Get(id)
		writeJSON(w, http.StatusOK, serviceViewOf(svc, ops.Registry.IsRunning(id)))
	})

	rr.Handle(ClassConfigure, "POST /api/services/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := ops.Stop(id); err != nil {
			writeServiceError(w, err)
			return
		}
		svc, _ := ops.Get(id)
		writeJSON(w, http.StatusOK, serviceViewOf(svc, ops.Registry.IsRunning(id)))
	})

	rr.Handle(ClassConfigure, "PUT /api/services/{id}/autostart", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Autostart bool `json:"autostart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		id := r.PathValue("id")
		if err := ops.SetAutostart(id, body.Autostart); err != nil {
			writeServiceError(w, err)
			return
		}
		svc, _ := ops.Get(id)
		writeJSON(w, http.StatusOK, serviceViewOf(svc, ops.Registry.IsRunning(id)))
	})
}
