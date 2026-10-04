package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// TraceHeader carries the trace ID on HTTP and WebSocket handshakes. It is
// deliberately not in the x-relay-* family, which the model path strips.
const TraceHeader = "X-Trace-Id"

type traceKey struct{}

// NewTraceID returns 32 lowercase hex characters from crypto/rand.
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the platform is unusable; a log line
		// must not take the process down over it.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// ValidTraceID reports whether id matches [A-Za-z0-9_-]{8,64}. Inbound IDs are
// untrusted, so this is the only gate between a caller and a log line.
func ValidTraceID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// TraceIDOrNew keeps a valid inbound ID and otherwise generates one. The
// rejected value is never logged: it is attacker-controlled.
func TraceIDOrNew(id string) string {
	if ValidTraceID(id) {
		return id
	}
	return NewTraceID()
}

// ContextWithTrace stores id in ctx. An invalid id leaves ctx unchanged.
func ContextWithTrace(ctx context.Context, id string) context.Context {
	if !ValidTraceID(id) {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, id)
}

// TraceFromContext returns the stored trace ID, or "" when there is none.
func TraceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}
