package sessions

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

var fixedModels = []world.Model{
	{Value: "haiku", Label: "Claude Haiku", Group: "Claude", Provider: "claude"},
	{Value: "sonnet", Label: "Claude Sonnet", Group: "Claude", Provider: "claude"},
	{Value: "opus", Label: "Claude Opus", Group: "Claude", Provider: "claude"},
}

// modelByValue finds the first row with that value: the fixed Claude rows,
// then the world's.
func (s *svc) modelByValue(v string) (world.Model, bool) {
	for _, m := range fixedModels {
		if m.Value == v {
			return m, true
		}
	}
	var out world.Model
	var found bool
	s.State.Read(func(m *state.Model) {
		for _, x := range m.Models {
			if x.Value == v && !found {
				out, found = x, true
			}
		}
	})
	return out, found
}

// kindOf: a known model's provider decides when it names a session kind,
// else the model id's prefix.
func (s *svc) kindOf(model string) string {
	provider := ""
	if m, found := s.modelByValue(model); found {
		provider = m.Provider
	}
	switch {
	case provider == "claude" || provider == "pi" || provider == "codex":
		return provider
	case provider == "" && strings.HasPrefix(model, "pi/"):
		return "pi"
	case provider == "" && strings.HasPrefix(model, "codex/"):
		return "codex"
	}
	return "chat"
}

func flags(provider string) (permissions, attachments bool) {
	switch provider {
	case "claude":
		return true, true
	case "chat", "openai", "ollama", "pi":
		return false, true
	}
	return false, false
}

func (s *svc) listModels(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	add := func(label, value, group, provider string) {
		p, a := flags(provider)
		rows = append(rows, map[string]any{"label": label, "value": value, "group": group, "provider": provider,
			"supportsPermissions": p, "supportsAttachments": a})
	}
	for _, m := range fixedModels {
		add(m.Label, m.Value, m.Group, m.Provider)
	}
	if raws, err := s.hosts.Models(r.Context()); err == nil {
		for _, raw := range raws {
			var m world.Model
			if json.Unmarshal(raw, &m) == nil {
				add(m.Label, m.Value, m.Group, m.Provider)
			}
		}
	}
	s.Events.Begin(r.Context(), "model.list", events.Sessions()).Set("count", len(rows)).End("ok", "", nil)
	ok(w, map[string]any{"models": rows, "providerSettings": map[string]any{
		"claude": []any{}, "codex": []any{},
		"pi": []any{map[string]any{"key": "thinkingLevel", "label": "Thinking Level", "type": "select", "default": "medium",
			"options": []string{"off", "minimal", "low", "medium", "high", "xhigh"}, "hint": "How hard the model thinks."}},
		"chat": []any{map[string]any{"key": "useRelayTools", "label": "Use Relay Tools", "type": "boolean", "default": false,
			"hint": "Let the model call the tools relay grants."}},
	}})
}
