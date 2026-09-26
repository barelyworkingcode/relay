//go:build darwin

package main

// The model key through the real stack (plans/client-model-routing.md): a real
// relay-sessions host process, a real shim and a real pty child, driven by
// AuthorizeLaunch and the frontend socket the way eve drives them. Relay's
// side is in this process; everything after POST /api/terminals is real.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

const keyHeaderMapping = "X-Relay-Key: ${MODEL_KEY}"

func addKeyEnvTemplates(t *testing.T, f *sessionHostFixture) {
	t.Helper()
	args := []string{
		"-marker", "${PROJECT_PATH}/rs10-marker.json",
		"-env-out", "${PROJECT_PATH}/rs10-env.json",
		"-sleep", "90s", "-exit-code", "0",
	}
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates,
			// A custom template that maps the key into the child's env.
			config.TerminalTemplate{
				ID: "rs10-keymap", Name: "rs10 keymap", Command: f.testTargetBin, Args: args, ModelKey: true, Sandbox: ptr(false),
				Env: map[string]string{
					"ANTHROPIC_BASE_URL":       "http://127.0.0.1:9911",
					"ANTHROPIC_CUSTOM_HEADERS": keyHeaderMapping,
				},
			},
			// Opted in to a key, but declares no mapping: nothing is injected.
			config.TerminalTemplate{ID: "rs10-keynomap", Name: "rs10 keynomap", Command: f.testTargetBin, Args: args, ModelKey: true, Sandbox: ptr(false)},
			// No key at all.
			config.TerminalTemplate{ID: "rs10-nokey", Name: "rs10 nokey", Command: f.testTargetBin, Args: args, Sandbox: ptr(false)},
		)
	}), "seed key templates")
}

func readChildEnv(t *testing.T, projPath string) map[string]string {
	t.Helper()
	var env map[string]string
	assertNoErr(t, json.Unmarshal(rs10WaitForFile(t, filepath.Join(projPath, "rs10-env.json")), &env), "decode child env")
	return env
}

// keyInEnv returns the rmk_ key found anywhere in env, and where.
func keyInEnv(env map[string]string) (key, name string) {
	for k, v := range env {
		if i := strings.Index(v, "rmk_"); i >= 0 {
			return v[i:], k
		}
	}
	return "", ""
}

func TestSessionHost_ModelKeyReachesTheChildOnlyThroughTheTemplateMapping(t *testing.T) {
	f := newSessionHostFixture(t)
	addKeyEnvTemplates(t, f)

	mapped := f.newProject(t, "rs10-key-mapped", nil)
	f.createTerminal(t, mapped.ID, "rs10-keymap")
	env := readChildEnv(t, mapped.Path)
	key, where := keyInEnv(env)
	if key == "" {
		t.Fatalf("a model_key template with a ${MODEL_KEY} mapping got no key in the child's env: %v", env)
	}
	if where != "ANTHROPIC_CUSTOM_HEADERS" || env["ANTHROPIC_CUSTOM_HEADERS"] != "X-Relay-Key: "+key {
		t.Fatalf("the key is in %s = %q, want exactly ANTHROPIC_CUSTOM_HEADERS = %q", where, env[where], "X-Relay-Key: "+key)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9911" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want the template's own value", env["ANTHROPIC_BASE_URL"])
	}
	projectID, label, ok := f.modelKeys.Lookup(key)
	if !ok || projectID != mapped.ID || !strings.HasPrefix(label, "session:") {
		t.Fatalf("the delivered key does not resolve to the launching project's session: %q %q %v", projectID, label, ok)
	}

	nomap := f.newProject(t, "rs10-key-nomap", nil)
	f.createTerminal(t, nomap.ID, "rs10-keynomap")
	env = readChildEnv(t, nomap.Path)
	if k, name := keyInEnv(env); k != "" {
		t.Fatalf("a template with no mapping had a key injected as %s", name)
	}
	for name := range env {
		if strings.Contains(name, "MODEL_KEY") {
			t.Fatalf("a default %s variable was set for a template with no mapping", name)
		}
	}

	nokey := f.newProject(t, "rs10-key-none", nil)
	f.createTerminal(t, nokey.ID, "rs10-nokey")
	if k, name := keyInEnv(readChildEnv(t, nokey.Path)); k != "" {
		t.Fatalf("a template without model_key had a key in %s", name)
	}
}

// The key dies when its session's root process dies, with relay-sessions
// unable to report SessionExited: the host is stopped (SIGSTOP) before the
// root is killed, so the only thing that can end the key is the launch table's
// own root-exit watcher.
func TestSessionHost_ModelKeyDiesWithItsRootWhenNothingReportsSessionExited(t *testing.T) {
	f := newSessionHostFixture(t)
	addKeyEnvTemplates(t, f)
	proj := f.newProject(t, "rs10-key-die", nil)

	sessionID := f.createTerminal(t, proj.ID, "rs10-keymap")
	key, _ := keyInEnv(readChildEnv(t, proj.Path))
	if key == "" {
		t.Fatal("no key delivered")
	}
	if _, _, ok := f.modelKeys.Lookup(key); !ok {
		t.Fatal("the key does not resolve while its session is live")
	}

	id, bound := f.launches.Bound(sessionID)
	if !bound || id.Process.PID <= 0 {
		t.Fatalf("the session's launch is not bound to a real root process: %+v", id)
	}
	hostPID, ok := f.registry.PIDsByServiceID()[config.RelaySessionsServiceID]
	if !ok || hostPID <= 0 {
		t.Fatal("no relay-sessions pid to stop")
	}
	// The whole group, for the same reason SIGKILLHostCleansUpAndRestarts
	// signals -pid: the host is spawned through `sh -l -c`.
	assertNoErr(t, syscall.Kill(-hostPID, syscall.SIGSTOP), "SIGSTOP relay-sessions")
	t.Cleanup(func() { _ = syscall.Kill(-hostPID, syscall.SIGCONT) })

	assertNoErr(t, syscall.Kill(int(id.Process.PID), syscall.SIGKILL), "SIGKILL the session's root (shim)")

	deadline := time.Now().Add(rs10Timeout)
	for time.Now().Before(deadline) {
		if _, _, ok := f.modelKeys.Lookup(key); !ok {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, _, ok := f.modelKeys.Lookup(key); ok {
		t.Fatal("the key still resolves after its root process was killed")
	}
	f.accounting.mu.Lock()
	_, stillTracked := f.accounting.byID[sessionID]
	f.accounting.mu.Unlock()
	if !stillTracked {
		t.Fatal("test premise broken: relay's accounting already dropped the session, so a SessionExited report may have ended the key instead of the launch watcher")
	}
	_ = os.Remove(filepath.Join(proj.Path, "rs10-env.json"))
}
