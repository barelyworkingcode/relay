package modelbroker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
)

// RewriteJSONModel replaces body's "model" field with canonical, leaving
// every other byte-for-byte value untouched via json.RawMessage. The
// caller (model_endpoint.go) calls this on EVERY request, never forwarding
// the original bytes even when canonical equals the requested spelling: the
// output is relay's own, single-key rendering of "model", never a copy of
// whatever the caller's body happened to contain there.
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
//
// Every key that case-folds to "model" is dropped before the canonical one
// is written back, not just the literal "model" key. ExtractJSONModel
// already refuses a body carrying more than one such key
// (ErrDuplicateModelKey), so in the ordinary path there is at most one to
// drop — this is the second half of that defence, not a second occasion for
// it: a caller of this function that skipped extraction (there is none
// today) still cannot produce a body relayLLM's case-insensitive decode
// could read two ways.
func RewriteJSONModel(body []byte, canonical string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("modelbroker: rewrite json model: %w", err)
	}
	for k := range fields {
		if foldsToModel(k) {
			delete(fields, k)
		}
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

// RewriteMultipartModel re-streams a multipart/form-data body, replacing
// EVERY part whose name case-folds to "model" with one canonical "model"
// field, and copying every other part — including the audio file, whatever
// its size — through unread into the new body. Returns the rewritten body
// and the Content-Type value (carrying the new writer's own boundary) the
// caller must set on the forwarded request, since the original boundary
// string does not survive into the copy.
//
// Like RewriteJSONModel, the caller (model_endpoint.go) calls this on every
// audio request, never forwarding the original parts unchanged.
// ExtractMultipartModel already refuses more than one part folding to
// "model" (ErrDuplicateModelPart), so this drops at most one original part
// in the ordinary path — the fold match here, not an exact one, is this
// function's own half of that same defence.
func RewriteMultipartModel(r io.Reader, boundary, canonical string) ([]byte, string, error) {
	mr := multipart.NewReader(r, boundary)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	wroteModel := false

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
		}
		if foldsToModel(part.FormName()) && part.FileName() == "" {
			// This is deliberate: drain and discard the original value
			// rather than copying it — the whole point of this function is
			// that the value written to the new part is canonical, not
			// part's bytes. Written at most once even if more than one
			// original part matches (defence in depth; the extractor
			// already refuses that case before this function is reached).
			_, _ = io.Copy(io.Discard, part)
			part.Close()
			if wroteModel {
				continue
			}
			if err := mw.WriteField("model", canonical); err != nil {
				return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
			}
			wroteModel = true
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
	if !wroteModel {
		if err := mw.WriteField("model", canonical); err != nil {
			return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("modelbroker: rewrite multipart model: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
