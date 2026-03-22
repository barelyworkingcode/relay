package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// resolveID
// ---------------------------------------------------------------------------

func TestResolveID_ReturnsIDWhenGiven(t *testing.T) {
	got := resolveID("my-id", "My Name")
	if got != "my-id" {
		t.Errorf("resolveID(\"my-id\", \"My Name\") = %q, want %q", got, "my-id")
	}
}

func TestResolveID_SlugifiesNameWhenIDEmpty(t *testing.T) {
	got := resolveID("", "My App")
	if got != "my-app" {
		t.Errorf("resolveID(\"\", \"My App\") = %q, want %q", got, "my-app")
	}
}

func TestResolveID_BothEmptyReturnsEmpty(t *testing.T) {
	got := resolveID("", "")
	if got != "" {
		t.Errorf("resolveID(\"\", \"\") = %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// parseEnvPairs
// ---------------------------------------------------------------------------

func TestParseEnvPairs_NilInput(t *testing.T) {
	env, err := parseEnvPairs(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Errorf("expected nil map, got %v", env)
	}
}

func TestParseEnvPairs_EmptySlice(t *testing.T) {
	env, err := parseEnvPairs([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil {
		t.Errorf("expected nil map, got %v", env)
	}
}

func TestParseEnvPairs_ValidPairs(t *testing.T) {
	env, err := parseEnvPairs([]string{"FOO=bar", "BAZ=qux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(env))
	}
	if env["FOO"] != "bar" {
		t.Errorf("env[\"FOO\"] = %q, want %q", env["FOO"], "bar")
	}
	if env["BAZ"] != "qux" {
		t.Errorf("env[\"BAZ\"] = %q, want %q", env["BAZ"], "qux")
	}
}

func TestParseEnvPairs_ValueContainsEquals(t *testing.T) {
	env, err := parseEnvPairs([]string{"KEY=val=ue"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["KEY"] != "val=ue" {
		t.Errorf("env[\"KEY\"] = %q, want %q", env["KEY"], "val=ue")
	}
}

func TestParseEnvPairs_InvalidPairNoEquals(t *testing.T) {
	_, err := parseEnvPairs([]string{"NOPE"})
	if err == nil {
		t.Fatal("expected error for pair without =, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --env format") {
		t.Errorf("error message %q does not mention invalid format", err.Error())
	}
}
