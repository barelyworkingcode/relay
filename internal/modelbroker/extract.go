package modelbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
)

// JSONBodyCap is the router's own body ceiling (relayLLM router.go's
// maxProxyBodyBytes), reused here so the broker refuses a request before
// relayLLM would ever see it.
const JSONBodyCap = 64 << 20

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
)

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
// happens to look. The buffer is then walked with a token-by-token decode so
// no field's value other than "model" is ever assembled into a Go value the
// caller could retain or log — a canary planted anywhere else in the body (a
// message, a tool result) never reaches the return value. Forwarding the
// request upstream is the caller's job with the same bytes.
func ExtractJSONModel(r io.Reader, maxBytes int64) (string, error) {
	body, err := io.ReadAll(&capReader{r: r, limit: maxBytes + 1})
	if err != nil {
		return "", wrapReadErr(err)
	}
	if int64(len(body)) > maxBytes {
		return "", ErrBodyTooLarge
	}

	dec := json.NewDecoder(bytes.NewReader(body))

	tok, err := dec.Token()
	if err != nil {
		return "", wrapReadErr(err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", ErrNotJSONObject
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", wrapReadErr(err)
		}
		key, _ := keyTok.(string)
		if key != "model" {
			if err := skipJSONValue(dec); err != nil {
				return "", wrapReadErr(err)
			}
			continue
		}
		var model string
		if err := dec.Decode(&model); err != nil {
			return "", wrapReadErr(err)
		}
		// This is deliberate: return the instant "model" is found rather
		// than draining the rest of the object. Everything after it —
		// messages, tool defs, attachments — is exactly the content this
		// function must never touch.
		if len(model) > maxModelFieldBytes {
			model = model[:maxModelFieldBytes]
		}
		return model, nil
	}
	return "", ErrModelFieldMissing
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
		if found || part.FormName() != "model" || part.FileName() != "" {
			// This is deliberate: keep draining to EOF even after "model" is
			// found, rather than returning early. NextPart discards an
			// unread part's remaining bytes itself — the file part's
			// content never reaches a variable here either way — but only
			// draining every part enforces maxBytes over the whole body
			// regardless of which part "model" happens to be, matching the
			// JSON extractor's cap on the request as a whole.
			part.Close()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, maxModelFieldBytes))
		part.Close()
		if err != nil {
			return "", wrapReadErr(err)
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
