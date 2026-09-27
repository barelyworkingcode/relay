//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework ApplicationServices -framework Foundation -framework IOKit -framework Security
#include "dialog_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

const (
	maxAgentWindows = 32
	detailBufLen    = 256
	textBufLen      = 8192
)

// agentWindows lists the on-screen windows owned by a process that is, or
// claims to be, the LocalAuthentication agent. Ownership is not yet verified.
func agentWindows() ([]laDialog, error) {
	var buf [maxAgentWindows]C.dbp_window
	n := int(C.dbp_list_windows(&buf[0], maxAgentWindows))
	switch {
	case n < 0:
		return nil, errors.New("the window server returned no window list")
	case n > maxAgentWindows:
		return nil, fmt.Errorf("%d LocalAuthentication windows are open, more than this helper inspects", n)
	}
	out := make([]laDialog, n)
	for i := range out {
		out[i] = laDialog{Window: uint32(buf[i].window), PID: int(buf[i].pid)}
	}
	return out, nil
}

func verifyOwner(pid int) (bool, string) {
	var detail [detailBufLen]C.char
	ok := C.dbp_owner_ok(C.int32_t(pid), &detail[0], detailBufLen) != 0
	return ok, C.GoString(&detail[0])
}

// dialogText reports false when the window cannot be reached through AX.
func dialogText(d laDialog) (string, bool) {
	buf := make([]C.char, textBufLen)
	n := C.dbp_window_text(C.int32_t(d.PID), C.uint32_t(d.Window), &buf[0], textBufLen)
	if n < 0 {
		return "", false
	}
	return C.GoString(&buf[0]), true
}

type sessionState struct {
	OnConsole, Locked, AXTrusted, PostEvents, WindowList bool
}

func probeSession() sessionState {
	var st C.dbp_session
	C.dbp_probe_session(&st)
	return sessionState{
		OnConsole:  st.on_console != 0,
		Locked:     st.locked != 0,
		AXTrusted:  st.ax_trusted != 0,
		PostEvents: st.post_events != 0,
		WindowList: st.window_list != 0,
	}
}

// answerDialog reports false only when it typed nothing.
func answerDialog(d laDialog, pw []uint16) (bool, string) {
	if len(pw) == 0 {
		return false, "empty password"
	}
	var detail [detailBufLen]C.char
	ok := C.dbp_answer(C.int32_t(d.PID), C.uint32_t(d.Window),
		(*C.uint16_t)(unsafe.Pointer(&pw[0])), C.int(len(pw)), &detail[0], detailBufLen) != 0
	return ok, C.GoString(&detail[0])
}

func cancelDialog(d laDialog) (bool, string) {
	var detail [detailBufLen]C.char
	ok := C.dbp_cancel(C.int32_t(d.PID), C.uint32_t(d.Window), &detail[0], detailBufLen) != 0
	return ok, C.GoString(&detail[0])
}
