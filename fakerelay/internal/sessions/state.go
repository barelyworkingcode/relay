package sessions

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
)

// serveState answers GET /v1/state: the live model, the hub's sessions and
// terminals, and every host agent's status.
func (s *svc) serveState(w http.ResponseWriter, r *http.Request) {
	var model map[string]any
	s.State.Read(func(m *state.Model) {
		b, _ := json.Marshal(map[string]any{"projects": m.Projects, "hosts": m.Hosts, "mcps": m.MCPs, "models": m.Models,
			"templates": m.Templates, "default_project": m.DefaultProject, "chief_of_staff": m.ChiefOfStaff, "eve": m.Eve})
		_ = json.Unmarshal(b, &model)
	})
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sessions := []map[string]any{}
	for _, id := range ids {
		x := s.sessions[id]
		row := s.row(x)
		row["state"], row["processing"] = x.state, x.processing
		sessions = append(sessions, row)
	}
	terms := []map[string]any{}
	for _, t := range s.terms {
		terms = append(terms, t.row())
	}
	conns := len(s.conns)
	s.mu.Unlock()
	sort.Slice(terms, func(i, j int) bool { return terms[i]["id"].(string) < terms[j]["id"].(string) })
	model["sessions"], model["terminals"], model["ws_connections"] = sessions, terms, conns
	model["host_status"] = s.Hosts.HostStatuses()
	ok(w, model)
}
