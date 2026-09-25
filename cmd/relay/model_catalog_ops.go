package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/barelyworkingcode/relay/internal/modelbroker"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// ModelCatalogOps is the read-only, ungated core behind the Settings model
// picker (docs/model-endpoint.md, "Choosing a project's models, in
// Settings"). It lists what relay-sessions serves at GET /api/models and
// adds the endpoint and alias-target detail relay's own broker cache holds.
type ModelCatalogOps struct {
	HostModels func(ctx context.Context) ([]sessiontypes.ModelInfo, error)
	BrokerRows func(ctx context.Context) ([]modelbroker.Row, error)
}

const (
	modelCatalogErrHostUnavailable    = "session host unavailable"
	modelCatalogWarnBrokerUnavailable = "model broker unavailable"

	modelKindChat  = "chat"
	modelKindOther = "other"

	modelBrokerGroupPrefix = "Model broker · "
)

// ModelCatalogView is the list_models IPC payload.
type ModelCatalogView struct {
	Status   string           `json:"status"`
	Error    string           `json:"error,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
	Models   []ModelEntryView `json:"models"`
}

// ModelEntryView is one selectable row. Target is set for an alias only.
type ModelEntryView struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
}

func unavailableModelCatalogView() ModelCatalogView {
	return ModelCatalogView{
		Status: "unavailable",
		Error:  modelCatalogErrHostUnavailable,
		Models: []ModelEntryView{},
	}
}

// List reads relay-sessions first and the broker cache second. The order is
// deliberate: relay-sessions' own /api/models fetch refreshes the shared
// broker cache, so the second read matches what the host just listed.
func (o *ModelCatalogOps) List(ctx context.Context) ModelCatalogView {
	if o == nil || o.HostModels == nil {
		slog.Warn("model catalog: session host unavailable", "error", "no session host client")
		return unavailableModelCatalogView()
	}
	host, err := o.HostModels(ctx)
	if err != nil {
		slog.Warn("model catalog: session host unavailable", "error", err)
		return unavailableModelCatalogView()
	}

	var broker []modelbroker.Row
	var warnings []string
	if o.BrokerRows == nil {
		slog.Warn("model catalog: model broker unavailable", "error", "no broker cache")
		warnings = append(warnings, modelCatalogWarnBrokerUnavailable)
	} else if broker, err = o.BrokerRows(ctx); err != nil {
		slog.Warn("model catalog: model broker unavailable", "error", err)
		broker = nil
		warnings = append(warnings, modelCatalogWarnBrokerUnavailable)
	}

	view := buildModelCatalogView(host, broker)
	view.Warnings = warnings
	return view
}

// buildModelCatalogView emits every host row exactly once, in host order.
// Only chat-provider rows are matched to broker rows; everything else keeps
// the host's own group and label.
func buildModelCatalogView(host []sessiontypes.ModelInfo, broker []modelbroker.Row) ModelCatalogView {
	byID := make(map[string]modelbroker.Row, len(broker))
	for _, r := range broker {
		if _, seen := byID[r.ID]; !seen {
			byID[r.ID] = r
		}
	}

	models := make([]ModelEntryView, 0, len(host))
	for _, m := range host {
		entry := ModelEntryView{
			ID:       m.Value,
			Label:    m.Label,
			Group:    m.Group,
			Provider: m.Provider,
			Kind:     modelKind(m.Value),
		}
		if row, ok := byID[m.Value]; ok && m.Provider == "chat" {
			applyBrokerRow(&entry, row)
		}
		models = append(models, entry)
	}
	return ModelCatalogView{Status: "ok", Models: models}
}

func applyBrokerRow(entry *ModelEntryView, row modelbroker.Row) {
	switch row.OwnedBy {
	case "anthropic-map":
		entry.Group = modelBrokerGroupPrefix + "aliases"
		entry.Target = row.Target
		entry.Label = entry.ID + " → " + row.Target
		if modelKind(row.Target) == modelKindOther {
			entry.Kind = modelKindOther
		}
	case "virtual":
		entry.Group = modelBrokerGroupPrefix + "virtual"
	default:
		entry.Group = modelBrokerGroupPrefix + row.OwnedBy
	}
}

// nonChatModelTokens is a heuristic; a wrong guess only moves a row between
// picker groups. "audio" is deliberately absent: chat models carry it in
// their names too, and those would be hidden under Other.
var nonChatModelTokens = map[string]bool{
	"tts": true, "asr": true, "stt": true, "speech": true, "whisper": true,
	"parakeet": true, "kokoro": true, "orpheus": true, "dia": true,
	"codec": true, "snac": true, "vocoder": true,
	"embed": true, "embedding": true, "embeddings": true,
	"rerank": true, "reranker": true,
}

// modelKind classifies an id as "chat" or "other" by whole tokens of its
// last path segment.
func modelKind(id string) string {
	name := strings.ToLower(id[strings.LastIndex(id, "/")+1:])
	tokens := strings.FieldsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for _, t := range tokens {
		if nonChatModelTokens[t] {
			return modelKindOther
		}
	}
	return modelKindChat
}
