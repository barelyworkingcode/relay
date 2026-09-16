package pioverlay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testModelKey = "rmk_" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestMaterializePiOverlay_DisabledWithoutModelKey(t *testing.T) {
	dir, err := MaterializePiOverlay(t.TempDir(), PiOverlayInputs{ModelID: "m", BaseURL: "http://127.0.0.1:1/v1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "" {
		t.Fatalf("expected no overlay dir without a model key, got %q", dir)
	}
}

func TestMaterializePiOverlay_ApiKeyMatchesLaunchSpecKey(t *testing.T) {
	projectDir := t.TempDir()
	overlayDir, err := MaterializePiOverlay(projectDir, PiOverlayInputs{
		ModelID:  "claude-sonnet-4",
		ModelKey: testModelKey,
		BaseURL:  "http://127.0.0.1:5555/v1",
	})
	if err != nil {
		t.Fatalf("MaterializePiOverlay: %v", err)
	}
	if overlayDir != filepath.Join(projectDir, ".pi") {
		t.Errorf("overlayDir = %q", overlayDir)
	}

	modelsPath := filepath.Join(overlayDir, "models.json")
	info, err := os.Stat(modelsPath)
	if err != nil {
		t.Fatalf("stat models.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("models.json mode = %o, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
			API     string `json:"api"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	relay, ok := parsed.Providers[RelayProvider]
	if !ok {
		t.Fatalf("missing %q provider: %s", RelayProvider, data)
	}
	if relay.APIKey != testModelKey {
		t.Errorf("apiKey = %q, want the LaunchSpec key %q", relay.APIKey, testModelKey)
	}
	if relay.BaseURL != "http://127.0.0.1:5555/v1" {
		t.Errorf("baseUrl = %q", relay.BaseURL)
	}
	if relay.API != "openai-completions" {
		t.Errorf("api = %q, want openai-completions (SP9: Authorization: Bearer via the openai-completions path)", relay.API)
	}
	if len(relay.Models) != 1 || relay.Models[0].ID != "claude-sonnet-4" {
		t.Errorf("models = %+v", relay.Models)
	}
}

func TestMaterializePiOverlay_SettingsJSONDefaultsToRelayModel(t *testing.T) {
	projectDir := t.TempDir()
	overlayDir, err := MaterializePiOverlay(projectDir, PiOverlayInputs{
		ModelID:  "claude-sonnet-4",
		ModelKey: testModelKey,
		BaseURL:  "http://127.0.0.1:5555/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(overlayDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["defaultProvider"] != RelayProvider {
		t.Errorf("defaultProvider = %v", settings["defaultProvider"])
	}
	if settings["defaultModel"] != "claude-sonnet-4" {
		t.Errorf("defaultModel = %v", settings["defaultModel"])
	}
}

func TestApplyPiOverlayEnv_SetsCodingAgentDir(t *testing.T) {
	projectDir := t.TempDir()
	env, err := ApplyPiOverlayEnv([]string{"PATH=/usr/bin"}, projectDir, PiOverlayInputs{
		ModelID:  "m",
		ModelKey: testModelKey,
		BaseURL:  "http://127.0.0.1:1/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "PI_CODING_AGENT_DIR=" + filepath.Join(projectDir, ".pi")
	found := false
	for _, e := range env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Errorf("env missing %q; got %v", want, env)
	}
}

func TestMaterializePiOverlay_EmptyOrRootProjectDirNoop(t *testing.T) {
	for _, dir := range []string{"", "/"} {
		got, err := MaterializePiOverlay(dir, PiOverlayInputs{ModelID: "m", ModelKey: testModelKey, BaseURL: "http://x/v1"})
		if err != nil {
			t.Fatalf("dir=%q: unexpected error: %v", dir, err)
		}
		if got != "" {
			t.Fatalf("dir=%q: expected no overlay, got %q", dir, got)
		}
	}
}
