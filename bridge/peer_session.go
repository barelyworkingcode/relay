package bridge

import (
	"net"

	"relaygo/presence"
)

// PeerCallerSession is the production resolver for a bridge connection's
// presence.CallerSession (§6.6): PeerGraphicAccess's kernel-attested fact
// about conn's PEER, translated into what Gate.Request reads off the
// context. Like PeerPID, it describes the far end of the socket, never
// relay's own process — the tray is always in the Aqua session with
// graphic access, so asking about self would always answer yes and fail
// open.
//
// Every branch that cannot determine the peer's session — conn is not a
// *net.UnixConn, its underlying fd cannot be read, or the kernel probe
// itself errors — resolves to GraphicAccess: false, never to leaving the
// context untouched. presence.WithCallerSession's own doc comment is
// explicit about why: an UNDETERMINED peer must refuse, not fall through
// to Gate.Request's "no session on the context at all" branch. That branch
// exists for doors that genuinely have no peer to ask about — the WebView
// IPC, the tray menu, the loopback TCP mux — and none of those ever dial
// this socket, so every connection handleConn accepts gets a real
// CallerSession, determined or not.
func PeerCallerSession(conn net.Conn) presence.CallerSession {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return presence.CallerSession{GraphicAccess: false}
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return presence.CallerSession{GraphicAccess: false}
	}
	var graphic bool
	var probeErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		graphic, probeErr = presence.PeerGraphicAccess(int(fd))
	}); ctrlErr != nil || probeErr != nil {
		return presence.CallerSession{GraphicAccess: false}
	}
	return presence.CallerSession{GraphicAccess: graphic}
}
