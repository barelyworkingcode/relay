package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/enrolment"
)

// credIDOf names the control-plane credential a request resolved to, or "" if
// none did — the same value control.RouteRegistrar.authorize reads for its own record.
func credIDOf(r *http.Request) string {
	id, _ := APICredentialIDFromContext(r.Context())
	return id
}

type enrolmentView struct {
	ClientID    string                 `json:"client_id"`
	Fingerprint string                 `json:"fingerprint"`
	ProjectIDs  []string               `json:"project_ids"`
	Budget      config.EnrolmentBudget `json:"budget"`
	CreatedAt   string                 `json:"created_at"`
	// Dir is populated only on create: the bundle directory the client key
	// was written to. Never the key itself, and never any other file's
	// contents — see EnrolmentCreated.
	Dir string `json:"dir,omitempty"`
	// Set when the record was written but the on-disk bundle (client key,
	// client cert, CA cert) failed to write. The mutation still succeeded,
	// so this rides on a 2xx — same shape as serviceView.ProcessError.
	BundleError string `json:"bundle_error,omitempty"`
	// CLIAdmin mirrors Enrolment.CLIAdmin: read-only here, there is no
	// HTTP or IPC update door for an enrolment.
	CLIAdmin bool `json:"cli_admin,omitempty"`
}

func enrolmentViewOf(e config.Enrolment) enrolmentView {
	return enrolmentView{
		ClientID:    e.ClientID,
		Fingerprint: e.Fingerprint,
		ProjectIDs:  e.ProjectIDs,
		Budget:      e.Budget,
		CreatedAt:   e.CreatedAt,
		CLIAdmin:    e.CLIAdmin,
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
	case errors.Is(err, enrolment.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, enrolment.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, errRemoteConfigChangedDuringApproval):
		return http.StatusConflict
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
func RegisterEnrolmentRoutes(rr *control.RouteRegistrar, ops *EnrolmentOps) {
	rr.Handle(control.ClassRead, "GET /api/enrolments", func(w http.ResponseWriter, r *http.Request) {
		list := ops.List()
		out := make([]enrolmentView, 0, len(list))
		for _, e := range list {
			out = append(out, enrolmentViewOf(e))
		}
		writeJSON(w, http.StatusOK, out)
	})

	rr.Handle(control.ClassRead, "GET /api/enrolments/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, err := ops.Get(r.PathValue("id"))
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, enrolmentViewOf(e))
	})

	rr.Handle(control.ClassGrant, "POST /api/enrolments", func(w http.ResponseWriter, r *http.Request) {
		var body enrolmentFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		// EnrolmentOps.Create now records the issuance itself (and undoes the
		// create if that recording fails) so it can attach the presence_id
		// the gate minted — this route no longer has a grant to name.
		created, err := ops.Create(r.Context(), body, auditViaHTTP, credIDOf(r))
		if err != nil && !errors.Is(err, enrolment.ErrBundle) {
			writeEnrolmentError(w, err)
			return
		}
		// The client private key never rides in this response, on 2xx or
		// otherwise: enrolmentView has no field that names it, only the
		// bundle directory it was written to.
		writeJSON(w, http.StatusCreated, withBundleError(createdViewOf(created), err))
	})

	rr.Handle(control.ClassGrant, "DELETE /api/enrolments/{id}", func(w http.ResponseWriter, r *http.Request) {
		// EnrolmentOps.Revoke now records the revocation itself, so it can
		// attach the presence_id the gate minted.
		if _, err := ops.Revoke(r.Context(), r.PathValue("id"), auditViaHTTP, credIDOf(r)); err != nil {
			writeEnrolmentError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	rr.Handle(control.ClassRead, "GET /api/remote", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ops.RemoteConfig())
	})

	// execute: the body sets the mTLS listener's bind address, and now also
	// the enrolment-request listener's (spec §1) — the caller chooses what
	// relay exposes (ADR-015 decision 1). Still one route, one class: the
	// enrolment fields ride remoteConfigFields unchanged, so this handler
	// gains no new surface, only two more fields on the body it already
	// decodes. SetRemoteConfig itself is now gated (remote.configure) when
	// the request actually widens what a remote client reaches — this
	// route was ClassExecute's one ungated member (F1); it no longer is.
	rr.Handle(control.ClassExecute, "PUT /api/remote", func(w http.ResponseWriter, r *http.Request) {
		var body remoteConfigFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		view, err := ops.SetRemoteConfig(r.Context(), body, auditViaHTTP, credIDOf(r))
		if err != nil {
			writeEnrolmentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}
