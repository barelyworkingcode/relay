package fakes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// CatalogueMCP serves the tools of a world MCP's catalogue: {"tools":[{name,
// description?, category?, inputSchema}]}. Each row is returned as stored.
type CatalogueMCP struct {
	ID        string
	Catalogue json.RawMessage
	Log       *CallLog
}

func (m CatalogueMCP) Tools(ctx context.Context) ([]json.RawMessage, error) {
	m.Log.Record("mcp-"+m.ID, "tools/list", nil)
	if len(m.Catalogue) == 0 {
		return []json.RawMessage{}, nil
	}
	var c struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(m.Catalogue, &c); err != nil {
		return nil, fmt.Errorf("mcp %s: catalogue: %w", m.ID, err)
	}
	if c.Tools == nil {
		c.Tools = []json.RawMessage{}
	}
	return c.Tools, nil
}

// WorldModelHost lists the models the world declares, as /api/models rows
// without the derived flags.
type WorldModelHost struct {
	List []world.Model
	Log  *CallLog
}

func (h WorldModelHost) Models(ctx context.Context) ([]json.RawMessage, error) {
	h.Log.Record("modelhost", "models", nil)
	out := make([]json.RawMessage, 0, len(h.List))
	for _, m := range h.List {
		b, err := json.Marshal(map[string]string{"value": m.Value, "label": m.Label, "group": m.Group, "provider": m.Provider})
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
