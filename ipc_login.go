package main

import "encoding/json"

// The Passkeys tab's IPC door. Thin adapters over LoginOps, the way
// ipc_enrolments.go is a thin adapter over EnrolmentOps: nothing here
// validates, and nothing here talks to the store — so the tab and
// `relay login` cannot disagree about what a revoke does.
//
// There is no mint handler. The bootstrap code is minted by the tray menu
// item and by `relay login enrol`, which are the two presentations ADR-016
// decision 2 allows, and adding a third one behind a button in the page
// would put the anchor's only remaining act on the surface the anchor exists
// to keep it off.

const (
	MsgListPasskeys  = "list_passkeys"
	MsgRevokePasskey = "revoke_passkey"
	MsgSignOutLogin  = "sign_out_login"
)

type ipcPasskeyIDMsg struct {
	ID string `json:"id"`
}

// ipcListPasskeys answers with both halves at once. They are one question —
// "who can sign in, and who is signed in" — and a tab that fetched them
// separately could render a passkey list next to a stale session list and
// make the revoke decision on the wrong facts.
func ipcListPasskeys(ctx *IPCContext, _ json.RawMessage) {
	ctx.UI.EmitEvent("onPasskeysReloaded",
		marshalForUI(ctx.LoginOps.Passkeys()),
		marshalForUI(ctx.LoginOps.Sessions()))
}

// ipcRevokePasskey removes the registration; it deliberately does NOT touch
// the credentials that passkey has already minted. Those are separate
// records with their own twelve-hour lifetimes (ADR-016 decision 3), and
// ending them is what sign_out_login is for — the tab says so at the point
// of the click rather than leaving the operator to infer it.
func ipcRevokePasskey(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcPasskeyIDMsg](raw, MsgRevokePasskey)
	if !ok || msg.ID == "" {
		return
	}
	removed, err := ctx.LoginOps.RevokePasskey(ctx.Ctx, msg.ID)
	if err != nil {
		ctx.UI.EmitEvent("onPasskeyError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onPasskeyRevoked", removed.ID, removed.Name)
}

func ipcSignOutLogin(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcPasskeyIDMsg](raw, MsgSignOutLogin)
	if !ok || msg.ID == "" {
		return
	}
	removed, err := ctx.LoginOps.SignOut(msg.ID)
	if err != nil {
		ctx.UI.EmitEvent("onPasskeyError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onLoginSessionRevoked", removed.ID, removed.Name)
}
