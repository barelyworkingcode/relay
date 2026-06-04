package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
)

// maxSessionBodyBytes bounds how much of a session-create body we buffer for
// model-allowlist validation. These payloads are small JSON envelopes
// (projectId, model, name, settings); 1 MiB is generous headroom while
// capping memory for a malformed or oversized request.
const maxSessionBodyBytes = 1 << 20

// newSessionModelGuard enforces a project's allowed_models allowlist on
// session creation, then forwards to the dispatcher.
//
// relayLLM has no project knowledge — the allowlist lives only in relay's
// settings — so this is the only layer that can enforce it. Keeping it here
// (rather than teaching relayLLM about projects) preserves the loose coupling
// between the two services. Eve's pickers filter the model list for UX, but
// this is the authoritative boundary.
//
// Enforcement is deliberately narrow:
//   - only POST /api/sessions (Eve's single session-create chokepoint),
//   - only when the project declares a non-empty, non-wildcard allowlist,
//   - only when the request names a concrete model (an empty model lets
//     relayLLM pick its configured default; the allowlist governs explicit
//     user choices, not server defaults).
//
// Anything we can't confidently classify as disallowed is forwarded so
// relayLLM remains the source of truth for every other failure mode.
func newSessionModelGuard(store SettingsStore, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSessionBodyBytes))
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		// Restore the body for the downstream proxy regardless of outcome.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		// Parse only the two fields we gate on so we don't couple to
		// relayLLM's evolving session schema.
		var payload struct {
			ProjectID string `json:"projectId"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			// Not a shape we understand — let relayLLM produce the error.
			next.ServeHTTP(w, r)
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

// modelAllowedForProject reports whether a session-create request naming
// `model` under `projectID` should be permitted. It fails open for cases that
// are not the allowlist's concern (no project scope, server-default model,
// unknown project, or an unrestricted/wildcard allowlist) and fails closed
// only when a project with an explicit allowlist is asked for a model not on
// it.
func modelAllowedForProject(store SettingsStore, projectID, model string) bool {
	if projectID == "" || model == "" {
		return true // no project scope, or server-default model
	}
	proj, _ := store.Get().findProjectByID(projectID)
	if proj == nil {
		return true // unknown project — let relayLLM produce the authoritative error
	}
	if len(proj.AllowedModels) == 0 || isWildcard(proj.AllowedModels) {
		return true // unrestricted
	}
	return slices.Contains(proj.AllowedModels, model)
}
