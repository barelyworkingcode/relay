package main

import (
	"strings"
	"testing"
)

// resolveID and its --id register-time override are gone (ADR-017
// implementation spec S6): McpOps.Add and ServiceOps.Create both derive a
// record's id from --name (slugify) unconditionally, the same as every
// other door always has, so a CLI-only override that brokering could never
// honour was removed along with the direct-mutation path it served.

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
