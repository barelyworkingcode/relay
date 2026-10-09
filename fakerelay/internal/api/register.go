// Package api serves the configuration doors: projects, Chief of Staff config,
// hosts, MCPs, terminal templates, Eve passkeys and the audit routes, plus the
// verbs that go with them.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/fakes"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

type api struct {
	server.Deps
	started time.Time
	calls   *fakes.CallLog

	mu      sync.Mutex
	extra   map[string]map[string]json.RawMessage // project fields the model has no slot for
	probeAt map[string]time.Time
}

// Register installs the doors and verbs.
func Register(r server.Registrar, d server.Deps) error {
	a := &api{Deps: d, started: d.Clock.Now(), calls: fakes.NewCallLog(d.Dir), extra: map[string]map[string]json.RawMessage{}, probeAt: map[string]time.Time{}}
	if err := a.openSeededWindow(); err != nil {
		return err
	}
	a.projectRoutes(r)
	a.cosConfigRoutes(r)
	a.hostRoutes(r)
	a.templateRoutes(r)
	a.mcpRoutes(r)
	a.eveRoutes(r)
	a.auditRoutes(r)
	a.verbs(r)
	return nil
}

func (a *api) now() time.Time { return a.Clock.Now().UTC() }

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

var errTooLarge = errors.New("request body too large")

// readBody reads a body of at most max bytes; a longer one is errTooLarge.
func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return nil, errTooLarge
	}
	return body, err
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any, strict bool) error {
	body, err := readBody(w, r, max)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if strict {
		dec.DisallowUnknownFields()
	}
	return dec.Decode(v)
}

func ok(w http.ResponseWriter, v any) { server.WriteJSON(w, http.StatusOK, v) }

func invalidJSON(w http.ResponseWriter, err error) {
	server.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
}

// gate asks for presence. It returns true when the operation may run. A
// refusal is written (403) and the event ended; a timeout waits for the caller
// to leave and writes no response.
func (a *api) gate(ctx context.Context, w http.ResponseWriter, ev *events.Event, op string) bool {
	err := a.Presence.Require(ctx, op)
	if err == nil {
		return true
	}
	reason := server.PresenceReason(err)
	if reason == "" {
		reason = "presence_unavailable"
	}
	ev.End("denied", reason, err)
	if reason != "presence_timeout" {
		server.WriteError(w, http.StatusForbidden, "presence was refused")
	}
	return false
}

func findProject(m *state.Model, id string) (world.Project, bool) {
	for _, p := range m.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return world.Project{}, false
}

func findHost(m *state.Model, id string) (*world.Host, bool) {
	for i := range m.Hosts {
		if m.Hosts[i].ID == id {
			return &m.Hosts[i], true
		}
	}
	return nil, false
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sortedIDs[T any](rows []T, id func(T) string) []T {
	out := append([]T(nil), rows...)
	sort.Slice(out, func(i, j int) bool { return id(out[i]) < id(out[j]) })
	return out
}
