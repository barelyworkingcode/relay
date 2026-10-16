package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The Allow Enrolment sheet's IPC doors. Thin adapters over the three cores
// that own the three targets, the way ipc_login.go is over LoginOps: each
// target keeps its own presence gate, and nothing here decides anything.

const (
	MsgOpenEnrolmentWindow   = "open_enrolment_window"
	MsgCloseEnrolmentWindows = "close_enrolment_windows"
)

// ipcOpenEnrolmentWindowMsg names the target to open. ProfileID is read for
// the profile target only.
type ipcOpenEnrolmentWindowMsg struct {
	// Type is the IPC envelope's own tag, which onSettingsIpc passes through
	// with the body. It is ignored; every other unknown field is refused.
	Type      string `json:"type"`
	Target    string `json:"target"`
	ProfileID string `json:"profile_id"`
}

type ipcCloseEnrolmentWindowsMsg struct {
	Type string `json:"type"`
}

type ipcEnrolmentWindowError struct {
	Error string `json:"error"`
}

// decodeIPCStrict refuses unknown fields: a body that names something this
// door does not read must not be taken as one it did.
func decodeIPCStrict[T any](raw json.RawMessage) (*T, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var msg T
	if err := dec.Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ipcOpenEnrolmentWindow opens one target. The cores are gated, and a gated
// call blocks on LocalAuthentication's completion handler, which needs the
// run loop pumped, so the open runs off the main thread and every UI touch
// hops back to it.
func ipcOpenEnrolmentWindow(ctx *IPCContext, raw json.RawMessage) {
	msg, err := decodeIPCStrict[ipcOpenEnrolmentWindowMsg](raw)
	if err != nil {
		dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: fmt.Sprintf("%s: invalid request: %v", MsgOpenEnrolmentWindow, err)})
		return
	}
	target := strings.TrimSpace(msg.Target)
	profileID := strings.TrimSpace(msg.ProfileID)
	switch {
	case target != enrolmentWindowTargetProfile && target != enrolmentWindowTargetHomeWork && target != enrolmentWindowTargetRelayLogin:
		dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: fmt.Sprintf("%q is not an enrolment target", msg.Target)})
		return
	case target == enrolmentWindowTargetProfile && profileID == "":
		dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: "choose an access profile"})
		return
	case target != enrolmentWindowTargetProfile && profileID != "":
		dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: "an access profile applies to the access-profile target only"})
		return
	}

	ctx.GoFunc(func() {
		var emitName string
		var payload any
		var err error
		switch target {
		case enrolmentWindowTargetHomeWork:
			emitName, payload, err = openHomeWorkWindow(ctx)
		case enrolmentWindowTargetRelayLogin:
			emitName, payload, err = openRelayLoginCode(ctx)
		default:
			emitName, payload, err = openProfileWindow(ctx, profileID)
		}
		if err != nil {
			dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: err.Error()})
			return
		}
		ctx.Platform.DispatchToMain(func() {
			ctx.UI.EmitEvent(emitName, payload)
			ctx.UpdateMenu()
		})
	})
}

func windowCore(ctx *IPCContext) *EnrolmentWindowOps {
	if ctx.EnrolmentOps == nil {
		return nil
	}
	return ctx.EnrolmentOps.Window
}

func openHomeWorkWindow(ctx *IPCContext) (string, any, error) {
	eve := runningEveEnrolmentOps()
	if eve == nil {
		return "", nil, errEveEnrolmentOpsUnavailable
	}
	view, err := eve.Open(ctx.Ctx, auditViaIPC)
	if err != nil {
		return "", nil, err
	}
	return "onEnrolmentWindowOpened", enrolmentWindowView{Open: true, Target: enrolmentWindowTargetHomeWork, Expires: view.Expires}, nil
}

func openRelayLoginCode(ctx *IPCContext) (string, any, error) {
	if ctx.LoginOps == nil {
		return "", nil, errLoginOpsUnavailable
	}
	view, err := ctx.LoginOps.MintBootstrap(ctx.Ctx, auditViaIPC)
	if err != nil {
		return "", nil, err
	}
	return "onLoginCodeMinted", view, nil
}

func openProfileWindow(ctx *IPCContext, profileID string) (string, any, error) {
	window := windowCore(ctx)
	if window == nil {
		return "", nil, errEnrolmentWindowOpsUnavailable
	}
	view, err := window.Open(ctx.Ctx, profileID, auditViaIPC)
	if err != nil {
		return "", nil, err
	}
	return "onEnrolmentWindowOpened", view, nil
}

// ipcCloseEnrolmentWindows closes every open target. Ungated, like the CLI
// door: closing narrows.
func ipcCloseEnrolmentWindows(ctx *IPCContext, raw json.RawMessage) {
	if _, err := decodeIPCStrict[ipcCloseEnrolmentWindowsMsg](raw); err != nil {
		dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: fmt.Sprintf("%s: invalid request: %v", MsgCloseEnrolmentWindows, err)})
		return
	}
	ctx.GoFunc(func() {
		window := windowCore(ctx)
		if window == nil {
			dispatchEmit(ctx, "onEnrolmentWindowError", ipcEnrolmentWindowError{Error: errEnrolmentWindowOpsUnavailable.Error()})
			return
		}
		closed, err := window.Close(ctx.Ctx, auditViaIPC)
		if closed == nil {
			closed = []string{}
		}
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onEnrolmentWindowError", ipcEnrolmentWindowError{Error: err.Error()})
			}
			ctx.UI.EmitEvent("onEnrolmentWindowsClosed", enrolmentWindowCloseResult{Closed: closed})
			ctx.UpdateMenu()
		})
	})
}
