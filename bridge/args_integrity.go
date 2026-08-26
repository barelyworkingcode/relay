package bridge

import "context"

type argsSHA256CtxKey struct{}

// WithArgsSHA256 carries a remote caller's argument hash for this one call.
// Carried in the context for the reason WithRemoteCaller gives: ToolRouter is
// implemented across repos and must not grow a parameter for every piece of
// caller metadata (ADR-008 Consequences).
func WithArgsSHA256(ctx context.Context, sum string) context.Context {
	if sum == "" {
		return ctx
	}
	return context.WithValue(ctx, argsSHA256CtxKey{}, sum)
}

// ArgsSHA256FromContext returns the hash the client sent, if any.
func ArgsSHA256FromContext(ctx context.Context) string {
	s, _ := ctx.Value(argsSHA256CtxKey{}).(string)
	return s
}
