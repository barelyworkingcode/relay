package api

import (
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
	"github.com/gorilla/websocket"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	outsideLimitPattern  = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
	outsideCursorPattern = regexp.MustCompile(`^[1-9][0-9]{0,14}$`)
)

// outsideTranscript mirrors relay's transcript route: the same checks in the
// same order, the same codes and the same 200 shape. It pages by entry, with
// ids e<index> and the oldest returned index as cursor.
func (a *api) outsideTranscript(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" {
		cosError(w, 400, "invalid_request", "this request takes no body")
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if (key != "before" && key != "limit") || len(values) != 1 {
			cosError(w, 400, "invalid_request", "only one before and one limit are accepted")
			return
		}
	}
	limit := 50
	if values, ok := query["limit"]; ok {
		n, err := strconv.Atoi(values[0])
		if !outsideLimitPattern.MatchString(values[0]) || err != nil || n > 200 {
			cosError(w, 400, "invalid_limit", "limit must be 1 to 200")
			return
		}
		limit = n
	}
	before := -1
	if values, ok := query["before"]; ok {
		n, err := strconv.ParseInt(values[0], 10, 64)
		if !outsideCursorPattern.MatchString(values[0]) || err != nil {
			cosError(w, 400, "invalid_cursor", "before is not a cursor")
			return
		}
		before = int(n)
	}
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
		cosError(w, 502, "outside_status_unavailable", "outside transcript could not be read")
		return
	}
	end := len(found.Transcript)
	if before > end {
		cosError(w, 400, "invalid_cursor", "before is past the end of the transcript")
		return
	}
	if before > 0 {
		end = before
	}
	first := end - limit
	if first < 0 {
		first = 0
	}
	entries := []map[string]any{}
	for k := first; k < end; k++ {
		t := found.Transcript[k]
		entries = append(entries, map[string]any{"id": "e" + strconv.Itoa(k), "role": t.Role, "text": t.Text, "at": t.At})
	}
	var cursor any
	if first > 0 {
		cursor = strconv.Itoa(first)
	}
	ok(w, map[string]any{"entries": entries, "cursor": cursor, "hasMore": first > 0})
}
func outsideRow(s world.OutsideFixture) map[string]any {
	return map[string]any{"id": s.ID, "name": s.Name, "projectId": s.ProjectID, "hostId": s.HostID, "machine": s.Machine, "directory": s.Directory, "branch": s.Branch, "state": s.State, "since": s.Since, "updatedAt": s.UpdatedAt, "source": s.Source, "outside": true, "readOnly": true, "tmuxSession": s.TmuxSession, "viewable": s.Viewable, "viewReason": s.ViewReason}
}

// outsideSnapshot lists every probed machine the way relay does: the console,
// plus each SSH host that has a project. A host whose probe reports a Windows
// shell is "unsupported" and contributes no rows. Fixture rows may also name a
// machine the world does not declare; it is listed too, so a scenario can
// stage one without a host.
func (a *api) outsideSnapshot() map[string]any {
	rows := []map[string]any{}
	type machine struct {
		hostID *string
		name   string
		status string
	}
	byKey := map[string]*machine{"": {name: "Console", status: "reachable"}}
	unsupported := map[string]bool{}
	a.State.Read(func(m *state.Model) {
		for _, h := range m.Hosts {
			for _, p := range m.Projects {
				if p.HostID != h.ID {
					continue
				}
				id := h.ID
				mc := &machine{hostID: &id, name: h.Name, status: "reachable"}
				if h.Probe != nil && (strings.HasPrefix(h.Probe.OS, "MINGW") || strings.HasPrefix(h.Probe.OS, "MSYS") || strings.HasPrefix(h.Probe.OS, "CYGWIN") || h.Probe.OS == "Windows") {
					mc.status = "unsupported"
					unsupported[h.ID] = true
				}
				byKey[h.ID] = mc
				break
			}
		}
		for _, s := range m.OutsideSessions {
			key := ""
			if s.HostID != nil {
				key = *s.HostID
			}
			if unsupported[key] {
				continue
			}
			rows = append(rows, outsideRow(s))
			mc := byKey[key]
			if mc == nil {
				mc = &machine{hostID: s.HostID, name: s.Machine, status: "reachable"}
				byKey[key] = mc
			}
			if s.MachineStatus != "" {
				mc.status = s.MachineStatus
			}
		}
	})
	machines := []map[string]any{}
	list := make([]*machine, 0, len(byKey))
	for _, mc := range byKey {
		list = append(list, mc)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	for _, mc := range list {
		row := map[string]any{"hostId": mc.hostID, "machine": mc.name, "status": mc.status, "lastProbeAt": rfc3339(a.now())}
		if mc.status == "unreachable" {
			row["error"] = "outside probe unavailable"
		}
		machines = append(machines, row)
	}
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
	r.Route(server.ClassExecute, "GET /api/outside-sessions/{id}/transcript", a.outsideTranscript)
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
