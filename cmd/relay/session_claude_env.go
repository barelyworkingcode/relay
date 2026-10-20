package main

import (
	"fmt"
	"os"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/childenv"
)

// resolveClaudeEnvPassthrough reads the values of the names in
// claude_env_passthrough from relay's own environment. It returns a nil map and
// nil error when the list is empty. The first entry that fails the name rule
// returns an error naming that entry; a listed name relay's environment does
// not set is skipped. Values never appear in an error.
func resolveClaudeEnvPassthrough(settings *config.Settings) (map[string]string, error) {
	if settings == nil || len(settings.ClaudeEnvPassthrough) == 0 {
		return nil, nil
	}
	var out map[string]string
	for _, name := range settings.ClaudeEnvPassthrough {
		if err := childenv.CheckClaudeEnvName(name); err != nil {
			return nil, fmt.Errorf("claude_env_passthrough: %w", err)
		}
		if v, ok := os.LookupEnv(name); ok {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = v
		}
	}
	return out, nil
}
