package service

import (
	"os"
	"os/exec"
	"testing"
)

func TestMergeEnv_EmptyMapIsNoop(t *testing.T) {
	cmd := exec.Command("true")
	MergeEnv(cmd, nil)
	if cmd.Env != nil {
		t.Errorf("expected cmd.Env to remain nil, got %v", cmd.Env)
	}

	cmd2 := exec.Command("true")
	MergeEnv(cmd2, map[string]string{})
	if cmd2.Env != nil {
		t.Errorf("expected cmd.Env to remain nil for empty map, got %v", cmd2.Env)
	}
}

func TestMergeEnv_MergesWithOsEnviron(t *testing.T) {
	cmd := exec.Command("true")
	MergeEnv(cmd, map[string]string{"TEST_KEY_RELAY": "test_value"})
	if cmd.Env == nil {
		t.Fatal("expected cmd.Env to be set")
	}

	osEnvLen := len(os.Environ())
	if len(cmd.Env) < osEnvLen+1 {
		t.Errorf("expected at least %d env vars, got %d", osEnvLen+1, len(cmd.Env))
	}

	found := false
	for _, entry := range cmd.Env {
		if entry == "TEST_KEY_RELAY=test_value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("merged env does not contain TEST_KEY_RELAY=test_value")
	}
}
