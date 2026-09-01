package bridge

import "context"

type argsSHA256CtxKey struct{}

// WithArgsSHA256 carries the hash in context rather than a parameter because
// ToolRouter is implemented across repos and must not grow a parameter for
// every piece of caller metadata (ADR-008 Consequences).
func WithArgsSHA256(ctx context.Context, sum string) context.Context {
	if sum == "" {
		return ctx
	}
	return context.WithValue(ctx, argsSHA256CtxKey{}, sum)
}

func ArgsSHA256FromContext(ctx context.Context) string {
	s, _ := ctx.Value(argsSHA256CtxKey{}).(string)
	return s
}
