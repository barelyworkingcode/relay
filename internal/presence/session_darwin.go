//go:build darwin

package presence

/*
#cgo LDFLAGS: -lbsm
#include <bsm/audit.h>
#include <bsm/libbsm.h>
#include <string.h>
#include <errno.h>

// auditon is deprecated since macOS 11, but A_GETSINFO_ADDR has no
// replacement and this exact call chain was measured working on 26.4
// (ADR-017 implementation spec §6.6.1). The pragma is scoped to this one
// function, not module-wide, so the next deprecated call anyone adds
// elsewhere still warns.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

static int relay_token_graphic_access(const unsigned char *raw, int *graphic) {
	audit_token_t token;
	memcpy(&token, raw, sizeof(token));
	au_asid_t asid = audit_token_to_asid(token);
	auditinfo_addr_t info;
	memset(&info, 0, sizeof(info));
	info.ai_asid = asid;
	if (auditon(A_GETSINFO_ADDR, &info, sizeof(info)) != 0) {
		return errno;
	}
	*graphic = (info.ai_flags & AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS) ? 1 : 0;
	return 0;
}

#pragma clang diagnostic pop
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

// PeerGraphicAccess reports whether the peer connected on fd belongs to a
// kernel audit session with console (graphic) access. fd must be the
// accepted connection's own file descriptor — the PEER's, never relay's own
// process (§6.6): the tray is always in the Aqua session, so probing self
// would always answer yes and fail open.
//
// The mechanism is unprivileged and unforgeable: setaudit_addr to join or
// forge a session returns EPERM for an unprivileged uid, and sudo inherits
// the caller's audit session. No caller-supplied environment variable plays
// any part here — this reads a kernel-attested property of the peer, never
// anything the peer asserts about itself.
func PeerGraphicAccess(fd int) (bool, error) {
	tok, err := peertoken.FromFD(fd)
	if err != nil {
		return false, fmt.Errorf("presence: determining peer session: %w", err)
	}
	raw := tok.Raw()
	var graphic C.int
	if errno := C.relay_token_graphic_access((*C.uchar)(unsafe.Pointer(&raw[0])), &graphic); errno != 0 {
		return false, fmt.Errorf("presence: determining peer session: %w", syscall.Errno(errno))
	}
	return graphic != 0, nil
}
