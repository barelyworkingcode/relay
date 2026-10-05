package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestWideCatalogue(t *testing.T) {
	tools := wideToolList()
	if len(tools) != 48 {
		t.Fatalf("tools = %d, want 48", len(tools))
	}
	cats := map[string]int{}
	for _, tl := range tools {
		cat, _ := tl["category"].(string)
		name, _ := tl["name"].(string)
		if !strings.HasPrefix(name, cat+"_") {
			t.Errorf("tool %q does not start with its category %q", name, cat)
		}
		if d, _ := tl["description"].(string); !strings.Contains(d, "Use when the user asks for") {
			t.Errorf("tool %q description lacks the trigger phrase", name)
		}
		cats[cat]++
	}
	if len(cats) != 48 {
		t.Errorf("categories = %d, want 48", len(cats))
	}
	for c, n := range cats {
		if n != 1 {
			t.Errorf("category %q has %d tools, want 1", c, n)
		}
	}
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 130000 {
		t.Errorf("tool defs = %d bytes, want >= 130000", len(b))
	}
	t.Logf("tool defs: %d bytes", len(b))
}

func callText(t *testing.T, name, args string) (string, bool) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	var r struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	if err := json.Unmarshal(wideCall(params), &r); err != nil || len(r.Content) != 1 {
		t.Fatalf("bad result for %s: %v", name, err)
	}
	return r.Content[0].Text, r.IsError
}

func TestWideCalls(t *testing.T) {
	sum := sha256.Sum256([]byte("rotterdam"))
	want := "TIDE-" + hex.EncodeToString(sum[:])[:8]
	if got, isErr := callText(t, "tides_lookup", `{"port":"  Rotterdam "}`); got != want || isErr {
		t.Errorf("tides_lookup = %q (err %v), want %q", got, isErr, want)
	}
	if got, isErr := callText(t, "tides_lookup", `{}`); !isErr || got != "port is required" {
		t.Errorf("tides_lookup without port = %q (err %v)", got, isErr)
	}
	for _, tl := range wideToolList() {
		name := tl["name"].(string)
		if name == "tides_lookup" {
			continue
		}
		if got, isErr := callText(t, name, `{"query":"x"}`); got != "ok "+name || isErr {
			t.Errorf("%s = %q (err %v), want %q", name, got, isErr, "ok "+name)
		}
	}
}
