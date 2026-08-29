package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

const (
	MsgCreateEnrolment    = "create_enrolment"
	MsgRevokeEnrolment    = "revoke_enrolment"
	MsgUpdateRemoteConfig = "update_remote_config"
)

type ipcCreateEnrolmentMsg struct {
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

type ipcEnrolmentIDMsg struct {
	ClientID string `json:"client_id"`
}

type ipcRemoteConfigMsg struct {
	Remove  bool   `json:"remove"`
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

// enrolmentBundleView is everything the UI is told about an emitted
// bundle: the directory, and nothing else. Create writes a client private
// key into that directory, and the key does not cross this boundary — not
// its bytes, not a preview, not a "reveal" button — because the settings
// WebView is a rendering surface, and key material that reaches it has been
// copied somewhere nobody will think to wipe.
//
// The three filenames inside the bundle are static UI copy rather than
// fields here, so there is no field on this struct that a future edit
// could quietly widen from "path" to "contents".
type enrolmentBundleView struct {
	Dir string `json:"dir"`
}

type remoteConfigView struct {
	Configured   bool   `json:"configured"`
	Enabled      bool   `json:"enabled"`
	Listen       string `json:"listen"`
	Effective    string `json:"effective"`
	AuditEnabled bool   `json:"audit_enabled"`
}

func remoteConfigViewOf(s *Settings, auditEnabled bool) remoteConfigView {
	resolved := s.Remote.resolve()
	v := remoteConfigView{
		Configured:   s.Remote != nil,
		Enabled:      resolved.Enabled,
		Effective:    resolved.Listen,
		AuditEnabled: auditEnabled,
	}
	if s.Remote != nil {
		v.Listen = s.Remote.Listen
	}
	return v
}

func enrolmentBudgetDefaults() EnrolmentBudget {
	return normalizeEnrolmentBudget(EnrolmentBudget{})
}

func ipcCreateEnrolment(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcCreateEnrolmentMsg](raw, MsgCreateEnrolment)
	if !ok {
		return
	}
	created, err := ctx.EnrolmentOps.Create(enrolmentFields{
		ClientID:   msg.ClientID,
		ProjectIDs: msg.ProjectIDs,
		Budget:     msg.Budget,
	})
	// Only errEnrolmentBundle means the record landed; every other error
	// means nothing was persisted, and announcing a row for it would add a
	// credential-less enrolment to the list.
	if err != nil && !errors.Is(err, errEnrolmentBundle) {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	if err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", fmt.Sprintf("enrolment created but %v", err))
	}
	// Recorded before the bundle directory is announced, and the enrolment is
	// revoked if that record cannot be written: the client key on disk is the
	// credential, so an unrecorded create is undone rather than reported.
	if auditErr := recordEnrolmentIssued(issuanceAuditorOrNil(ctx.Audit), ctx.Store, created.Enrolment, auditViaIPC, ""); auditErr != nil {
		ctx.UI.EmitEvent("onEnrolmentError", fmt.Sprintf("enrolment %q could not be recorded in the audit log (%v) and has been removed", created.Enrolment.ClientID, auditErr))
		return
	}
	ctx.UI.EmitEvent("onEnrolmentCreated",
		marshalForUI(created.Enrolment),
		marshalForUI(enrolmentBundleView{Dir: created.Dir}))
}

// ipcRevokeEnrolment goes through ctx.EnrolmentOps.Revoke, which goes
// through revokeEnrolment rather than RemoveEnrolment directly: revokeEnrolment
// fires the revocation hook so the listener severs LIVE connections holding
// that certificate, and removes the emitted bundle. Deleting the record
// alone would leave a compromised agent in its scanner loop working
// indefinitely, since it never needs to reconnect.
func ipcRevokeEnrolment(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcEnrolmentIDMsg](raw, MsgRevokeEnrolment)
	if !ok || msg.ClientID == "" {
		return
	}
	revoked, err := ctx.EnrolmentOps.Revoke(msg.ClientID)
	if err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	// Reported and not undone: a revocation narrows, and refusing to narrow
	// one because the log is broken is the worse failure of the two.
	if auditErr := recordIssuance(issuanceAuditorOrNil(ctx.Audit), CredentialIssuance{
		Revoked:    true,
		Credential: auditCredentialEnrolment,
		Subject:    revoked.ClientID,
		Grants:     revoked.ProjectIDs,
		Via:        auditViaIPC,
	}); auditErr != nil {
		slog.Error("enrolment revoked but not recorded in the audit log", "client_id", revoked.ClientID, "error", auditErr)
	}
	ctx.UI.EmitEvent("onEnrolmentRevoked", revoked.ClientID, revoked.Fingerprint)
}

func ipcUpdateRemoteConfig(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcRemoteConfigMsg](raw, MsgUpdateRemoteConfig)
	if !ok {
		return
	}
	view, err := ctx.EnrolmentOps.SetRemoteConfig(remoteConfigFields{
		Remove:  msg.Remove,
		Enabled: msg.Enabled,
		Listen:  msg.Listen,
	})
	if err != nil {
		ctx.UI.EmitEvent("onRemoteConfigError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onRemoteConfigUpdated", marshalForUI(view))
}
