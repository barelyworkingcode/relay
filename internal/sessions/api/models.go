package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// staticClaudeModels mirrors relayLLM's own hardcoded Claude alias list
// (its api.go's RegisterModelRoutes) -- Claude's aliases aren't
// discoverable from anywhere relay-sessions can reach, so they stay a
// literal here exactly as they did before the move.
var staticClaudeModels = []sessionstypes.ModelInfo{
	{Label: "Claude Haiku", Value: "haiku", Group: "Claude", Provider: "claude"},
	{Label: "Claude Sonnet", Value: "sonnet", Group: "Claude", Provider: "claude"},
	{Label: "Claude Opus", Value: "opus", Group: "Claude", Provider: "claude"},
}

// ModelsConfig is what HandleModels needs to reach pi's own model list and
// relay's model broker.
type ModelsConfig struct {
	PiBinary    string
	ModelSocket string

	// dial overrides how the broker catalog fetch reaches ModelSocket.
	// Test-only seam; nil dials ModelSocket over a real Unix socket, the
	// same dialer-seam shape provider.ChatConfig uses.
	dial func(ctx context.Context) (net.Conn, error)
}

func (c ModelsConfig) dialer() func(ctx context.Context) (net.Conn, error) {
	if c.dial != nil {
		return c.dial
	}
	d := net.Dialer{}
	return func(ctx context.Context) (net.Conn, error) {
		return d.DialContext(ctx, "unix", c.ModelSocket)
	}
}

// fetchBrokerModels lists relay's model broker catalog over ModelSocket,
// tokenless: relay-sessions' own peer identity on that socket carries the
// `sessions` capability, which C1 grants the unfiltered catalog with no
// bearer needed (model_endpoint.go's resolveIdentity, ServiceCapabilitySessions
// branch). Returns nil on any failure -- a model listing degrades by
// omission, the same convention FetchPiModels already uses for a missing pi
// binary.
//
// taken names any Value already claimed by an earlier source (the static
// Claude aliases) -- a broker row sharing one of those ids is dropped
// rather than merged in under a colliding Value, since eve resolves a
// model by first Value match and an unselectable second entry would just
// silently launch as the wrong kind.
func fetchBrokerModels(ctx context.Context, cfg ModelsConfig, taken map[string]bool) []sessionstypes.ModelInfo {
	if cfg.ModelSocket == "" && cfg.dial == nil {
		return nil
	}
	dial := cfg.dialer()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://model.sock/v1/models", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var parsed struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil {
		return nil
	}

	models := make([]sessionstypes.ModelInfo, 0, len(parsed.Data))
	for _, row := range parsed.Data {
		if taken[row.ID] {
			continue
		}
		models = append(models, sessionstypes.ModelInfo{
			Label:    row.ID,
			Value:    row.ID,
			Group:    "Model Broker",
			Provider: "chat",
		})
	}
	return models
}

// HandleModels writes the merged model catalog: relay-sessions' fixed
// Claude aliases, pi's own currently-configured models, and relay's model
// broker catalog. This is relayLLM's original /api/models (its api.go's
// RegisterModelRoutes) minus the sources SP10/MB-3 retired (native Ollama,
// per-endpoint OpenAI config, managed llama/mlx servers) -- every model
// that isn't Claude or pi now reaches this list through the broker instead
// of a source this package dials itself.
func HandleModels(cfg ModelsConfig, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	models := append([]sessionstypes.ModelInfo{}, staticClaudeModels...)
	models = append(models, provider.FetchPiModels(r.Context(), cfg.PiBinary)...)

	taken := make(map[string]bool, len(models))
	for _, m := range models {
		taken[m.Value] = true
	}
	models = append(models, fetchBrokerModels(r.Context(), cfg, taken)...)

	for i := range models {
		caps := sessionstypes.CapabilitiesForProvider(models[i].Provider)
		models[i].SupportsPermissions = models[i].SupportsPermissions || caps.SupportsPermissions
		models[i].SupportsAttachments = models[i].SupportsAttachments || caps.SupportsAttachments
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models":           models,
		"providerSettings": provider.ProviderSettings(),
	})
}
