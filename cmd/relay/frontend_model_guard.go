package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"io"
	"log/slog"
	"net/http"
	"slices"
)

const maxSessionBodyBytes = 1 << 20

// relayLLM has no project knowledge, so this guard is the only layer that
// can enforce a project's allowed_models allowlist; everything not
// confidently disallowed is forwarded, keeping relayLLM the source of truth.
//
// Must classify the request itself rather than rely on an exact mux
// pattern: Go's ServeMux routes "POST /api/sessions/" (trailing slash) to
// the "/" catch-all, bypassing a guard mounted only on the exact
// "POST /api/sessions" pattern.
func newSessionModelGuard(store config.SettingsStore, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !isSessionCreatePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Read one byte past the cap to distinguish "just fits" from
		// oversized; truncating and forwarding would fail open instead.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSessionBodyBytes+1))
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		if len(body) > maxSessionBodyBytes {
			slog.Warn("frontend: session create body exceeds inspection cap; rejecting",
				"limit", maxSessionBodyBytes)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "session create body too large",
			})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		// Only the two fields the guard gates on, so it doesn't couple to
		// relayLLM's evolving session schema.
		var payload struct {
			ProjectID string `json:"projectId"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			next.ServeHTTP(w, r)
			return
		}

		if err := refuseRemoteSession(store, payload.ProjectID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		if !modelAllowedForProject(store, payload.ProjectID, payload.Model) {
			slog.Warn("frontend: blocked session create with disallowed model",
				"project", payload.ProjectID, "model", payload.Model)
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "model not allowed for this project",
			})
			return
		}
		next.ServeHTTP(w, r)
	}
}

func isSessionCreatePath(p string) bool {
	return p == "/api/sessions" || p == "/api/sessions/"
}

// Must be a separate check, not a case inside the model allowlist:
// validateProjectShape requires a remote project's AllowedModels to be
// empty, but modelAllowedForProject treats an empty allowlist as
// unrestricted — folded together, a remote project would permit every
// model, the most permissive outcome reached through the most restrictive
// configuration.
func refuseRemoteSession(store config.SettingsStore, projectID string) error {
	if projectID == "" {
		return nil
	}
	proj, _ := config.FindProjectByID(store.Get(), projectID)
	if proj == nil || !proj.IsRemote() {
		return nil
	}
	return fmt.Errorf("project %s is a remote project and cannot host a session", projectID)
}

func modelAllowedForProject(store config.SettingsStore, projectID, model string) bool {
	if projectID == "" || model == "" {
		return true // no project scope, or server-default model
	}
	proj, _ := config.FindProjectByID(store.Get(), projectID)
	if proj == nil {
		return true // unknown project — let relayLLM produce the authoritative error
	}
	if len(proj.AllowedModels) == 0 || config.IsWildcard(proj.AllowedModels) {
		return true // unrestricted
	}
	return slices.Contains(proj.AllowedModels, model)
}
