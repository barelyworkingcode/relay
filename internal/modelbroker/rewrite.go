package modelbroker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
)

// RewriteJSONModel replaces body's top-level "model" field with canonical,
// leaving every other byte-for-byte value untouched via json.RawMessage.
//
// This is deliberate, not an ADR-012-style violation of "forward what the
// caller sent": canonicalize (normalise.go) accepts spellings — "llama/x",
// "pi/relay-router/x" — that exist only for the grant check, because
// relayLLM's router dispatches on the bare id alone and 400s on anything
// else. Forwarding the caller's original spelling after allowing it under a
// grant would make the ALLOW decision correct and the call itself fail.
// Object key order and whitespace are not preserved (unlike relay's tool-call
// path) because neither is meaningful to an HTTP JSON API the way MCP
// argument bytes are meaningful to `_meta.args_sha256`.
func RewriteJSONModel(body []byte, canonical string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("modelbroker: rewrite json model: %w", err)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("modelbroker: rewrite json model: %w", err)
	}
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("modelbroker: rewrite json model: %w", err)
	}
	return out, nil
}

// RewriteMultipartModel re-streams a multipart/form-data body, replacing the
// "model" field's value with canonical and copying every other part —
// including the audio file, whatever its size — through unread into the new
// body. Returns the rewritten body and the Content-Type value (carrying the
// new writer's own boundary) the caller must set on the forwarded request,
// since the original boundary string does not survive into the copy.
func RewriteMultipartModel(r io.Reader, boundary, canonical string) ([]byte, string, error) {
	mr := multipart.NewReader(r, boundary)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
		}
		if part.FormName() == "model" && part.FileName() == "" {
			// This is deliberate: drain and discard the original value rather
			// than copying it — the whole point of this function is that the
			// value written to the new part is canonical, not part's bytes.
			_, _ = io.Copy(io.Discard, part)
			if err := mw.WriteField("model", canonical); err != nil {
				part.Close()
				return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
			}
			part.Close()
			continue
		}
		w, err := mw.CreatePart(part.Header)
		if err != nil {
			part.Close()
			return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
		}
		if _, err := io.Copy(w, part); err != nil {
			part.Close()
			return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
		}
		part.Close()
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
