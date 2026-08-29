package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	fields := enrolmentFields{
		ClientID:   msg.ClientID,
		ProjectIDs: msg.ProjectIDs,
		Budget:     msg.Budget,
	}

	// Off the main thread: EnrolmentOps.Create is gated (enrolment.create,
	// §6.4 of the ADR-017 implementation spec), and Gate.Require blocks on
	// LocalAuthentication's async completion handler, which needs the
	// Cocoa run loop pumped to be delivered — the same deadlock
	// showLoginCode's doc comment in trayapp.go describes.
	ctx.GoFunc(func() {
		// EnrolmentOps.Create now records the issuance itself (and undoes
		// the create if that recording fails) so it can attach the
		// presence_id the gate minted.
		created, err := ctx.EnrolmentOps.Create(ctx.Ctx, fields, auditViaIPC, "")
		// Only errEnrolmentBundle means the record landed; every other
		// error means nothing was persisted, and announcing a row for it
		// would add a credential-less enrolment to the list.
		if err != nil && !errors.Is(err, errEnrolmentBundle) {
			dispatchEmit(ctx, "onEnrolmentError", err.Error())
			return
		}
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onEnrolmentError", fmt.Sprintf("enrolment created but %v", err))
			}
			ctx.UI.EmitEvent("onEnrolmentCreated",
				marshalForUI(created.Enrolment),
				marshalForUI(enrolmentBundleView{Dir: created.Dir}))
		})
	})
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
	// Off the main thread: EnrolmentOps.Revoke is gated (enrolment.revoke,
	// §6.4); same reasoning as ipcCreateEnrolment above.
	ctx.GoFunc(func() {
		// EnrolmentOps.Revoke now records the revocation itself, so it can
		// attach the presence_id the gate minted.
		revoked, err := ctx.EnrolmentOps.Revoke(ctx.Ctx, msg.ClientID, auditViaIPC, "")
		if err != nil {
			dispatchEmit(ctx, "onEnrolmentError", err.Error())
			return
		}
		dispatchEmit(ctx, "onEnrolmentRevoked", revoked.ClientID, revoked.Fingerprint)
	})
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
