package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
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
	ClientID   string                 `json:"client_id"`
	ProjectIDs []string               `json:"project_ids"`
	Budget     config.EnrolmentBudget `json:"budget"`
}

type ipcEnrolmentIDMsg struct {
	ClientID string `json:"client_id"`
}

// ipcRemoteConfigMsg's JSON tags match remoteConfigFields'.
type ipcRemoteConfigMsg struct {
	Remove                   bool            `json:"remove"`
	Enabled                  bool            `json:"enabled"`
	Listen                   string          `json:"listen"`
	EnrolmentListen          string          `json:"enrolment_listen"`
	RemovedEnrolmentRequests json.RawMessage `json:"enrolment_requests,omitempty"`
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
// enrolmentFields. There is deliberately no field that could carry a CSR or
// a profile: see approveFields' own doc comment for why those fields must not
// exist on ANY caller of Approve, this one included. The body decodes with
// unknown fields refused (see ipcApproveEnrolmentRequest).
type ipcApproveEnrolmentRequestMsg struct {
	// Type is the IPC envelope's own tag, which onSettingsIpc passes through
	// with the body. It is ignored; every other unknown field is refused.
	Type      string                 `json:"type"`
	RequestID string                 `json:"request_id"`
	ClientID  string                 `json:"client_id"`
	Budget    config.EnrolmentBudget `json:"budget"`
}

// pendingEnrolmentRequestView is the Pending requests panel's projection of
// enrolmentRequestView (enrolment_requests.go): JSON tags matching this
// file's other view types, and by construction no field that could carry
// the CSR the record holds — TestPendingEnrolmentRequestView_CarriesNoCSRBytes
// pins that by reflection AND by marshalling a populated value, so a field
// added here under a name that dodges the reflection scan (or a stray
// %v-style dump) still fails the marshal check. The request carries no
// grant and no budget field either (spec §3): those are the human's choice
// at approval, never the network peer's. ProfileID and ProfileName are the
// window's profile: the sheet shows it and offers no choice.
type pendingEnrolmentRequestView struct {
	RequestID        string `json:"request_id"`
	SPKISHA256       string `json:"spki_sha256"`
	Label            string `json:"label"`
	RemoteAddr       string `json:"remote_addr"`
	ArrivedAt        string `json:"arrived_at"`
	ExpiresAt        string `json:"expires_at"`
	Approved         bool   `json:"approved"`
	ApprovedClientID string `json:"approved_client_id,omitempty"`

	// The comparison code and its two failure states (spec §6.2). SAS is
	// empty until the requesting machine has opened its commitment, which
	// is why SASReady exists as its own bool rather than being inferred
	// from a non-empty string: "no code yet" and "this row will never have
	// one" are different sentences on screen and only one of them is an
	// error.
	SAS       string `json:"sas,omitempty"`
	SASReady  bool   `json:"sas_ready"`
	SASFailed bool   `json:"sas_failed"`

	// ProfileID is the access profile of the window the request arrived in,
	// and ProfileName its display name (the id when the profile has none or
	// is gone).
	ProfileID   string `json:"profile_id"`
	ProfileName string `json:"profile_name"`

	// SuggestedClientID is the host's collision-free suggestion for the
	// approval sheet's client-id field. Advisory: the validation inside
	// store.With is still the authority on uniqueness, and empty means "no
	// suggestion", never a chosen id.
	SuggestedClientID string `json:"suggested_client_id,omitempty"`
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
		SAS:              v.SAS,
		SASReady:         v.SASReady,
		SASFailed:        v.SASFailed,
		ProfileID:        v.ProfileID,
		ProfileName:      v.ProfileID,
	}
}

// pendingEnrolmentRequestViewsOf takes the settings snapshot the suggestion
// is computed against rather than a store, so the projection stays a pure
// function of what the caller already read — the count in the tray menu and
// the rows in the panel come from one read, never two.
func pendingEnrolmentRequestViewsOf(views []enrolmentRequestView, s *config.Settings) []pendingEnrolmentRequestView {
	out := make([]pendingEnrolmentRequestView, 0, len(views))
	for _, v := range views {
		pv := pendingEnrolmentRequestViewOf(v)
		pv.SuggestedClientID = suggestClientID(s, v.Label)
		pv.ProfileName = profileDisplayName(s, v.ProfileID)
		out = append(out, pv)
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
	// CAFingerprint is relay's CA certificate SHA-256, read straight off
	// disk with no sealer and no CA generation on this path. Empty when no CA
	// has been generated yet. It stays in the API for readers that already
	// use it; no relay command or screen asks the operator to carry it.
	CAFingerprint string `json:"ca_fingerprint,omitempty"`

	// EnrolmentListen/EnrolmentEffective mirror Listen/Effective for the
	// enrolment window's socket. EnrolmentEffective is always populated — the
	// address a window binds — so the tab can show it while no window is open.
	EnrolmentListen    string `json:"enrolment_listen,omitempty"`
	EnrolmentEffective string `json:"enrolment_effective"`

	// EnrolmentWindow is the open enrolment window, or {"open": false}.
	// Filled in by the doors that hold the supervisor (EnrolmentOps.RemoteConfig);
	// remoteConfigViewOf alone cannot see in-memory state.
	EnrolmentWindow enrolmentWindowView `json:"enrolment_window"`
}

func remoteConfigViewOf(s *config.Settings, auditEnabled bool) remoteConfigView {
	resolved := resolveRemoteConfig(s.Remote)
	v := remoteConfigView{
		Configured:         s.Remote != nil,
		Enabled:            resolved.Enabled,
		Effective:          resolved.Listen,
		AuditEnabled:       auditEnabled,
		EnrolmentEffective: defaultEnrolmentListen,
		EnrolmentWindow:    currentEnrolmentWindowView(s),
	}
	if s.Remote != nil {
		v.Listen = s.Remote.Listen
		v.EnrolmentListen = s.Remote.EnrolmentListen
		if v.EnrolmentListen != "" {
			v.EnrolmentEffective = v.EnrolmentListen
		}
	}
	if fp, err := enrolment.CAFingerprintFromDisk(); err == nil {
		v.CAFingerprint = fp
	}
	return v
}

func enrolmentBudgetDefaults() config.EnrolmentBudget {
	return enrolment.NormalizeBudget(config.EnrolmentBudget{})
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
		// Only enrolment.ErrBundle means the record landed; every other
		// error means nothing was persisted, and announcing a row for it
		// would add a credential-less enrolment to the list.
		if err != nil && !errors.Is(err, enrolment.ErrBundle) {
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
// through enrolment.Revoke rather than deleting the record directly: enrolment.Revoke
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
	fields := remoteConfigFields{
		Remove:                   msg.Remove,
		Enabled:                  msg.Enabled,
		Listen:                   msg.Listen,
		EnrolmentListen:          msg.EnrolmentListen,
		RemovedEnrolmentRequests: msg.RemovedEnrolmentRequests,
	}
	// Off the main thread: EnrolmentOps.SetRemoteConfig is gated
	// (remote.configure) when the request actually widens what a remote
	// client can reach, and Gate.Require blocks on LocalAuthentication's
	// async completion handler -- the same deadlock ipcCreateEnrolment's
	// comment above describes.
	ctx.GoFunc(func() {
		view, err := ctx.EnrolmentOps.SetRemoteConfig(ctx.Ctx, fields, auditViaIPC, "")
		if err != nil {
			dispatchEmit(ctx, "onRemoteConfigError", err.Error())
			return
		}
		dispatchEmit(ctx, "onRemoteConfigUpdated", marshalForUI(view))
	})
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
		marshalForUI(pendingEnrolmentRequestViewsOf(ctx.EnrolmentOps.PendingRequests(), config.DisplaySettings(ctx.Store))))
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
	// Unknown fields are refused, not ignored: a body that still names a
	// profile must not be read as an approval that ignored it.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var msg ipcApproveEnrolmentRequestMsg
	if err := dec.Decode(&msg); err != nil {
		dispatchEmit(ctx, "onEnrolmentError", fmt.Sprintf("approve_enrolment_request: invalid request: %v", err))
		return
	}
	if msg.RequestID == "" || msg.ClientID == "" {
		return
	}
	fields := approveFields{
		RequestID: msg.RequestID,
		ClientID:  msg.ClientID,
		Budget:    msg.Budget,
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
		// Only enrolment.ErrBundle, errEnrolmentRequestExpired,
		// errEnrolmentRequestRefused and errEnrolmentRequestAbandonedInFlight
		// mean the record landed; every other
		// error means nothing was persisted, and announcing a row for it
		// would add a credential-less enrolment to the list — same rule
		// ipcCreateEnrolment follows.
		if err != nil && !errors.Is(err, enrolment.ErrBundle) && !errors.Is(err, errEnrolmentRequestExpired) && !errors.Is(err, errEnrolmentRequestRefused) &&
			!errors.Is(err, errEnrolmentRequestAbandonedInFlight) {
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
	if err := ctx.EnrolmentOps.Refuse(ctx.Ctx, msg.RequestID); err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	emitPendingEnrolmentRequests(ctx)
}
