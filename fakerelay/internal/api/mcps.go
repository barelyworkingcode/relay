package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/barelyworkingcode/relay/fakerelay/internal/fakes"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func (a *api) mcpRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/mcps", a.listMCPs)
	r.Route(server.ClassRead, "GET /api/mcps/{id}/tools", a.mcpTools)
}

func (a *api) mcps() []world.MCP {
	var out []world.MCP
	a.State.Read(func(m *state.Model) { out = append(out, m.MCPs...) })
	return out
}

func (a *api) listMCPs(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]string{}
	for _, m := range a.mcps() {
		rows = append(rows, map[string]string{"id": m.ID, "display_name": m.Name})
	}
	a.Events.Begin(r.Context(), "mcp.list").Set("count", len(rows)).End("ok", "", nil)
	ok(w, rows)
}

// category is the tool's own, else the part of the name before the first
// underscore with an upper-case first letter.
func category(t map[string]any) string {
	if c, _ := t["category"].(string); c != "" {
		return c
	}
	name, _ := t["name"].(string)
	head, _, found := strings.Cut(name, "_")
	if !found || head == "" {
		return ""
	}
	rs := []rune(head)
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs)
}

func (a *api) mcpTools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "mcp.tools.list").Set("mcp_id", id)
	var found *world.MCP
	for _, m := range a.mcps() {
		if m.ID == id {
			m := m
			found = &m
		}
	}
	if found == nil {
		ev.End("error", "not_found", errors.New("MCP not registered or not connected"))
		server.WriteError(w, http.StatusNotFound, "MCP not registered or not connected")
		return
	}
	var srv fakes.MCPServer = fakes.CatalogueMCP{ID: id, Catalogue: found.Catalogue, Log: a.calls}
	rows, err := srv.Tools(r.Context())
	if err != nil {
		ev.End("error", "internal", err)
		server.WriteError(w, http.StatusServiceUnavailable, "tool list not available")
		return
	}
	out := make([]map[string]string, 0, len(rows))
	for _, raw := range rows {
		var t map[string]any
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		row := map[string]string{"name": str(t["name"])}
		if d := str(t["description"]); d != "" {
			row["description"] = d
		}
		if c := category(t); c != "" {
			row["category"] = c
		}
		out = append(out, row)
	}
	ev.Set("count", len(out)).End("ok", "", nil)
	ok(w, out)
}

func str(v any) string { s, _ := v.(string); return s }
