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

func TestRewriteJSONModel_RefusesNonObject(t *testing.T) {
	if _, err := RewriteJSONModel([]byte(`[1,2,3]`), "x"); err == nil {
		t.Fatal("a JSON array rewrote without error")
	}
	if _, err := RewriteJSONModel([]byte(`not json`), "x"); err == nil {
		t.Fatal("malformed JSON rewrote without error")
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
