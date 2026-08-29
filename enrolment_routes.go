package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// credIDOf names the control-plane credential a request resolved to, or "" if
// none did — the same value RouteRegistrar.authorize reads for its own record.
func credIDOf(r *http.Request) string {
	id, _ := APICredentialIDFromContext(r.Context())
	return id
}

type enrolmentView struct {
	ClientID    string          `json:"client_id"`
	Fingerprint string          `json:"fingerprint"`
	ProjectIDs  []string        `json:"project_ids"`
	Budget      EnrolmentBudget `json:"budget"`
	CreatedAt   string          `json:"created_at"`
	// Dir is populated only on create: the bundle directory the client key
	// was written to. Never the key itself, and never any other file's
	// contents — see EnrolmentCreated.
	Dir string `json:"dir,omitempty"`
	// Set when the record was written but the on-disk bundle (client key,
	// client cert, CA cert) failed to write. The mutation still succeeded,
	// so this rides on a 2xx — same shape as serviceView.ProcessError.
	BundleError string `json:"bundle_error,omitempty"`
}

func enrolmentViewOf(e Enrolment) enrolmentView {
	return enrolmentView{
		ClientID:    e.ClientID,
		Fingerprint: e.Fingerprint,
		ProjectIDs:  e.ProjectIDs,
		Budget:      e.Budget,
		CreatedAt:   e.CreatedAt,
	}
}

func createdViewOf(c EnrolmentCreated) enrolmentView {
	v := enrolmentViewOf(c.Enrolment)
	v.Dir = c.Dir
	return v
}

func withBundleError(v enrolmentView, err error) enrolmentView {
	if err != nil {
		v.BundleError = err.Error()
	}
	return v
}

func enrolmentHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errEnrolmentNotFound):
		return http.StatusNotFound
	case errors.Is(err, errEnrolmentInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeEnrolmentError(w http.ResponseWriter, err error) {
	writeJSON(w, enrolmentHTTPStatus(err), map[string]string{"error": err.Error()})
}

// ops is the same instance the Remote Clients tab's IPC handlers use, so an
// enrolment created from curl and one created from the tray share validation,
// the CA, and the revocation hook.
func RegisterEnrolmentRoutes(rr *RouteRegistrar, ops *EnrolmentOps) {
	rr.Handle(ClassRead, "GET /api/enrolments", func(w http.ResponseWriter, r *http.Request) {
		list := ops.List()
		out := make([]enrolmentView, 0, len(list))
		for _, e := range list {
			out = append(out, enrolmentViewOf(e))
		}
		writeJSON(w, http.StatusOK, out)
	})

	rr.Handle(ClassRead, "GET /api/enrolments/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, err := ops.Get(r.PathValue("id"))
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrolmentViewOf(e))
	})

	rr.Handle(ClassGrant, "POST /api/enrolments", func(w http.ResponseWriter, r *http.Request) {
		var body enrolmentFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		created, err := ops.Create(body)
		if err != nil && !errors.Is(err, errEnrolmentBundle) {
			writeEnrolmentError(w, err)
			return
		}
		// This route already writes a control_decision saying this caller was
		// allowed to reach POST /api/enrolments. That is not the same fact as
		// "a client certificate was issued to hermes-mail granting proj_mail",
		// which is what an operator reading the log is looking for, so the act
		// gets its own record — and the enrolment is revoked if that record
		// cannot be written.
		if auditErr := recordEnrolmentIssued(rr.Issuance, ops.Store, created.Enrolment, auditViaHTTP, credIDOf(r)); auditErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "the enrolment could not be recorded in the audit log and has been revoked: " + auditErr.Error(),
			})
			return
		}
		// The client private key never rides in this response, on 2xx or
		// otherwise: enrolmentView has no field that names it, only the
		// bundle directory it was written to.
		writeJSON(w, http.StatusCreated, withBundleError(createdViewOf(created), err))
	})

	rr.Handle(ClassGrant, "DELETE /api/enrolments/{id}", func(w http.ResponseWriter, r *http.Request) {
		revoked, err := ops.Revoke(r.PathValue("id"))
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		// Reported and not undone, unlike the create above: a revocation
		// narrows, and refusing to narrow one because the log is broken would
		// make a failing disk the reason a compromised client stays enrolled.
		if auditErr := rr.recordIssuedBy(r, CredentialIssuance{
			Revoked:    true,
			Credential: auditCredentialEnrolment,
			Subject:    revoked.ClientID,
			Grants:     revoked.ProjectIDs,
		}); auditErr != nil {
			slog.Error("enrolment revoked but not recorded in the audit log", "client_id", revoked.ClientID, "error", auditErr)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	rr.Handle(ClassRead, "GET /api/remote", func(w http.ResponseWriter, r *http.Request) {
		view, err := ops.RemoteConfig()
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})

	// execute: the body sets the mTLS listener's bind address — the caller
	// chooses what relay exposes (ADR-015 decision 1).
	rr.Handle(ClassExecute, "PUT /api/remote", func(w http.ResponseWriter, r *http.Request) {
		var body remoteConfigFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		view, err := ops.SetRemoteConfig(body)
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}
