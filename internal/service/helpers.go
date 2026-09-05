package service

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
)

// MergeEnv adds environment variables to a command, replacing rather than
// duplicating any existing entry for the same key. Registry.Start calls this
// several times per spawn — once for the operator's cfg.Env, then again for
// relay's own RELAY_* injections (service token, frontend creds, bridge
// socket) — and a later call must always win: a duplicate key in a child's
// envp is otherwise implementation-defined (which one getenv returns depends
// on the runtime reading it), which is not a fact relay's own credentials
// should depend on.
func MergeEnv(cmd *exec.Cmd, env map[string]string) {
	if len(env) == 0 {
		return
	}
	base := cmd.Environ()
	kept := make([]string, 0, len(base))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, overridden := env[key]; overridden {
			continue
		}
		kept = append(kept, kv)
	}
	cmd.Env = append(kept, envSlice(env)...)
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// RevealEnv reveals every value of m, refusing if any could not be
// opened. A spawned child must never receive fewer or different variables
// than its record configures, so a value the sealed store cannot currently
// open is refused outright rather than passed through empty or silently
// dropped — the same "everything that needs a sealed value refuses"
// principle §5.6 clause 4 names for ResolvePtyEnv and the admin ops,
// extended to every other consumer of a sealed env value: an external MCP
// child (internal/mcpbroker) and a TCC permission probe (cmd/relay) spawn
// through this too, not only a managed service.
func RevealEnv(m map[string]config.Secret) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		pt, ok := v.Reveal()
		if !ok {
			return nil, fmt.Errorf("env value %q could not be opened: the sealed store is unavailable", k)
		}
		out[k] = pt
	}
	return out, nil
}
