package modelbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
)

// JSONBodyCap bounds a non-audio route's request body. This is deliberately
// smaller than relayLLM's own maxProxyBodyBytes (64 MiB): extracting and
// rewriting a JSON body's "model" field (ExtractJSONModel, RewriteJSONModel)
// together amplify a body's size several-fold in live allocations — a
// pathologically nested but otherwise ordinary near-cap body measured at
// roughly 11x at 64 MiB, i.e. hundreds of MiB of GC pressure per request
// from a single caller (relay#116 re-review, S6). Every route this cap
// applies to (chat/completions, responses, messages, embeddings) carries
// text, not binary payloads, so 16 MiB is generous headroom over any real
// conversation history while keeping worst-case amplification in the tens
// of MiB, not hundreds. See docs/model-endpoint.md's Limits section for the
// combined bound with bodyBudget (model_endpoint.go).
const JSONBodyCap = 16 << 20

// AudioMultipartCap matches relayLLM's maxTranscriptionBytes: audio uploads
// are bounded far tighter than the general JSON cap.
const AudioMultipartCap = 25 << 20

// maxModelFieldBytes bounds a single extracted "model" value. Model ids are
// short (well under a hundred bytes in every real catalog); a pathologically
// long one is refused rather than copied in full — this is deliberate, not a
// realistic id ever needs it.
const maxModelFieldBytes = 4096

var (
	// ErrBodyTooLarge is returned when a body exceeds the caller-supplied
	// cap. It is a sentinel so callers can map it to the 413 the spec's
	// oversize-body tests expect without string-matching an error.
	ErrBodyTooLarge = errors.New("modelbroker: body exceeds cap")
	// ErrModelFieldMissing means the body parsed cleanly but named no model.
	ErrModelFieldMissing = errors.New("modelbroker: no model field")
	// ErrNotJSONObject means the body's top level isn't a JSON object, so
	// there is no "model" key to find.
	ErrNotJSONObject = errors.New("modelbroker: body is not a JSON object")
	// ErrDuplicateModelKey means more than one top-level JSON key case-folds
	// to "model". This is deliberate, not overcautious: encoding/json's own
	// field matching is case-insensitive and takes the LAST matching key it
	// sees, so relayLLM's decode of {"model":"a","Model":"b"} resolves to
	// "b" while a check run against only the first "model" key would have
	// approved "a" — refusing outright is the only answer that can't be
	// smuggled past by a caller picking whichever spelling relay checks and
	// whichever the checked-and-forwarded body decodes to.
	ErrDuplicateModelKey = errors.New("modelbroker: more than one key names \"model\"")
	// ErrDuplicateModelPart is ErrDuplicateModelKey's multipart counterpart:
	// more than one form part's name case-folds to "model".
	ErrDuplicateModelPart = errors.New("modelbroker: more than one part names \"model\"")
	// ErrModelFieldTooLong means the model value itself, not the body as a
	// whole, exceeds maxModelFieldBytes. Refused outright rather than
	// silently checked-and-forwarded truncated: a truncated value that
	// happens to match an allowed model's prefix must never be the value a
	// grant decision is made against while the full, different value is
	// what actually reaches the upstream.
	ErrModelFieldTooLong = errors.New("modelbroker: model field exceeds the length limit")
	// ErrTrailingData means the body decoded a complete top-level JSON
	// object but had further non-whitespace bytes after its closing brace.
	// Caught here rather than left for RewriteJSONModel's own
	// json.Unmarshal (which also rejects trailing data) so the caller gets
	// this package's own shape-appropriate 400, not that later call's
	// generic decode error.
	ErrTrailingData = errors.New("modelbroker: trailing data after the JSON object")
)

// foldsToModel reports whether key matches "model" under the same
// case-insensitive comparison encoding/json's own field matching uses
// (Unicode simple case folding — strings.EqualFold implements the same
// rule for the ASCII-and-simple-fold cases every real "model" spelling
// falls into).
func foldsToModel(key string) bool {
	return strings.EqualFold(key, "model")
}

// capReader wraps r so a read attempted once limit bytes have already been
// delivered fails with ErrBodyTooLarge, rather than silently truncating or
// returning a bare EOF a caller could mistake for "body ended early, no
// model". It does not itself decide where the cap sits — see ExtractJSONModel
// and ExtractMultipartModel for why they pass different limits.
type capReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n >= c.limit {
		return 0, ErrBodyTooLarge
	}
	if room := c.limit - c.n; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ExtractJSONModel reads the top-level "model" field from a JSON request
// body of at most maxBytes. The body is first read whole into memory, capped
// at maxBytes — this is deliberate, not a missed optimisation: a decoder
// reading lazily from the wire would stop as soon as the JSON value closes
// and would never notice trailing bytes past the cap, so the cap has to be
// enforced by an eager, bounded read rather than by however far the decoder
// happens to look.
//
// A caller that has already read and size-capped the body itself (as
// model_endpoint.go does, needing the raw bytes again afterward for
// RewriteJSONModel) should call ExtractJSONModelFromBytes directly instead
// — going through a Reader here would make a second full copy of bytes the
// caller already holds, doubling exactly the allocation this function's own
// cap exists to bound (relay#116 re-review, S6).
func ExtractJSONModel(r io.Reader, maxBytes int64) (string, error) {
	body, err := io.ReadAll(&capReader{r: r, limit: maxBytes + 1})
	if err != nil {
		return "", wrapReadErr(err)
	}
	if int64(len(body)) > maxBytes {
		return "", ErrBodyTooLarge
	}
	return extractJSONModelBytes(body)
}

// ExtractJSONModelFromBytes is ExtractJSONModel's entry point for a caller
// that already holds the whole, size-capped body as a []byte. See
// ExtractJSONModel's doc for why this skips a redundant copy.
func ExtractJSONModelFromBytes(body []byte) (string, error) {
	return extractJSONModelBytes(body)
}

// extractJSONModelBytes is walked with a token-by-token decode so no field's
// value other than "model" is ever assembled into a Go value the caller
// could retain or log — a canary planted anywhere else in the body (a
// message, a tool result) never reaches the return value.
//
// The WHOLE object is walked, never returned early on the first match: a
// second key that case-folds to "model" is a request built to disagree with
// itself about what model it names, refused via ErrDuplicateModelKey rather
// than resolved by picking a side (see that error's own comment for why).
// The caller must not forward the original bytes even when there is exactly
// one match — see RewriteJSONModel.
func extractJSONModelBytes(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// This is subtle: without UseNumber, the decoder converts every number
	// literal it walks past — including ones nowhere near "model" — to
	// float64 via strconv.ParseFloat, which errors on a value out of
	// float64's range (a bare "1e400" is valid JSON but not a valid
	// float64). skipJSONValue would surface that as a refusal for a field
	// this function never even looks at, rejecting a body relayLLM's own
	// (untyped) decode would accept. UseNumber makes Token() hand back the
	// literal's text as json.Number instead, which never fails to parse.
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return "", wrapReadErr(err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", ErrNotJSONObject
	}

	model, found := "", false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", wrapReadErr(err)
		}
		key, _ := keyTok.(string)
		if !foldsToModel(key) {
			if err := skipJSONValue(dec); err != nil {
				return "", wrapReadErr(err)
			}
			continue
		}
		// This is subtle: a second key that folds to "model" is refused
		// rather than resolved by "first wins" or "last wins" — see
		// ErrDuplicateModelKey. The whole object is still walked (not
		// returned early) so a duplicate anywhere in the body is caught
		// regardless of which occurrence relay would otherwise have used.
		if found {
			return "", ErrDuplicateModelKey
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return "", wrapReadErr(err)
		}
		if len(v) > maxModelFieldBytes {
			return "", ErrModelFieldTooLong
		}
		model, found = v, true
	}
	if !found {
		return "", ErrModelFieldMissing
	}

	// The closing '}': dec.More() returned false the instant it saw it
	// without consuming it, so it must be read explicitly before checking
	// for trailing data.
	if _, err := dec.Token(); err != nil {
		return "", wrapReadErr(err)
	}
	// This is deliberate: a value after the object closes is refused here,
	// before the grant check, rather than left for RewriteJSONModel's own
	// json.Unmarshal to reject later — that call's error currently has no
	// caller-facing shape and was surfacing as a bare 500. Nothing past the
	// object was ever inspected above, so this is the first point simple
	// enough to check.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", ErrTrailingData
		}
		return "", wrapReadErr(err)
	}
	return model, nil
}

// skipJSONValue consumes exactly one JSON value from dec without retaining
// it: a scalar is already fully consumed by the Token() call that read it,
// and an object or array is drained by depth-counting further delimiter
// tokens. Nothing skipped here is copied into a variable that outlives this
// call.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim == '}' || delim == ']' {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func wrapReadErr(err error) error {
	if errors.Is(err, ErrBodyTooLarge) {
		return ErrBodyTooLarge
	}
	return fmt.Errorf("modelbroker: decode body: %w", err)
}

// ExtractMultipartModel reads the "model" form field from a multipart/
// form-data body (the audio routes' shape) of at most maxBytes total, per
// mime.ParseMediaType's boundary parameter. Every other part — in
// particular the audio file itself — is never read into memory: NextPart
// discards an unread part's remaining bytes before returning the next one,
// so this function's only allocation for those parts is the part header.
//
// A second part whose name case-folds to "model" is refused
// (ErrDuplicateModelPart), the multipart mirror of ExtractJSONModel's
// ErrDuplicateModelKey: relayLLM's own multipart parsing keeps the LAST
// same-named part, so a check run against only the first would approve a
// value the upstream never actually uses.
func ExtractMultipartModel(r io.Reader, boundary string, maxBytes int64) (string, error) {
	cr := &capReader{r: r, limit: maxBytes + 1}
	mr := multipart.NewReader(cr, boundary)

	model, found := "", false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", wrapReadErr(err)
		}
		if !foldsToModel(part.FormName()) || part.FileName() != "" {
			// This is deliberate: keep draining to EOF regardless, rather
			// than returning early. NextPart discards an unread part's
			// remaining bytes itself — the file part's content never
			// reaches a variable here either way — but only draining every
			// part enforces maxBytes over the whole body regardless of
			// which part "model" happens to be, matching the JSON
			// extractor's cap on the request as a whole.
			part.Close()
			continue
		}
		if found {
			part.Close()
			return "", ErrDuplicateModelPart
		}
		b, err := io.ReadAll(io.LimitReader(part, maxModelFieldBytes+1))
		part.Close()
		if err != nil {
			return "", wrapReadErr(err)
		}
		if int64(len(b)) > maxModelFieldBytes {
			return "", ErrModelFieldTooLong
		}
		model, found = string(b), true
	}
	// A well-formed multipart body whose total size lands exactly on the cap
	// can finish parsing (find its closing boundary) without capReader's own
	// mid-read check ever firing — the same lazy-decoder gap ExtractJSONModel
	// avoids by reading eagerly. Checking bytes actually consumed here closes
	// it for the multipart path too.
	if cr.n > maxBytes {
		return "", ErrBodyTooLarge
	}
	if !found {
		return "", ErrModelFieldMissing
	}
	return model, nil
}
