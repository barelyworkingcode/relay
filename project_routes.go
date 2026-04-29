package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// RegisterProjectRoutes wires the project HTTP endpoints. Payloads are
// snake_case to match relay's on-disk format; Eve normalizes to camelCase
// on its side.
func RegisterProjectRoutes(mux *http.ServeMux, store SettingsStore) {
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		projects := store.Get().Projects
		if projects == nil {
			projects = []Project{}
		}
		writeJSON(w, http.StatusOK, projects)
	})

	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		proj, _ := store.Get().findProjectByID(r.PathValue("id"))
		if proj == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusOK, proj)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("frontend: failed to encode response", "error", err)
	}
}
