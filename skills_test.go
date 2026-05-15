package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaygo/mcp"
)

// stubLister returns a fixed tool list regardless of token. Used so skill
// tests don't need a live bridge.
type stubLister struct {
	tools []mcp.Tool
	err   error
}

func (s stubLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(s.tools)
}

func TestRenderSkillMd_NoTokenLeakage(t *testing.T) {
	proj := Project{Name: "tbo", Token: "secret-plaintext-token-do-not-leak"}
	tools := []mcp.Tool{
		{Name: "fs_read", Description: "Read a file"},
		{Name: "fs_write", Description: "Write a file"},
	}
	out := renderSkillMd(proj, tools)

	if strings.Contains(out, proj.Token) {
		t.Fatalf("SKILL.md must never contain the plaintext token; got:\n%s", out)
	}
	if !strings.Contains(out, "fs_read") || !strings.Contains(out, "fs_write") {
		t.Fatalf("tool names missing from output:\n%s", out)
	}
	// allowed-tools should scope to the resolved relay binary + "mcp call *".
	// The exact path is dynamic (resolved via os.Executable), so just check
	// the suffix that names the action.
	if !strings.Contains(out, "mcp call *)") {
		t.Fatalf("expected allowed-tools to scope to `mcp call *`; got:\n%s", out)
	}
	if !strings.Contains(out, "allowed-tools: Bash(") {
		t.Fatalf("expected allowed-tools frontmatter line; got:\n%s", out)
	}
	if !strings.Contains(out, "RELAY_TOKEN") {
		t.Fatalf("expected guidance about RELAY_TOKEN env var; got:\n%s", out)
	}
	if !strings.Contains(out, "Relay binary path:") {
		t.Fatalf("expected the resolved relay binary path to be documented; got:\n%s", out)
	}
}

func TestRenderSkillMd_EmptyTools(t *testing.T) {
	proj := Project{Name: "empty", Token: "tok"}
	out := renderSkillMd(proj, nil)
	if !strings.Contains(out, "No tools are currently available") {
		t.Fatalf("expected empty-tools message; got:\n%s", out)
	}
}

func TestEmitSkill_WritesAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "relay")
	proj := Project{Name: "p1", Token: "tok"}
	lister := stubLister{tools: []mcp.Tool{{Name: "tool_a", Description: "first"}}}

	path, err := EmitSkill(context.Background(), lister, proj, dir, RegenAlways)
	if err != nil {
		t.Fatalf("EmitSkill: %v", err)
	}
	if !strings.HasSuffix(path, "SKILL.md") {
		t.Fatalf("expected SKILL.md suffix, got %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written skill: %v", err)
	}
	if !strings.Contains(string(data), "tool_a") {
		t.Fatalf("expected tool_a in file; got:\n%s", data)
	}

	// Second call with the same lister should produce identical output —
	// deterministic ordering means stable on-disk content.
	if _, err := EmitSkill(context.Background(), lister, proj, dir, RegenAlways); err != nil {
		t.Fatalf("EmitSkill (idempotent): %v", err)
	}
	data2, _ := os.ReadFile(path)
	if string(data) != string(data2) {
		t.Fatalf("expected stable output across regen; got drift")
	}
}

func TestEmitSkill_SkipIfExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "relay")
	proj := Project{Name: "p1", Token: "tok"}
	v1 := stubLister{tools: []mcp.Tool{{Name: "old"}}}
	v2 := stubLister{tools: []mcp.Tool{{Name: "new"}}}

	if _, err := EmitSkill(context.Background(), v1, proj, dir, RegenAlways); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := EmitSkill(context.Background(), v2, proj, dir, RegenSkipIfExists); err != nil {
		t.Fatalf("skipIfExists: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if !strings.Contains(string(data), "old") || strings.Contains(string(data), "new") {
		t.Fatalf("skipIfExists must not overwrite; got:\n%s", data)
	}
}

func TestEmitSkill_Never(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "relay")
	proj := Project{Name: "p1", Token: "tok"}
	// RegenNever skips even the ListTools call — pass a lister that would
	// error to prove we don't reach it.
	lister := stubLister{err: errors.New("must not be called")}
	if _, err := EmitSkill(context.Background(), lister, proj, dir, RegenNever); err != nil {
		t.Fatalf("RegenNever should be a no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("RegenNever must not write a file; err=%v", err)
	}
}

func TestRemoveSkill_RefusesNonRelayDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "not-relay")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill(tmp); err == nil {
		t.Fatal("expected refusal for non-relay dir; got nil")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("dir should be untouched after refusal: %v", err)
	}
}

func TestRemoveSkill_RelayDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "relay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill(dir); err != nil {
		t.Fatalf("RemoveSkill: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be gone; err=%v", err)
	}
}
