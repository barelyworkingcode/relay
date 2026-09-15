package modelbroker

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

func TestRewriteJSONModel_ReplacesOnlyModelField(t *testing.T) {
	body := []byte(`{"model":"llama/x","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	out, err := RewriteJSONModel(body, "x")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["model"]) != `"x"` {
		t.Fatalf("model = %s, want \"x\"", got["model"])
	}
	var messages []map[string]string
	if err := json.Unmarshal(got["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0]["content"] != "hi" {
		t.Fatalf("messages survived incorrectly: %s", got["messages"])
	}
	if string(got["temperature"]) != "0.5" {
		t.Fatalf("temperature = %s, want 0.5", got["temperature"])
	}
}

// TestRewriteJSONModel_DropsEveryFoldMatchingKey is B1(b)'s defence-in-depth
// proof: even given a body extraction should have already refused (more
// than one key folding to "model"), the rewrite leaves exactly one "model"
// key, with the canonical value, and no surviving case-variant relayLLM's
// own case-insensitive decode could still pick up.
func TestRewriteJSONModel_DropsEveryFoldMatchingKey(t *testing.T) {
	body := []byte(`{"model":"llama/x","Model":"evil","MODEL":"also-evil","messages":[1,2]}`)
	out, err := RewriteJSONModel(body, "x")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rewritten body has %d keys, want 2 (model, messages): %s", len(got), out)
	}
	if string(got["model"]) != `"x"` {
		t.Fatalf("model = %s, want \"x\"", got["model"])
	}
	if _, ok := got["Model"]; ok {
		t.Fatal("a case-variant key survived the rewrite")
	}
	if _, ok := got["MODEL"]; ok {
		t.Fatal("a case-variant key survived the rewrite")
	}
}

func TestRewriteJSONModel_RefusesNonObject(t *testing.T) {
	if _, err := RewriteJSONModel([]byte(`[1,2,3]`), "x"); err == nil {
		t.Fatal("a JSON array rewrote without error")
	}
	if _, err := RewriteJSONModel([]byte(`not json`), "x"); err == nil {
		t.Fatal("malformed JSON rewrote without error")
	}
}

// TestRewriteMultipartModel_DropsEveryFoldMatchingPart is B2's defence-in-
// depth proof, the multipart mirror of
// TestRewriteJSONModel_DropsEveryFoldMatchingKey: even given a body
// extraction should already have refused, the rewrite emits exactly one
// "model" part, canonical, with no surviving case-variant part relayLLM's
// own last-wins multipart parsing could still pick up.
func TestRewriteMultipartModel_DropsEveryFoldMatchingPart(t *testing.T) {
	body, boundary := buildMultipartWithFields(t, [][2]string{
		{"model", "llama/x"}, {"Model", "evil"}, {"language", "en"}, {"MODEL", "also-evil"},
	})
	out, contentType, err := RewriteMultipartModel(bytes.NewReader(body), boundary, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	names := map[string]int{}
	values := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		var b bytes.Buffer
		b.ReadFrom(part)
		names[part.FormName()]++
		values[part.FormName()] = b.String()
	}
	if names["model"] != 1 {
		t.Fatalf("got %d \"model\" parts, want exactly 1: %v", names["model"], names)
	}
	if values["model"] != "x" {
		t.Fatalf("model = %q, want x", values["model"])
	}
	if names["Model"] != 0 || names["MODEL"] != 0 {
		t.Fatalf("a case-variant part survived the rewrite: %v", names)
	}
	if values["language"] != "en" {
		t.Fatalf("language = %q, want en (untouched)", values["language"])
	}
}

func TestRewriteMultipartModel_ReplacesModelKeepsFile(t *testing.T) {
	fileContent := []byte("fake-audio-bytes")
	body, boundary := buildMultipart(t, "llama/x", map[string]string{"language": "en"}, fileContent)

	out, contentType, err := RewriteMultipartModel(bytes.NewReader(body), boundary, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	got := map[string]string{}
	var gotFile []byte
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" || part.FormName() == "language" {
			var sb strings.Builder
			buf := make([]byte, 512)
			for {
				n, rerr := part.Read(buf)
				sb.Write(buf[:n])
				if rerr != nil {
					break
				}
			}
			got[part.FormName()] = sb.String()
		} else if part.FormName() == "file" {
			var b bytes.Buffer
			b.ReadFrom(part)
			gotFile = b.Bytes()
		}
	}
	if got["model"] != "x" {
		t.Fatalf("model = %q, want x", got["model"])
	}
	if got["language"] != "en" {
		t.Fatalf("language = %q, want en (untouched)", got["language"])
	}
	if !bytes.Equal(gotFile, fileContent) {
		t.Fatalf("file content = %q, want %q", gotFile, fileContent)
	}
}

func TestRewriteMultipartModel_NoModelFieldStillCopiesFile(t *testing.T) {
	fileContent := []byte("audio")
	body, boundary := buildMultipart(t, "", map[string]string{"language": "en"}, fileContent)
	out, contentType, err := RewriteMultipartModel(bytes.NewReader(body), boundary, "x")
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(contentType)
	mr := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	sawFile := false
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "file" {
			sawFile = true
		}
	}
	if !sawFile {
		t.Fatal("file part did not survive a body with no model field")
	}
}

// TestRewriteJSONModel_PreservesBigNumbersByteForByte is the UseNumber nit's
// forwarding half (relay#116 re-review): RewriteJSONModel decodes into
// map[string]json.RawMessage, which never parses a field's value at all —
// only "model" itself is ever replaced — so a number literal outside
// float64's safe range must survive rewriting exactly as the caller wrote
// it, not as whatever float64 would round-trip it to.
func TestRewriteJSONModel_PreservesBigNumbersByteForByte(t *testing.T) {
	const thirtyDigitInt = "123456789012345678901234567890"
	const hugeExponent = "1e400"
	body := []byte(`{"model":"llama/x","big_int":` + thirtyDigitInt + `,"big_exp":` + hugeExponent + `}`)

	out, err := RewriteJSONModel(body, "x")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["big_int"]) != thirtyDigitInt {
		t.Fatalf("big_int = %s, want byte-identical %s", got["big_int"], thirtyDigitInt)
	}
	if string(got["big_exp"]) != hugeExponent {
		t.Fatalf("big_exp = %s, want byte-identical %s", got["big_exp"], hugeExponent)
	}
	if string(got["model"]) != `"x"` {
		t.Fatalf("model = %s, want \"x\"", got["model"])
	}
}
