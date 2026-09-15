package modelbroker

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// buildNearCapChatBody builds a realistic chat/completions-shaped JSON body
// (a "model" field plus a long "messages" array of ordinary text) of
// approximately targetSize bytes — the same shape the relay#116 re-review's
// 60 MiB/766 MiB measurement used, scaled to JSONBodyCap.
func buildNearCapChatBody(targetSize int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"vCode","messages":[`)
	chunk := strings.Repeat("a", 900)
	first := true
	for b.Len() < targetSize-64 {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, `{"role":"user","content":%q}`, chunk)
	}
	b.WriteString(`],"stream":true}`)
	return b.Bytes()
}

// TestExtractAndRewrite_AllocationStaysBoundedAtCap is S6's regression
// guard (relay#116 re-review): extraction and rewrite together must not
// amplify a body's size past a documented, generous multiplier. The
// re-review measured roughly 13x on the OLD 64 MiB cap and unbounded body
// (before ExtractJSONModelFromBytes removed a redundant full copy); this
// asserts a loose ceiling on the CURRENT JSONBodyCap so a future change that
// reintroduces an extra full-body copy (each one adds another 1x) fails
// here instead of only showing up as memory pressure in production.
func TestExtractAndRewrite_AllocationStaysBoundedAtCap(t *testing.T) {
	const allowedMultiplier = 15 // generous headroom over the ~10-11x measured today
	body := buildNearCapChatBody(JSONBodyCap - 4096)
	if len(body) >= JSONBodyCap {
		t.Fatalf("test body of %d bytes is not under JSONBodyCap %d", len(body), JSONBodyCap)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	// Mirrors model_endpoint.go's serveModelRoute: the caller already holds
	// bodyBytes (as if from readCapped) and passes it straight to the bytes
	// entry point, then rewrites in place — never via the reader-based
	// ExtractJSONModel, which would make its own extra copy of bytes the
	// caller already has.
	model, err := ExtractJSONModelFromBytes(body)
	if err != nil {
		t.Fatalf("ExtractJSONModelFromBytes: %v", err)
	}
	out, err := RewriteJSONModel(body, model)
	if err != nil {
		t.Fatalf("RewriteJSONModel: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("rewritten body is empty")
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(len(body)) * allowedMultiplier
	if allocated > limit {
		t.Fatalf("extract+rewrite of a %d-byte body allocated %d bytes (%.1fx), want <= %dx (%d bytes)",
			len(body), allocated, float64(allocated)/float64(len(body)), allowedMultiplier, limit)
	}
	t.Logf("body=%d bytes, allocated=%d bytes (%.1fx)", len(body), allocated, float64(allocated)/float64(len(body)))
}
