package modelbroker

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"strings"
	"testing"
)

const canary = "CANARY-2f19b3c4-e001-4e9a-9c2a-planted-in-a-field-the-model-never-names"

func TestExtractJSONModel_Basic(t *testing.T) {
	body := `{"model":"foo","messages":[{"role":"user","content":"hi"}]}`
	model, err := ExtractJSONModel(strings.NewReader(body), JSONBodyCap)
	if err != nil {
		t.Fatalf("ExtractJSONModel: %v", err)
	}
	if model != "foo" {
		t.Fatalf("got %q, want foo", model)
	}
}

func TestExtractJSONModel_ModelFieldOrderDoesNotMatter(t *testing.T) {
	// model after a large nested structure the extractor must skip, not parse.
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%q,"nested":{"a":[1,2,[3,4,{"b":5}]]}}],"model":"foo","stream":true}`, canary)
	model, err := ExtractJSONModel(strings.NewReader(body), JSONBodyCap)
	if err != nil {
		t.Fatalf("ExtractJSONModel: %v", err)
	}
	if model != "foo" {
		t.Fatalf("got %q, want foo", model)
	}
}

// TestExtractJSONModel_CanaryNeverRetained plants a canary string in every
// other field of the body (a message, a tool definition, a user id) and
// proves it never reaches the returned value nor any log line — the only
// thing this function is allowed to hand back or record is "model" itself.
func TestExtractJSONModel_CanaryNeverRetained(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	body := fmt.Sprintf(`{
		"user": %q,
		"messages": [{"role":"user","content":%q}],
		"tools": [{"name":%q,"description":%q}],
		"model": "foo",
		"metadata": {"trace": %q}
	}`, canary, canary, canary, canary, canary)

	model, err := ExtractJSONModel(strings.NewReader(body), JSONBodyCap)
	if err != nil {
		t.Fatalf("ExtractJSONModel: %v", err)
	}
	if model != "foo" {
		t.Fatalf("got %q, want foo", model)
	}
	if strings.Contains(model, canary) {
		t.Fatalf("canary leaked into the returned model value")
	}
	if strings.Contains(logBuf.String(), canary) {
		t.Fatalf("canary leaked into logs: %s", logBuf.String())
	}
}

func TestExtractJSONModel_MissingField(t *testing.T) {
	_, err := ExtractJSONModel(strings.NewReader(`{"messages":[]}`), JSONBodyCap)
	if !errors.Is(err, ErrModelFieldMissing) {
		t.Fatalf("got %v, want ErrModelFieldMissing", err)
	}
}

func TestExtractJSONModel_NotAnObject(t *testing.T) {
	_, err := ExtractJSONModel(strings.NewReader(`["not","an","object"]`), JSONBodyCap)
	if !errors.Is(err, ErrNotJSONObject) {
		t.Fatalf("got %v, want ErrNotJSONObject", err)
	}
}

func TestExtractJSONModel_OversizeBodyRefused(t *testing.T) {
	// A body that just fits succeeds; one byte more is refused. Uses a tiny
	// cap so the test allocates nothing close to the real 64 MiB ceiling.
	fits := `{"model":"ab"}`
	if _, err := ExtractJSONModel(strings.NewReader(fits), int64(len(fits))); err != nil {
		t.Fatalf("body exactly at cap: %v", err)
	}
	oversize := fits + " "
	_, err := ExtractJSONModel(strings.NewReader(oversize), int64(len(fits)))
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v, want ErrBodyTooLarge", err)
	}
}

func TestExtractJSONModel_LongModelValueTruncated(t *testing.T) {
	long := strings.Repeat("x", maxModelFieldBytes*2)
	body := fmt.Sprintf(`{"model":%q}`, long)
	model, err := ExtractJSONModel(strings.NewReader(body), JSONBodyCap)
	if err != nil {
		t.Fatalf("ExtractJSONModel: %v", err)
	}
	if len(model) != maxModelFieldBytes {
		t.Fatalf("got length %d, want %d", len(model), maxModelFieldBytes)
	}
}

// buildMultipart writes a multipart/form-data body with a "model" field, an
// arbitrary set of other fields, and one "file" part carrying arbitrary
// bytes (standing in for an audio upload).
func buildMultipart(t *testing.T, model string, other map[string]string, fileContent []byte) (body []byte, boundary string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if model != "" {
		if err := w.WriteField("model", model); err != nil {
			t.Fatalf("WriteField model: %v", err)
		}
	}
	for k, v := range other {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("WriteField %s: %v", k, err)
		}
	}
	if fileContent != nil {
		fw, err := w.CreateFormFile("file", "audio.wav")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := fw.Write(fileContent); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.Boundary()
}

func TestExtractMultipartModel_Basic(t *testing.T) {
	body, boundary := buildMultipart(t, "whisper-1", map[string]string{"language": "en"}, []byte("not-real-audio-bytes"))
	model, err := ExtractMultipartModel(bytes.NewReader(body), boundary, AudioMultipartCap)
	if err != nil {
		t.Fatalf("ExtractMultipartModel: %v", err)
	}
	if model != "whisper-1" {
		t.Fatalf("got %q, want whisper-1", model)
	}
}

// TestExtractMultipartModel_CanaryInFileNeverRead proves the audio part's
// bytes are never copied into any value this function can return — a canary
// planted in the file part, even one many times larger than the model
// field, never surfaces.
func TestExtractMultipartModel_CanaryInFileNeverRead(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	fileContent := []byte(strings.Repeat("pad", 1000) + canary + strings.Repeat("pad", 1000))
	body, boundary := buildMultipart(t, "whisper-1", nil, fileContent)

	model, err := ExtractMultipartModel(bytes.NewReader(body), boundary, AudioMultipartCap)
	if err != nil {
		t.Fatalf("ExtractMultipartModel: %v", err)
	}
	if model != "whisper-1" {
		t.Fatalf("got %q, want whisper-1", model)
	}
	if strings.Contains(logBuf.String(), canary) {
		t.Fatalf("canary leaked into logs: %s", logBuf.String())
	}
}

func TestExtractMultipartModel_MissingField(t *testing.T) {
	body, boundary := buildMultipart(t, "", map[string]string{"language": "en"}, []byte("audio"))
	_, err := ExtractMultipartModel(bytes.NewReader(body), boundary, AudioMultipartCap)
	if !errors.Is(err, ErrModelFieldMissing) {
		t.Fatalf("got %v, want ErrModelFieldMissing", err)
	}
}

func TestExtractMultipartModel_OversizeRefused(t *testing.T) {
	body, boundary := buildMultipart(t, "whisper-1", nil, bytes.Repeat([]byte("a"), 4096))
	_, err := ExtractMultipartModel(bytes.NewReader(body), boundary, 128)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v, want ErrBodyTooLarge", err)
	}
}
