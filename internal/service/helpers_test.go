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

// A later MergeEnv call — relay's own RELAY_* injections always run after
// the operator's cfg.Env in Registry.Start — must replace an earlier one's
// value for the same key rather than appending a second, ambiguous entry.
func TestMergeEnv_LaterCallOverridesEarlierForSameKey(t *testing.T) {
	cmd := exec.Command("true")
	MergeEnv(cmd, map[string]string{"RELAY_BRIDGE_SOCKET": "operator-supplied"})
	MergeEnv(cmd, map[string]string{"RELAY_BRIDGE_SOCKET": "relays-real-value"})

	var matches []string
	for _, entry := range cmd.Env {
		if len(entry) >= len("RELAY_BRIDGE_SOCKET=") && entry[:len("RELAY_BRIDGE_SOCKET=")] == "RELAY_BRIDGE_SOCKET=" {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one RELAY_BRIDGE_SOCKET entry, got %v", matches)
	}
	if matches[0] != "RELAY_BRIDGE_SOCKET=relays-real-value" {
		t.Errorf("got %q, want the later call's value to win", matches[0])
	}
}

// Two independent keys across two calls must both survive — dedup must be
// per-key, not "keep only the most recent call's env."
func TestMergeEnv_UnrelatedKeysFromEarlierCallsSurvive(t *testing.T) {
	cmd := exec.Command("true")
	MergeEnv(cmd, map[string]string{"FIRST_KEY": "1"})
	MergeEnv(cmd, map[string]string{"SECOND_KEY": "2"})

	var sawFirst, sawSecond bool
	for _, entry := range cmd.Env {
		switch entry {
		case "FIRST_KEY=1":
			sawFirst = true
		case "SECOND_KEY=2":
			sawSecond = true
		}
	}
	if !sawFirst || !sawSecond {
		t.Errorf("expected both keys to survive, env = %v", cmd.Env)
	}
}
