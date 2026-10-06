package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	codexModelsCacheTTL = 5 * time.Minute
	codexModelsTimeout  = 8 * time.Second
	codexModelGroup     = "Codex"
	codexModelPrefix    = "codex/"
)

type codexModelsCache struct {
	mu        sync.Mutex
	models    []sessionstypes.ModelInfo
	expiresAt time.Time
	binPath   string
}

var codexModels codexModelsCache

// FetchCodexModels lists the models Codex's own `debug models` reports as
// selectable, as `codex/<slug>` entries. The exec is cached for 5 minutes.
// Returns nil when Codex is absent or the call fails, so a caller drops the
// Codex group silently.
func FetchCodexModels(ctx context.Context, binaryPath string) []sessionstypes.ModelInfo {
	path := resolveCodexPath(binaryPath)

	codexModels.mu.Lock()
	if !codexModels.expiresAt.IsZero() && time.Now().Before(codexModels.expiresAt) && codexModels.binPath == path {
		cached := codexModels.models
		codexModels.mu.Unlock()
		return cached
	}
	codexModels.mu.Unlock()

	if _, err := os.Stat(path); err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, codexModelsTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "debug", "models")
	cmd.Env = childBaseEnv()
	var out bytes.Buffer
	cmd.Stdout = &out // stdout only: stderr noise must not reach the JSON parse
	if err := cmd.Run(); err != nil {
		slog.Warn("codex debug models failed", "error", err)
		return nil
	}

	models, err := parseCodexModels(out.Bytes())
	if err != nil {
		slog.Warn("codex debug models: unparseable output", "error", err)
		return nil
	}

	codexModels.mu.Lock()
	codexModels.models = models
	codexModels.expiresAt = time.Now().Add(codexModelsCacheTTL)
	codexModels.binPath = path
	codexModels.mu.Unlock()
	return models
}

func parseCodexModels(raw []byte) ([]sessionstypes.ModelInfo, error) {
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	models := make([]sessionstypes.ModelInfo, 0, len(doc.Models))
	seen := make(map[string]bool)
	for _, m := range doc.Models {
		if m.Visibility != "list" || m.Slug == "" || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		label := m.DisplayName
		if label == "" {
			label = m.Slug
		}
		models = append(models, sessionstypes.ModelInfo{
			Label:    label,
			Value:    codexModelPrefix + m.Slug,
			Group:    codexModelGroup,
			Provider: "codex",
		})
	}
	return models, nil
}
