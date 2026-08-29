//go:build darwin

package presence

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework LocalAuthentication -framework Foundation
#include "localauth_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Numeric LAError codes this provider classifies on (§6.5.1). Branching on
// anything else — including the message, which is indistinguishable between
// a wrong password and the undocumented -1000 measured in a sessionless
// context — is exactly the trap the spec names.
const (
	laErrorUserCancel = -2
)

type laResult struct {
	success bool
	code    int64
}

var (
	laMu   sync.Mutex
	laMap  = make(map[uintptr]chan laResult)
	laNext uintptr
)

func laRegister(ch chan laResult) uintptr {
	laMu.Lock()
	defer laMu.Unlock()
	laNext++
	id := laNext
	laMap[id] = ch
	return id
}

func laTake(id uintptr) chan laResult {
	laMu.Lock()
	defer laMu.Unlock()
	ch := laMap[id]
	delete(laMap, id)
	return ch
}

// relayPresenceLAResult is called back from localauth_darwin.m's completion
// block, on whatever queue LocalAuthentication chose — never assumed to be
// the goroutine that started the evaluation. A token with no registered
// channel (already delivered, or a stray call) is silently dropped rather
// than panicking: this is a boundary from C and must not be able to crash
// the process on a double-fire.
//
//export relayPresenceLAResult
func relayPresenceLAResult(token C.uintptr_t, success C.int, code C.longlong) {
	ch := laTake(uintptr(token))
	if ch == nil {
		return
	}
	ch <- laResult{success: success != 0, code: int64(code)}
}

// laInFlight tracks outstanding evaluations so the goroutine Evaluate spawns
// is a tracked one, per §6.5: evaluatePolicy must never run on the Cocoa
// main thread, which the tray's run loop owns and which the dialog needs.
var laInFlight sync.WaitGroup

var localAuthProviderConstructions atomic.Int64

// LocalAuthProviderConstructions reports how many times the real darwin
// provider's constructor has been called in this process. Production wiring
// calls it exactly once, in runTrayApp; the hermetic suite must never reach
// it at all (AC-20) — the developer-visible symptom of a regression here is
// a password dialog appearing during `go test`.
func LocalAuthProviderConstructions() int64 {
	return localAuthProviderConstructions.Load()
}

// LocalAuthProvider is the darwin Provider (§6.5): a login-password prompt
// via LAPolicyDeviceOwnerAuthentication, never
// deviceOwnerAuthenticationWithBiometrics — measured, the biometric policy
// fails -7 (BiometryNotEnrolled) on hardware with no Touch ID and no Secure
// Enclave, and biometryType is not a usable probe either (it misreports on
// this class of machine). There is no branch for biometry to fall back to.
type LocalAuthProvider struct{}

// NewLocalAuthProvider constructs the real provider. It must be called from
// exactly one place in production (runTrayApp) and never from the hermetic
// suite, which is why its calls are counted.
func NewLocalAuthProvider() *LocalAuthProvider {
	localAuthProviderConstructions.Add(1)
	return &LocalAuthProvider{}
}

// Evaluate must never be called from the thread that owns the Cocoa run
// loop: it blocks the calling goroutine on a channel until the async
// LocalAuthentication reply arrives, and if that goroutine is also the
// thread the dialog's run loop needs pumped, the two deadlock each other.
func (p *LocalAuthProvider) Evaluate(ctx context.Context, reason string) error {
	ch := make(chan laResult, 1)
	token := laRegister(ch)

	cReason := C.CString(reason)
	laInFlight.Add(1)
	go func() {
		defer laInFlight.Done()
		defer C.free(unsafe.Pointer(cReason))
		C.relay_la_evaluate(cReason, C.uintptr_t(token))
	}()

	select {
	case res := <-ch:
		return classifyLAResult(res)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// classifyLAResult is the whole of §6.5.1: branch on the numeric code alone,
// treat everything outside {success, -2 LAErrorUserCancel} as failure, and
// never give -1000 — or any other undocumented code — a path of its own. An
// implementation that special-cased -1000 would break the day Apple changes
// what that code means; this one has nothing to break.
func classifyLAResult(res laResult) error {
	if res.success {
		return nil
	}
	if res.code == laErrorUserCancel {
		return ErrRefused
	}
	return ErrRefused
}
