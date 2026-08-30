package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	MsgCreateEnrolment    = "create_enrolment"
	MsgRevokeEnrolment    = "revoke_enrolment"
	MsgUpdateRemoteConfig = "update_remote_config"

	// Pending requests (spec §3): the Remote Clients tab's own doors onto
	// the pending table, thin adapters over EnrolmentOps exactly like the
	// three above are over Create/Revoke/SetRemoteConfig.
	MsgListEnrolmentRequests   = "list_enrolment_requests"
	MsgApproveEnrolmentRequest = "approve_enrolment_request"
	MsgRefuseEnrolmentRequest  = "refuse_enrolment_request"
)

type ipcCreateEnrolmentMsg struct {
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

type ipcEnrolmentIDMsg struct {
	ClientID string `json:"client_id"`
}

// ipcRemoteConfigMsg's JSON tags match remoteConfigFields' — see that
// type's doc comment for why EnrolmentRequests/EnrolmentListen carry no
// save-time refusal of their own.
type ipcRemoteConfigMsg struct {
	Remove            bool   `json:"remove"`
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	EnrolmentRequests bool   `json:"enrolment_requests"`
	EnrolmentListen   string `json:"enrolment_listen"`
}

// ipcEnrolmentRequestIDMsg is refuse_enrolment_request's whole body: the
// pending record it names, nothing else.
type ipcEnrolmentRequestIDMsg struct {
	RequestID string `json:"request_id"`
}

// ipcApproveEnrolmentRequestMsg is approve_enrolment_request's body —
// approveFields' own JSON shape (enrolment_ops.go), repeated here rather
// than reused directly so this door's wire type can evolve independently of
// the CLI's, the way ipcCreateEnrolmentMsg already stands apart from
// enrolmentFields. There is deliberately no field that could carry a CSR:
// see approveFields' own doc comment for why that field must not exist on
// ANY caller of Approve, this one included.
type ipcApproveEnrolmentRequestMsg struct {
	RequestID  string          `json:"request_id"`
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

// pendingEnrolmentRequestView is the Pending requests panel's projection of
// enrolmentRequestView (enrolment_requests.go): JSON tags matching this
// file's other view types, and by construction no field that could carry
// the CSR the record holds — TestPendingEnrolmentRequestView_CarriesNoCSRBytes
// pins that by reflection AND by marshalling a populated value, so a field
// added here under a name that dodges the reflection scan (or a stray
// %v-style dump) still fails the marshal check. The request carries no
// grant and no budget field either (spec §3): those are the human's choice
// at approval, never the network peer's.
type pendingEnrolmentRequestView struct {
	RequestID        string `json:"request_id"`
	SPKISHA256       string `json:"spki_sha256"`
	Label            string `json:"label"`
	RemoteAddr       string `json:"remote_addr"`
	ArrivedAt        string `json:"arrived_at"`
	ExpiresAt        string `json:"expires_at"`
	Approved         bool   `json:"approved"`
	ApprovedClientID string `json:"approved_client_id,omitempty"`
}

func pendingEnrolmentRequestViewOf(v enrolmentRequestView) pendingEnrolmentRequestView {
	return pendingEnrolmentRequestView{
		RequestID:        v.RequestID,
		SPKISHA256:       v.SPKISHA256,
		Label:            v.Label,
		RemoteAddr:       v.RemoteAddr,
		ArrivedAt:        v.ArrivedAt.UTC().Format(time.RFC3339),
		ExpiresAt:        v.ExpiresAt.UTC().Format(time.RFC3339),
		Approved:         v.Approved,
		ApprovedClientID: v.ApprovedClientID,
	}
}

func pendingEnrolmentRequestViewsOf(views []enrolmentRequestView) []pendingEnrolmentRequestView {
	out := make([]pendingEnrolmentRequestView, 0, len(views))
	for _, v := range views {
		out = append(out, pendingEnrolmentRequestViewOf(v))
	}
	return out
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
	// CAFingerprint is relay's CA certificate SHA-256 (spec §6) — the value
	// `relayremote request --ca-fingerprint` must be given, and the one
	// thing that closes the request channel's MITM (§6: "only the CA pin
	// stops this"). Read straight off disk, exactly like `relay enrol
	// ca-fingerprint`, so this and that command are byte-identical for the
	// same CA (AC-30) with no sealer and no CA generation on this path.
	// Empty when no CA has been generated yet (no enrolment created or
	// signed on this install): the tab shows that as an explanation rather
	// than surfacing loadCACertificateOnly's error, since a missing CA here
	// is not a caller mistake to report as a failure.
	CAFingerprint string `json:"ca_fingerprint,omitempty"`

	// EnrolmentRequests/EnrolmentListen/EnrolmentEffective mirror
	// Enabled/Listen/Effective for the third listener (spec §1).
	// EnrolmentEffective is always populated — the address the listener
	// would bind if turned on — so the tab can show it even while the
	// listener is off, the same way Effective already does for the
	// tool-plane one.
	EnrolmentRequests  bool   `json:"enrolment_requests"`
	EnrolmentListen    string `json:"enrolment_listen,omitempty"`
	EnrolmentEffective string `json:"enrolment_effective"`
}

func remoteConfigViewOf(s *Settings, auditEnabled bool) remoteConfigView {
	resolved := s.Remote.resolve()
	v := remoteConfigView{
		Configured:         s.Remote != nil,
		Enabled:            resolved.Enabled,
		Effective:          resolved.Listen,
		AuditEnabled:       auditEnabled,
		EnrolmentEffective: defaultEnrolmentListen,
	}
	if s.Remote != nil {
		v.Listen = s.Remote.Listen
		v.EnrolmentRequests = boolOr(s.Remote.EnrolmentRequests, false)
		v.EnrolmentListen = s.Remote.EnrolmentListen
		if v.EnrolmentListen != "" {
			v.EnrolmentEffective = v.EnrolmentListen
		}
	}
	if fp, err := caFingerprintFromDisk(); err == nil {
		v.CAFingerprint = fp
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
		Remove:            msg.Remove,
		Enabled:           msg.Enabled,
		Listen:            msg.Listen,
		EnrolmentRequests: msg.EnrolmentRequests,
		EnrolmentListen:   msg.EnrolmentListen,
	})
	if err != nil {
		ctx.UI.EmitEvent("onRemoteConfigError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onRemoteConfigUpdated", marshalForUI(view))
}

// emitPendingEnrolmentRequests is the one place that reads
// EnrolmentOps.PendingRequests and projects it for the WebView — every
// handler below that changes the table's contents ends by calling this
// rather than building its own event, so the panel can never drift from
// what a plain list actually shows. Not gated: PendingRequests is a read
// (EnrolmentOps.PendingRequests's own doc comment), so this never needs
// ctx.GoFunc on its own account.
func emitPendingEnrolmentRequests(ctx *IPCContext) {
	ctx.UI.EmitEvent("onEnrolmentRequestsChanged",
		marshalForUI(pendingEnrolmentRequestViewsOf(ctx.EnrolmentOps.PendingRequests())))
}

// ipcListEnrolmentRequests answers the Remote Clients tab's Pending requests
// panel. Fetched on demand (tab open) rather than seeded into the first
// paint, the way the Tool Calls tab's log is: the table is held by the
// running tray process alone (never settings.json), and a network peer can
// change it between one paint and the next, so a value seeded once would
// go stale in a way nothing here would ever correct.
func ipcListEnrolmentRequests(ctx *IPCContext, _ json.RawMessage) {
	emitPendingEnrolmentRequests(ctx)
}

// ipcApproveEnrolmentRequest turns a pending network request into a signed
// certificate, exactly like ipcCreateEnrolment does for an operator-typed
// request — Approve reuses enrolment.sign's own gate rather than opening a
// second door into issuance, see EnrolmentOps.Approve's own doc comment.
func ipcApproveEnrolmentRequest(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcApproveEnrolmentRequestMsg](raw, MsgApproveEnrolmentRequest)
	if !ok || msg.RequestID == "" || msg.ClientID == "" {
		return
	}
	fields := approveFields{
		RequestID:  msg.RequestID,
		ClientID:   msg.ClientID,
		ProjectIDs: msg.ProjectIDs,
		Budget:     msg.Budget,
	}

	// Off the main thread: EnrolmentOps.Approve is gated (enrolment.sign,
	// spec §3), and Gate.Require blocks on LocalAuthentication's async
	// completion handler exactly as EnrolmentOps.Create's does — the
	// approval is a gated call fired from a UI click, precisely the shape
	// §11.6 warns deadlocks the Cocoa run loop if it runs on the main
	// thread. Same fix as ipcCreateEnrolment: ctx.GoFunc, then
	// DispatchToMain before any UI touch.
	ctx.GoFunc(func() {
		created, err := ctx.EnrolmentOps.Approve(ctx.Ctx, fields, auditViaIPC, "")
		// Only errEnrolmentBundle and errEnrolmentRequestExpired mean the
		// record landed; every other error means nothing was persisted,
		// and announcing a row for it would add a credential-less
		// enrolment to the list — same rule ipcCreateEnrolment follows.
		if err != nil && !errors.Is(err, errEnrolmentBundle) && !errors.Is(err, errEnrolmentRequestExpired) {
			dispatchEmit(ctx, "onEnrolmentError", err.Error())
			return
		}
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onEnrolmentError", fmt.Sprintf("enrolment request approved but %v", err))
			}
			ctx.UI.EmitEvent("onEnrolmentCreated",
				marshalForUI(created.Enrolment),
				marshalForUI(enrolmentBundleView{Dir: created.Dir}))
			emitPendingEnrolmentRequests(ctx)
		})
	})
}

// ipcRefuseEnrolmentRequest is the operator's explicit decline (spec §2,
// §3). Deliberately NOT run through ctx.GoFunc: EnrolmentOps.Refuse never
// calls requireGate (see its own doc comment — declining a stranger's
// request is not the act presence.Gate protects), so there is no
// LocalAuthentication completion handler to deadlock the run loop against.
func ipcRefuseEnrolmentRequest(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcEnrolmentRequestIDMsg](raw, MsgRefuseEnrolmentRequest)
	if !ok || msg.RequestID == "" {
		return
	}
	if err := ctx.EnrolmentOps.Refuse(msg.RequestID); err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	emitPendingEnrolmentRequests(ctx)
}
