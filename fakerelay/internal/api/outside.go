package api

import (
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
	"github.com/gorilla/websocket"
	"net/http"
	"time"
)

func outsideRow(s world.OutsideFixture) map[string]any {
	return map[string]any{"id": s.ID, "name": s.Name, "projectId": s.ProjectID, "hostId": s.HostID, "machine": s.Machine, "directory": s.Directory, "branch": s.Branch, "state": s.State, "since": s.Since, "updatedAt": s.UpdatedAt, "source": s.Source, "outside": true, "readOnly": true, "tmuxSession": s.TmuxSession, "viewable": s.Viewable, "viewReason": s.ViewReason}
}
func (a *api) outsideSnapshot() map[string]any {
	rows := []map[string]any{}
	machines := []map[string]any{}
	seen := map[string]bool{}
	a.State.Read(func(m *state.Model) {
		for _, s := range m.OutsideSessions {
			rows = append(rows, outsideRow(s))
			key := s.Machine
			if !seen[key] {
				seen[key] = true
				status := s.MachineStatus
				if status == "" {
					status = "reachable"
				}
				machines = append(machines, map[string]any{"hostId": s.HostID, "machine": s.Machine, "status": status, "lastProbeAt": rfc3339(a.now()), "error": ""})
			}
		}
	})
	return map[string]any{"type": "outside_sessions", "revision": a.outsideRevision, "probeIntervalMs": 15000, "sessions": rows, "machines": machines}
}
func (a *api) outsideRoutes(r server.Registrar) {
	r.Route(server.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions", func(w http.ResponseWriter, r *http.Request) {
		a.outsideMu.Lock()
		defer a.outsideMu.Unlock()
		ok(w, a.outsideSnapshot())
	})
	r.Route(server.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var found *world.OutsideFixture
		a.State.Read(func(m *state.Model) {
			for _, s := range m.OutsideSessions {
				if s.ID == r.PathValue("id") {
					copy := s
					found = &copy
					break
				}
			}
		})
		if found == nil {
			cosError(w, 404, "outside_session_not_found", "outside session not found")
			return
		}
		if found.MachineStatus == "unreachable" {
			cosError(w, 502, "outside_status_unavailable", "outside status unavailable")
			return
		}
		turn := found.LatestTurn
		text := []rune(turn.Text)
		if len(text) > 8192 {
			text = text[len(text)-8192:]
			turn.Text = string(text)
		}
		excerpt := text
		if len(excerpt) > 500 {
			excerpt = excerpt[len(excerpt)-500:]
		}
		ok(w, map[string]any{"session": outsideRow(*found), "lastTurn": turn, "lastExcerpt": string(excerpt)})
	})
	r.Route(server.ClassChiefOfStaff, "GET /ws/outside-sessions", a.outsideWS)
	r.Control("PUT /v1/outside-sessions", func(w http.ResponseWriter, r *http.Request) {
		var rows []world.OutsideFixture
		if err := decode(w, r, 1<<20, &rows, true); err != nil {
			invalidJSON(w, err)
			return
		}
		if err := world.ValidateOutside(rows); err != nil {
			server.WriteError(w, 400, "invalid outside fixture")
			return
		}
		a.outsideMu.Lock()
		defer a.outsideMu.Unlock()
		_ = a.State.Write(func(m *state.Model) error { m.OutsideSessions = rows; return nil })
		a.outsideRevision++
		snapshot := a.outsideSnapshot()
		for ch := range a.outsideSubscribers {
			select {
			case ch <- snapshot:
			default:
				<-ch
				ch <- snapshot
			}
		}
		a.Events.Begin(r.Context(), "outside_sessions.changed").Set("revision", a.outsideRevision).End("ok", "", nil)
		ok(w, map[string]any{"revision": a.outsideRevision})
	})
}
func (a *api) outsideWS(w http.ResponseWriter, r *http.Request) {
	c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	ch := make(chan map[string]any, 1)
	a.outsideMu.Lock()
	a.outsideSubscribers[ch] = true
	ch <- a.outsideSnapshot()
	a.outsideMu.Unlock()
	defer func() { a.outsideMu.Lock(); delete(a.outsideSubscribers, ch); a.outsideMu.Unlock() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if kind, _, err := c.NextReader(); err == nil && (kind == websocket.TextMessage || kind == websocket.BinaryMessage) {
			c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "outside sessions are read-only"), time.Now().Add(time.Second))
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-r.Context().Done():
			return
		case s := <-ch:
			c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if c.WriteJSON(s) != nil {
				return
			}
		}
	}
}
