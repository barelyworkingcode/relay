package main

// --stack runs the api phase against one verification stack: a relay
// instance devboxWorld's stack-up started on its own config dir, from its own
// bundle, on its own clone of the world. Everything that names a relay
// instance comes from stack.json; nothing reaches the installed tray, the
// shared world's scripts or a lock, so two stacks can be verified at once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// stackFile is stack.json as relay#479 pins it. credential_file is relay's
// addition: absent, the execute credential is the run directory's
// "credential" beside stack.json.
type stackFile struct {
	Slot           string `json:"slot"`
	ConfigDir      string `json:"config_dir"`
	RelayBin       string `json:"relay_bin"`
	Marker         string `json:"marker"`
	EveURL         string `json:"eve_url"`
	EveDataDir     string `json:"eve_data_dir"`
	CredentialFile string `json:"credential_file"`
	Head           struct {
		Relay string `json:"relay"`
		Eve   string `json:"eve"`
	} `json:"head"`
}

func readStack(path string) (stackFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return stackFile{}, errors.New("stack file is not readable")
	}
	var s stackFile
	if err := json.Unmarshal(raw, &s); err != nil {
		return stackFile{}, errors.New("stack file is not valid JSON")
	}
	for name, p := range map[string]string{"config_dir": s.ConfigDir, "relay_bin": s.RelayBin, "marker": s.Marker} {
		if !filepath.IsAbs(p) {
			return stackFile{}, fmt.Errorf("stack file %s is not an absolute path", name)
		}
	}
	if s.Slot == "" {
		return stackFile{}, errors.New("stack file names no slot")
	}
	if s.CredentialFile == "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return stackFile{}, err
		}
		s.CredentialFile = filepath.Join(filepath.Dir(abs), "credential")
	} else if !filepath.IsAbs(s.CredentialFile) {
		return stackFile{}, errors.New("stack file credential_file is not an absolute path")
	}
	return s, nil
}

// relayArgs puts the stack's config dir in front of a relay command line, so
// a stack run's every relay call names its instance; outside a stack the
// line is unchanged.
func (e env) relayArgs(args []string) []string {
	if !e.Stack {
		return args
	}
	return append([]string{"--config-dir", e.ConfigDir}, args...)
}

func (e env) relayExec(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, e.RelayBin, e.relayArgs(args)...)
}

// relayShell is relay, quoted, for a line a live session runs.
func (e env) relayShell() string {
	if !e.Stack {
		return shellQuote(e.RelayBin)
	}
	return shellQuote(e.RelayBin) + " --config-dir " + shellQuote(e.ConfigDir)
}
