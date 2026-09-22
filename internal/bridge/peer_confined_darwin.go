//go:build darwin

package bridge

/*
#include <dlfcn.h>
#include <stdint.h>
#include <string.h>

// audit_token_t has no header on this system: it is a private but
// stable-shaped 8-word struct (32 bytes total) that every kernel API taking
// one, public or private, agrees on. sandbox_check_by_audit_token itself is
// undocumented too — no header, no framework — but is resolved by symbol
// name at runtime rather than linked, so a future OS that removes it fails
// the dlsym below instead of failing to build.
typedef struct { uint32_t val[8]; } relay_audit_token_t;
typedef int (*relay_sandbox_check_fn_t)(relay_audit_token_t token, const char *operation, int type, ...);

static relay_sandbox_check_fn_t relay_sandbox_check_fn = NULL;

// relay_resolve_sandbox_check dlopens libsandbox and resolves the symbol.
// The Go side calls this at most once, behind a sync.Once, never per
// connection. relay_sandbox_check_fn stays NULL on any failure, and every
// check below then reports "could not verify" rather than guessing.
static void relay_resolve_sandbox_check(void) {
	void *handle = dlopen("/usr/lib/libsandbox.1.dylib", RTLD_LAZY);
	if (handle == NULL) {
		return;
	}
	relay_sandbox_check_fn = (relay_sandbox_check_fn_t)dlsym(handle, "sandbox_check_by_audit_token");
}

// SANDBOX_FILTER_NONE is 0 in the private libsandbox ABI: the coarse
// "confined by anything at all" check, which needs no operation or path
// argument to go with it.
#define RELAY_SANDBOX_FILTER_NONE 0

// relay_sandbox_check_by_token returns 1 (confined), 0 (not confined), or -1
// (the symbol never resolved). The Go caller must fail closed on -1, never
// read it as "not confined".
static int relay_sandbox_check_by_token(const unsigned char *raw) {
	if (relay_sandbox_check_fn == NULL) {
		return -1;
	}
	relay_audit_token_t token;
	memcpy(&token, raw, sizeof(token));
	return relay_sandbox_check_fn(token, NULL, RELAY_SANDBOX_FILTER_NONE) > 0 ? 1 : 0;
}
*/
import "C"

import (
	"log/slog"
	"sync"
	"unsafe"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

var resolveSandboxCheckOnce sync.Once

// warnSandboxCheckUnavailableOnce fires at most once no matter how many
// connections hit an unresolved symbol, so a future OS dropping
// sandbox_check_by_audit_token degrades to refusing every relay-sandbox
// attach without flooding the log once per request.
var warnSandboxCheckUnavailableOnce sync.Once

// PeerConfined reports whether tok's process is, right now, confined by a
// Seatbelt sandbox. It is the real, darwin implementation of
// PeerConfinedFunc: see that type's doc for the contract callers rely on.
func PeerConfined(tok peertoken.Token) (confined bool, ok bool) {
	resolveSandboxCheckOnce.Do(func() { C.relay_resolve_sandbox_check() })
	if !tok.Valid() {
		return false, false
	}
	raw := tok.Raw()
	switch C.relay_sandbox_check_by_token((*C.uchar)(unsafe.Pointer(&raw[0]))) {
	case 1:
		return true, true
	case 0:
		return false, true
	default:
		warnSandboxCheckUnavailableOnce.Do(func() {
			slog.Error("bridge: sandbox_check_by_audit_token unresolved; refusing every relay-sandbox attach until relay restarts")
		})
		return false, false
	}
}
