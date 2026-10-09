package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// configDirSource says which rule chose the config dir, so an error about a
// dead socket can name the knob that picked it.
type configDirSource string

const (
	configDirFromFlag    configDirSource = "--config-dir"
	configDirFromEnv     configDirSource = configDirSource(bridge.EnvConfigDir)
	configDirFromDefault configDirSource = "default"
)

const configDirFlag = "--config-dir"

// selectConfigDir picks the config dir for this process and returns args with
// the flag removed. First rule that applies: --config-dir anywhere in args
// before a bare "--", then RELAY_CONFIG_DIR, then the default dir.
//
// This is deliberate: every subcommand owns its own flag.FlagSet, so the flag
// is stripped here, before any of them parses. A literal "--config-dir" meant
// for a registered command's argv is written "--args=--config-dir".
func selectConfigDir(args []string, getenv func(string) string) (rest []string, dir string, source configDirSource, err error) {
	rest = make([]string, 0, len(args))
	var flagValue string
	seen := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var value string
		switch {
		case a == configDirFlag:
			if i+1 >= len(args) {
				return nil, "", "", errors.New(configDirFlag + " needs a directory path")
			}
			i++
			value = args[i]
		case strings.HasPrefix(a, configDirFlag+"="):
			value = a[len(configDirFlag)+1:]
		default:
			rest = append(rest, a)
			continue
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return nil, "", "", errors.New(configDirFlag + " needs a directory path")
		}
		if seen {
			return nil, "", "", errors.New(configDirFlag + " given more than once")
		}
		seen, flagValue = true, value
	}

	if seen {
		abs, err := filepath.Abs(flagValue)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s %q: %w", configDirFlag, flagValue, err)
		}
		return rest, abs, configDirFromFlag, nil
	}
	if env := getenv(bridge.EnvConfigDir); env != "" {
		if !filepath.IsAbs(env) {
			return nil, "", "", fmt.Errorf("%s must be an absolute path, got %q", bridge.EnvConfigDir, env)
		}
		return rest, filepath.Clean(env), configDirFromEnv, nil
	}
	return rest, bridge.DefaultConfigDir(), configDirFromDefault, nil
}
