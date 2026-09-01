package mcpbroker

import (
	"bytes"
	"encoding/json"
)

// marshalJSONVerbatim encodes v without compacting or HTML-escaping the
// output, so a json.RawMessage argument round-trips byte-for-byte instead of
// being rewritten by json.Marshal's default escaping -- relay forwards a
// caller's tool arguments and must not edit them (ADR-012). Go's encoder
// still re-spells a raw U+2028/U+2029 as \u2028/\u2029 even with escaping
// off; there is no seam around that, and it decodes back to the same
// character (see TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged).
func marshalJSONVerbatim(v interface{}) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a trailing newline; trimmed since the caller's frame supplies its own.
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// mustMarshalJSONString encodes a string relay produced itself (not a
// caller's payload), so encoding cannot fail; the error is dropped rather
// than propagated to callers that have nothing to do with it.
func mustMarshalJSONString(s string) json.RawMessage {
	out, err := marshalJSONVerbatim(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return out
}
