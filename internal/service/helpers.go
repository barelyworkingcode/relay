package service

import (
	"fmt"
	"os/exec"

	"github.com/barelyworkingcode/relay/internal/config"
)

// MergeEnv adds environment variables to a command.
func MergeEnv(cmd *exec.Cmd, env map[string]string) {
	if len(env) == 0 {
		return
	}
	cmd.Env = append(cmd.Environ(), envSlice(env)...)
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// RevealEnv reveals every value of m, refusing if any could not be
// unsealed from the sealed store.
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
