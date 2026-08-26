package main

import (
	"bytes"
	"encoding/json"
)

// Verbatim JSON encoding for the wire.
//
// ADR-012: relay forwards a caller's tool arguments as bytes. Two habits of
// encoding/json get in the way of that, and both are handled here:
//
//   - `json.Marshal` on a `json.RawMessage` COMPACTS it, and compaction with
//     the default settings rewrites `<`, `>` and `&` as the escapes
//     `\u003c`, `\u003e`, `\u0026`, and the two Unicode line separators as
//     `\u2028`/`\u2029`. None of that is lossy — it decodes back to the same
//     document — but it is still relay editing a payload it has no business
//     editing, and it makes "what relay sent is what the client sent"
//     untestable as a byte equality.
//   - Decoding a JSON string into a Go `string` IS lossy: an unpaired UTF-16
//     surrogate, which JSON permits and `\ud800` spells, becomes U+FFFD. That
//     one is handled by never decoding (see ExternalMcpManager.CallTool);
//     these helpers exist so the bytes that survive that decision also survive
//     the encode on the way out.
//
// What remains after this is insignificant whitespace, so the guarantee relay
// makes is that the arguments it sends are `json.Compact` of the arguments it
// received. Compaction is not optional: the stdio transport is
// newline-delimited, and a caller's pretty-printed arguments would break the
// frame. No MCP can observe the difference.
//
// One measured exception, pinned by
// TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged: Go's encoder
// re-spells a raw U+2028/U+2029 as `\u2028`/`\u2029` even with escaping off,
// and there is no seam short of hand-assembling the frame. It decodes back to
// the same character, which is the line ADR-012 draws — relay may not change
// what a document means, and does not claim to preserve how it was spelled.
func marshalJSONVerbatim(v interface{}) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode always terminates with exactly one newline; a JSON-RPC frame
	// supplies its own.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// mustMarshalJSONString encodes a string relay itself produced — a tool name,
// a progress token — as a JSON string. These are relay's own values, never a
// caller's payload, so a Go `string` is the honest representation of them and
// the lossiness that motivates this file does not apply. Encoding a string
// cannot fail, so the error is dropped rather than propagated to callers that
// would have nothing to do with it.
func mustMarshalJSONString(s string) json.RawMessage {
	out, err := marshalJSONVerbatim(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return out
}
