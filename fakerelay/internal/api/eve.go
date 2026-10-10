package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
)

const eveWindow = 5 * time.Minute

var errEveClosed = errors.New("eve passkey enrolment is not open")

func (a *api) eveRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/eve/passkey-enrolment", a.eveStatus)
	r.Route(server.ClassConfigure, "POST /api/eve/passkey-enrolment/consume", a.eveConsume)
	r.Route(server.ClassConfigure, "PUT /api/eve/passkeys", a.evePasskeys)
	r.Route(server.ClassRead, "GET /api/eve/passkeys/revocations", a.eveRevocations)
}

// openSeededWindow opens the window for a world that starts with it open.
func (a *api) openSeededWindow() error {
	return a.State.Write(func(m *state.Model) error {
		if m.Eve.EnrolmentOpen && m.Eve.WindowExpires == "" {
			m.Eve.WindowExpires = rfc3339(a.now().Add(eveWindow))
		}
		return nil
	})
}

// windowOpen: an expiry that does not parse reads as expired.
func (a *api) windowOpen(m *state.Model) (string, bool) {
	exp, err := time.Parse(time.RFC3339, m.Eve.WindowExpires)
	if m.Eve.WindowExpires == "" || err != nil || !a.now().Before(exp) {
		return "", false
	}
	return m.Eve.WindowExpires, true
}

func (a *api) eveStatus(w http.ResponseWriter, r *http.Request) {
	var exp string
	var open bool
	a.State.Read(func(m *state.Model) { exp, open = a.windowOpen(m) })
	if !open {
		ok(w, map[string]any{"open": false})
		return
	}
	ok(w, map[string]any{"open": true, "expires": exp})
}

func (a *api) eveConsume(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "eve.enrolment.consume")
	var claim struct {
		IP    string `json:"ip"`
		Label string `json:"label"`
	}
	if err := decode(w, r, 4<<10, &claim, false); err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	var exp string
	err := a.State.Write(func(m *state.Model) error {
		e, open := a.windowOpen(m)
		if !open {
			return errEveClosed
		}
		exp, m.Eve.WindowExpires, m.Eve.EnrolmentOpen = e, "", false
		return nil
	})
	if err != nil {
		ev.End("error", "conflict", err)
		server.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	ev.End("ok", "", nil)
	ok(w, map[string]string{"expires": exp})
}

type passkey struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Created  string `json:"created"`
	LastUsed string `json:"last_used"`
	Reported string `json:"reported,omitempty"`
}

func passkeys(m *state.Model) []passkey {
	out := []passkey{}
	for _, raw := range m.Eve.Passkeys {
		var p passkey
		if json.Unmarshal(raw, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// revocations are pending ids, stored as strings or {"id"} objects.
func revocations(m *state.Model) []string {
	out := []string{}
	for _, raw := range m.Eve.Revocations {
		var s string
		var o struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &s) == nil {
			out = append(out, s)
		} else if json.Unmarshal(raw, &o) == nil && o.ID != "" {
			out = append(out, o.ID)
		}
	}
	return out
}

func (a *api) evePasskeys(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "eve.passkey.report")
	var body struct {
		Passkeys []passkey `json:"passkeys"`
	}
	if err := decode(w, r, 1<<20, &body, false); err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	for _, p := range body.Passkeys {
		if p.ID == "" {
			ev.End("error", "invalid", errors.New("passkey id is required"))
			server.WriteError(w, http.StatusBadRequest, "passkey id is required")
			return
		}
	}
	pending := []string{}
	_ = a.State.Write(func(m *state.Model) error {
		present := map[string]bool{}
		for _, p := range body.Passkeys {
			present[p.ID] = true
		}
		// The report is the acknowledgement: a pending revocation survives
		// only while its id is still listed and the list is not just that id.
		old := revocations(m)
		m.Eve.Passkeys, m.Eve.Revocations = nil, nil
		for _, p := range body.Passkeys {
			p.Reported = rfc3339(a.now())
			b, _ := json.Marshal(p)
			m.Eve.Passkeys = append(m.Eve.Passkeys, b)
		}
		for _, id := range old {
			if present[id] && len(body.Passkeys) > 1 {
				pending = append(pending, id)
				b, _ := json.Marshal(map[string]string{"id": id, "requested": rfc3339(a.now())})
				m.Eve.Revocations = append(m.Eve.Revocations, b)
			}
		}
		return nil
	})
	ev.Set("count", len(body.Passkeys)).End("ok", "", nil)
	ok(w, map[string]any{"revocations": pending})
}

func (a *api) eveRevocations(w http.ResponseWriter, r *http.Request) {
	var ids []string
	a.State.Read(func(m *state.Model) { ids = revocations(m) })
	ok(w, map[string]any{"revocations": ids})
}

func abbrev(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
