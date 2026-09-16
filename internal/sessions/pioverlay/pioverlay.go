// Package pioverlay materializes the per-project pi (pi.dev coding agent)
// config overlay: a small .pi-relay/ directory under the session's own
// project carrying models.json and settings.json, pointed at by
// PI_CODING_AGENT_DIR. The name is relay-owned, not pi's own ".pi"
// convention, precisely because the caller removes this directory wholesale
// when the session ends — see defaultOverlayDirName.
//
// C8's sealed rmk_ model key lands here, in models.json's apiKey field — SP9
// confirmed pi sends it as `Authorization: Bearer <key>` to whatever baseUrl
// the same entry names, and never writes it anywhere else on disk. This
// package writes exactly one provider entry, "relay", rather than relayLLM's
// richer multi-provider overlay (global-config merge, per-provider excludes,
// llama/mlx router aliases): that whole mechanism existed to front
// relayLLM's own local model router, which the session-host migration
// retires in favor of C8's single model broker. A session's pi process needs
// exactly one routable model — the one its LaunchSpec names — so one
// provider entry is what the new contract actually calls for, not a reduced
// port of the old one.
package pioverlay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RelayProvider is the provider name this package registers in pi's
// models.json for the relay-brokered model.
const RelayProvider = "relay"

// overlayFileMode is 0600: models.json holds the session's live model key at
// rest for as long as the file exists (SP9's finding) — nothing but this
// process's own uid may read it.
const overlayFileMode = 0o600

// defaultOverlayDirName is deliberately not ".pi": pi's own convention for
// that name is a project's real, user-owned config directory (custom
// settings, agent files), and Kill removes the overlay directory wholesale
// when the session ends. A name only relay ever creates means that removal
// can never take a user's own directory or its pre-existing contents with
// it.
const defaultOverlayDirName = ".pi-relay"

// PiOverlayInputs bundles everything MaterializePiOverlay needs to write one
// session's pi overlay. ModelKey is the LaunchSpec's C8 model key, resolved
// by the caller (this package never mints or looks one up); BaseURL is the
// http(s) endpoint that key authenticates against, resolved by the caller
// from RELAY_MODEL_SOCKET (see provider.startModelProxy — the "how" of that
// conversion is this port's one open judgment call, not this package's).
type PiOverlayInputs struct {
	DirName  string // overlay dir name under projectDir; "" defaults to defaultOverlayDirName
	ModelID  string // the single model id this session is bound to
	ModelKey string // LaunchSpec model key; "" disables the relay provider entry entirely
	BaseURL  string // e.g. "http://127.0.0.1:PORT/v1"; required whenever ModelKey is set

	SupportsImages bool
}

// enabled reports whether there is anything to materialize. A session with
// no model key (a Claude-kind session never gets one; C5's own table scopes
// model_key to pi/chat/pty-with-model_key templates) skips the overlay
// entirely and pi falls back to its own global ~/.pi/agent/ config.
func (in PiOverlayInputs) enabled() bool {
	return in.ModelKey != "" && in.BaseURL != ""
}

// ApplyPiOverlayEnv materializes the overlay and, when one is written, sets
// PI_CODING_AGENT_DIR in env. Returned env replaces the caller's.
func ApplyPiOverlayEnv(env []string, projectDir string, inputs PiOverlayInputs) ([]string, error) {
	overlayDir, err := MaterializePiOverlay(projectDir, inputs)
	if err != nil {
		return env, err
	}
	if overlayDir == "" {
		return env, nil
	}
	return setEnv(env, "PI_CODING_AGENT_DIR", overlayDir), nil
}

// MaterializePiOverlay writes <projectDir>/<dirName>/{models,settings}.json
// reflecting inputs' single relay-brokered model and returns the overlay dir
// path for use as PI_CODING_AGENT_DIR. Returns ("", nil) when there is
// nothing to materialize (no model key) or projectDir is empty/root.
func MaterializePiOverlay(projectDir string, inputs PiOverlayInputs) (string, error) {
	if !inputs.enabled() {
		return "", nil
	}
	if projectDir == "" || projectDir == "/" {
		return "", nil
	}

	dirName := inputs.DirName
	if dirName == "" {
		dirName = defaultOverlayDirName
	}
	overlayDir := filepath.Join(projectDir, dirName)

	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		return "", fmt.Errorf("pi overlay: mkdir %s: %w", overlayDir, err)
	}

	if err := writePiOverlayJSON(filepath.Join(overlayDir, "models.json"), buildPiModelsJSON(inputs)); err != nil {
		return "", err
	}
	if err := writePiOverlayJSON(filepath.Join(overlayDir, "settings.json"), buildPiSettingsJSON(inputs)); err != nil {
		return "", err
	}

	return overlayDir, nil
}

// buildPiModelsJSON emits the {providers: {...}} shape pi's ModelRegistry
// expects: one "relay" provider, one model, apiKey = the session's own
// LaunchSpec model key.
func buildPiModelsJSON(inputs PiOverlayInputs) map[string]any {
	modelInput := []string{"text"}
	if inputs.SupportsImages {
		modelInput = append(modelInput, "image")
	}
	return map[string]any{
		"providers": map[string]any{
			RelayProvider: map[string]any{
				"baseUrl": inputs.BaseURL,
				"api":     "openai-completions",
				"apiKey":  inputs.ModelKey,
				"models": []map[string]any{
					{"id": inputs.ModelID, "input": modelInput},
				},
			},
		},
	}
}

// buildPiSettingsJSON emits a minimal settings.json pointing pi at the
// relay-brokered model by default.
func buildPiSettingsJSON(inputs PiOverlayInputs) map[string]any {
	return map[string]any{
		"defaultProvider": RelayProvider,
		"defaultModel":    inputs.ModelID,
	}
}

// writePiOverlayJSON marshals v with 2-space indent and writes it atomically
// (tmp + rename) at overlayFileMode.
func writePiOverlayJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("pi overlay: marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, overlayFileMode); err != nil {
		return fmt.Errorf("pi overlay: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pi overlay: rename %s: %w", tmp, err)
	}
	return nil
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
