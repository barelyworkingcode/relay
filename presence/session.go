package presence

import "context"

// CallerSession is what the gate knows about the kernel audit session the
// calling process belongs to. It always describes the PEER of a connection,
// resolved once per connection alongside bridge.PeerPID — never relay's own
// session: the tray is always in the Aqua session with graphic access, so a
// check that asked about relay itself would always answer yes and fail open
// (§6.6).
type CallerSession struct {
	// GraphicAccess reports the peer's AU_SESSION_FLAG_HAS_GRAPHIC_ACCESS
	// bit. It answers "can this process display UI?", not "did this come
	// from SSH?" — `open -a` from an SSH shell launches into the console
	// session and reports true. That is a named limitation, not a
	// boundary: the boundary is the password inside the prompt.
	GraphicAccess bool
}

type callerSessionKey struct{}

// WithCallerSession attaches sess to ctx. Called once per connection, at the
// point bridge.PeerPID is already resolved — never per gated call.
//
// A door with no peer to ask about (the WebView IPC, the tray menu, the
// loopback TCP mux) must leave ctx untouched rather than invent a value:
// Gate.Request treats "no CallerSession on the context at all" as "prompt",
// which is correct for those doors. A peer whose session could NOT be
// determined is a different case and must not be folded into that one —
// attach CallerSession{GraphicAccess: false} instead, which refuses the same
// as a confirmed non-graphic session. Defaulting an undetermined peer to
// "prompt" would reopen the exact console prompt-spam this mechanism exists
// to prevent.
func WithCallerSession(ctx context.Context, sess CallerSession) context.Context {
	return context.WithValue(ctx, callerSessionKey{}, sess)
}

// CallerSessionFromContext reports the session WithCallerSession attached,
// if any.
func CallerSessionFromContext(ctx context.Context) (CallerSession, bool) {
	sess, ok := ctx.Value(callerSessionKey{}).(CallerSession)
	return sess, ok
}
