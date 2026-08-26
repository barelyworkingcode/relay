package main

import (
	"context"
	"encoding/json"
	"testing"

	"relaygo/bridge"
)

// Relay must forward the client's hash unchanged, even when it does not match
// the arguments. Recomputing it here would validate relay against itself and
// detect exactly the corruption the hash exists to catch.
func TestArgsSHA256ForwardedVerbatimEvenWhenWrong(t *testing.T) {
	const wrong = "0000000000000000000000000000000000000000000000000000000000000000"
	out := mergeArgsSHA256(json.RawMessage(`{"project_id":"p"}`), wrong)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("meta is not an object: %v", err)
	}
	if m["args_sha256"] != wrong {
		t.Errorf("args_sha256 = %v, want it forwarded verbatim as %q", m["args_sha256"], wrong)
	}
	if m["project_id"] != "p" {
		t.Errorf("project_id was lost: %v", m["project_id"])
	}
}

func TestArgsSHA256AbsentStaysAbsent(t *testing.T) {
	base := json.RawMessage(`{"project_id":"p"}`)
	if got := mergeArgsSHA256(base, ""); string(got) != string(base) {
		t.Errorf("meta = %s, want it unchanged when no hash was sent", got)
	}
}

func TestArgsSHA256RidesTheContext(t *testing.T) {
	ctx := bridge.WithArgsSHA256(context.Background(), "abc")
	if got := bridge.ArgsSHA256FromContext(ctx); got != "abc" {
		t.Errorf("from context = %q, want %q", got, "abc")
	}
	if got := bridge.ArgsSHA256FromContext(context.Background()); got != "" {
		t.Errorf("bare context = %q, want empty", got)
	}
}
