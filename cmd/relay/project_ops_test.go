package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

// TestProjectOps_RegenSkill_RefusesHostedProject is item 1's regression
// test: a project whose directory lives on a Host must never have relay
// write .claude/skills/relay* on the console at a path that only describes
// the remote machine.
func TestProjectOps_RegenSkill_RefusesHostedProject(t *testing.T) {
	mkSandboxRelayHome(t)
	dir := t.TempDir()
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	consolePath := filepath.Join(dir, "hosted-project-path")
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{
			ID:            "p_hosted",
			Name:          "hosted",
			Path:          consolePath,
			HostID:        "h_devbox",
			GenerateSkill: true,
		})
	}); err != nil {
		t.Fatalf("seed hosted project: %v", err)
	}

	ops := &ProjectOps{Store: store}
	regenDir, found, err := ops.RegenSkill(context.Background(), fixedTokenLister{}, "p_hosted")
	if !found {
		t.Fatalf("expected the hosted project to be found")
	}
	if err == nil {
		t.Fatalf("expected RegenSkill to refuse a hosted project, got dir %q", regenDir)
	}
	if !errors.Is(err, errProjectHosted) {
		t.Fatalf("expected errProjectHosted, got %v", err)
	}
	if regenDir != "" {
		t.Fatalf("expected no dir returned on refusal, got %q", regenDir)
	}

	// Nothing must have been written under the path a hosted project's Path
	// merely describes on the (unreachable) remote machine.
	if _, statErr := os.Stat(filepath.Join(consolePath, ".claude", "skills")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no skills dir written on the console for a hosted project, stat err = %v", statErr)
	}
}

// TestProjectOps_RegenSkill_ConsoleProjectOK is the control case: a plain
// console project still gets its skill dir written.
func TestProjectOps_RegenSkill_ConsoleProjectOK(t *testing.T) {
	mkSandboxRelayHome(t)
	dir := t.TempDir()
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	projPath := t.TempDir()
	ops := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	created, err := ops.Create(context.Background(), project.CreateFields{
		Name:          "console",
		Path:          projPath,
		GenerateSkill: true,
	}, nil, auditViaHTTP, "")
	if err != nil {
		t.Fatalf("seed console project: %v", err)
	}

	regenDir, found, err := ops.RegenSkill(context.Background(), fixedTokenLister{}, created.ID)
	if !found {
		t.Fatalf("expected the console project to be found")
	}
	if err != nil {
		t.Fatalf("RegenSkill: %v", err)
	}
	if regenDir == "" {
		t.Fatalf("expected a skill dir path back")
	}
	if _, statErr := os.Stat(regenDir); statErr != nil {
		t.Fatalf("expected skill dir to exist: %v", statErr)
	}
}

// TestProjectOps_RegenSkill_NotFound covers the not-found branch both doors
// rely on to answer 404 / "project not found".
func TestProjectOps_RegenSkill_NotFound(t *testing.T) {
	mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	ops := &ProjectOps{Store: store}
	_, found, err := ops.RegenSkill(context.Background(), fixedTokenLister{}, "nope")
	if found {
		t.Fatalf("expected found=false for an unknown project")
	}
	if err != nil {
		t.Fatalf("expected no error for an unknown project, got %v", err)
	}
}

// TestProjectOps_Remove_CleansUpSkillDir is item 2's regression test: the
// shared Remove core deletes the project and its skill directory exactly as
// the two doors used to do inline.
func TestProjectOps_Remove_CleansUpSkillDir(t *testing.T) {
	mkSandboxRelayHome(t)
	store := sealedSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	projPath := t.TempDir()
	skillDir := filepath.Join(projPath, ".claude", "skills", "relay-files")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("seed skill dir: %v", err)
	}

	if err := store.With(func(s *config.Settings) {
		s.AddProject(config.Project{
			ID:            "p_remove",
			Name:          "remove-me",
			Path:          projPath,
			GenerateSkill: true,
		})
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	var events atomic.Int64
	ops := &ProjectOps{Store: store, Queue: commitQueueFor(t, store, &events)}
	removed, found, err := ops.Remove("p_remove")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !found {
		t.Fatalf("expected the project to be found")
	}
	if removed.ID != "p_remove" {
		t.Fatalf("expected removed project id p_remove, got %q", removed.ID)
	}
	if events.Load() != 1 {
		t.Fatalf("expected one commit event, got %d", events.Load())
	}
	if proj, _ := config.FindProjectByID(store.Get(), "p_remove"); proj != nil {
		t.Fatalf("expected project to be gone from settings")
	}
	if _, statErr := os.Stat(skillDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the relay-managed skill dir to be removed, stat err = %v", statErr)
	}

	// Removing an unknown id is a no-op, not an error.
	_, found, err = ops.Remove("p_remove")
	if err != nil {
		t.Fatalf("Remove of already-gone project: %v", err)
	}
	if found {
		t.Fatalf("expected found=false removing an already-gone project")
	}
}
