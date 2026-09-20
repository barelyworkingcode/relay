package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
		if r.Method == http.MethodPut {
			if sessionID, ok := isSessionModelUpdatePath(r.URL.Path); ok {
				guardSessionModelUpdate(store, next, w, r, sessionID)
				return
			}
		}

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
// project.ValidateShape requires a remote project's AllowedModels to be
// empty, but modelAllowedForProject treats an empty allowlist as
// unrestricted — folded together, a remote project would permit every
// model, the most permissive outcome reached through the most restrictive
// configuration.
func refuseRemoteSession(store config.SettingsStore, projectID string) error {
	if projectID == "" {
		return nil
	}
	proj, _ := config.FindProjectByID(config.FreshSettings(store), projectID)
	if proj == nil || !proj.IsRemote() {
		return nil
	}
	return fmt.Errorf("project %s is a remote project and cannot host a session", projectID)
}

func modelAllowedForProject(store config.SettingsStore, projectID, model string) bool {
	if projectID == "" || model == "" {
		return true // no project scope, or server-default model
	}
	proj, _ := config.FindProjectByID(config.FreshSettings(store), projectID)
	if proj == nil {
		return true // unknown project — let relayLLM produce the authoritative error
	}
	if len(proj.AllowedModels) == 0 || config.IsWildcard(proj.AllowedModels) {
		return true // unrestricted
	}
	return slices.Contains(proj.AllowedModels, model)
}

// isSessionModelUpdatePath reports whether p targets relayLLM's
// PUT /api/sessions/{id}/model route, tolerating one trailing slash. Like
// isSessionCreatePath, this classifies the raw path itself: the guard sits
// in front of the "/" catch-all proxy, not behind a mux pattern, so there is
// no ServeMux to do this matching for it.
func isSessionModelUpdatePath(p string) (id string, ok bool) {
	const prefix = "/api/sessions/"
	const suffix = "/model"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
	id, ok = strings.CutSuffix(rest, suffix)
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// guardSessionModelUpdate enforces allowed_models on PUT
// /api/sessions/{id}/model, the other route that can change a live pi
// session's model (relayLLM's ../relayLLM/api.go, SetPiModel). relay keeps
// no session table, so the session's projectId is resolved by asking
// relayLLM itself — through next, the same handler this request would
// otherwise be forwarded to unchecked — with an internal GET /api/sessions
// carrying over the incoming request's headers so the proxy authenticates
// and injects relayLLM's own internal bearer exactly as it does for any other
// forwarded request.
//
// FAIL CLOSED throughout: a lookup error, a non-200 lookup response, or a
// session missing from the list all refuse with 403 rather than forward —
// relayLLM being unreachable is not a reason to let a model switch bypass
// the allowlist a working relayLLM would have been checked against.
func guardSessionModelUpdate(store config.SettingsStore, next http.Handler, w http.ResponseWriter, r *http.Request, sessionID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSessionBodyBytes+1))
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	if len(body) > maxSessionBodyBytes {
		slog.Warn("frontend: session model-update body exceeds inspection cap; rejecting",
			"limit", maxSessionBodyBytes)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": "session model update body too large",
		})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	var payload struct {
		Provider string `json:"provider"`
		ModelID  string `json:"modelId"`
	}
	// Malformed or incomplete: SetModel refuses when either field is empty,
	// so there is no candidate model here to check against the allowlist —
	// forward and let relayLLM produce its own error.
	if err := json.Unmarshal(body, &payload); err != nil || payload.Provider == "" || payload.ModelID == "" {
		next.ServeHTTP(w, r)
		return
	}

	projectID, err := lookupSessionProjectID(next, r, sessionID)
	if err != nil {
		slog.Warn("frontend: could not resolve session's project for model-update guard; refusing",
			"session", sessionID, "error", err)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "could not verify session ownership",
		})
		return
	}

	model := piModelID(payload.Provider, payload.ModelID)
	if !modelAllowedForProject(store, projectID, model) {
		slog.Warn("frontend: blocked session model update to disallowed model",
			"project", projectID, "session", sessionID, "model", model)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "model not allowed for this project",
		})
		return
	}
	next.ServeHTTP(w, r)
}

// piModelID spells a pi-backed model the same way session creation does
// (relayLLM's pi_models.go), so the same allowed_models entries gate both
// routes.
func piModelID(provider, modelID string) string {
	return "pi/" + provider + "/" + modelID
}

// lookupSessionProjectID resolves a session's projectId by asking relayLLM's
// own GET /api/sessions through next, since relay keeps no session table of
// its own. The incoming request's headers are carried over so the reverse
// proxy's Director authenticates the lookup and injects its internal service
// token exactly as it would for the caller's original request.
func lookupSessionProjectID(next http.Handler, r *http.Request, sessionID string) (string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return "", fmt.Errorf("build session lookup request: %w", err)
	}
	req.Header = r.Header.Clone()

	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("session lookup returned status %d", rec.Code)
	}

	var sessions []struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		return "", fmt.Errorf("decode session list: %w", err)
	}
	for _, s := range sessions {
		if s.ID == sessionID {
			return s.ProjectID, nil
		}
	}
	return "", fmt.Errorf("session %s not found", sessionID)
}
