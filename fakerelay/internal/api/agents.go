package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const (
	maxAgentBody = 64 << 10
	localMachine = "this Mac"
)

// agentErr is the coded error family: {"error":code,"message":text}, plus
// command and machine on a lookup refusal.
type agentErr struct {
	status           int
	code, msg        string
	command, machine string
}

func (e *agentErr) Error() string { return e.msg }

func (e *agentErr) reason() string {
	switch {
	case e.status == http.StatusNotFound:
		return "not_found"
	case e.status == http.StatusConflict:
		return "conflict"
	case e.status == http.StatusBadGateway:
		return "upstream"
	}
	return "invalid"
}

func (e *agentErr) write(w http.ResponseWriter) {
	body := map[string]string{"error": e.code, "message": e.msg}
	if e.command != "" {
		body["command"] = e.command
	}
	if e.machine != "" && (e.command != "" || e.code == "machine_unreachable") {
		body["machine"] = e.machine
	}
	server.WriteJSON(w, e.status, body)
}

func ruleToErr(r *world.AgentRuleError) *agentErr {
	status := http.StatusBadRequest
	if r.Code == "name_taken" {
		status = http.StatusConflict
	}
	return &agentErr{status: status, code: r.Code, msg: r.Message}
}

func (a *api) agentRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/projects/{id}/agents", a.listAgents)
	r.Route(server.ClassConfigure, "POST /api/projects/{id}/agents", a.addAgent)
	r.Route(server.ClassConfigure, "PUT /api/projects/{id}/agents/{name}", a.updateAgent)
	r.Route(server.ClassConfigure, "DELETE /api/projects/{id}/agents/{name}", a.removeAgent)
}

func (a *api) listAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "agent.list").Set("project_id", id)
	var p world.Project
	var found bool
	a.State.Read(func(m *state.Model) { p, found = findProject(m, id) })
	if !found {
		e := &agentErr{status: http.StatusNotFound, code: "project_not_found", msg: "project not found"}
		ev.End("error", e.reason(), e)
		e.write(w)
		return
	}
	entries := p.Agents
	if entries == nil || p.Kind == "remote" {
		entries = []world.AgentEntry{}
	}
	ev.Set("count", len(entries)).End("ok", "", nil)
	ok(w, map[string]any{"projectId": id, "entries": entries})
}

// decodeAgent reads a strict AgentEntry body of at most 64 KiB.
func decodeAgent(w http.ResponseWriter, r *http.Request) (world.AgentEntry, *agentErr) {
	var in world.AgentEntry
	bad := func(msg string) *agentErr {
		return &agentErr{status: http.StatusBadRequest, code: "invalid_body", msg: msg}
	}
	body, err := readBody(w, r, maxAgentBody)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return in, bad("request body too large")
		}
		return in, bad("invalid JSON: " + err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, bad("invalid JSON: " + err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return in, bad("invalid JSON: unexpected data after the value")
	}
	return in, nil
}

func (a *api) addAgent(w http.ResponseWriter, r *http.Request) {
	in, derr := decodeAgent(w, r)
	if derr != nil {
		derr.write(w)
		return
	}
	a.writeAgent(w, r, "agent.add", r.PathValue("id"), "", in)
}

func (a *api) updateAgent(w http.ResponseWriter, r *http.Request) {
	in, derr := decodeAgent(w, r)
	if derr != nil {
		derr.write(w)
		return
	}
	a.writeAgent(w, r, "agent.update", r.PathValue("id"), r.PathValue("name"), in)
}

func (a *api) removeAgent(w http.ResponseWriter, r *http.Request) {
	a.writeAgent(w, r, "agent.remove", r.PathValue("id"), r.PathValue("name"), world.AgentEntry{})
}

// writeAgent runs the shared write path of add, update and remove in the
// pinned validation order. target is the existing agent's name for an update
// or remove.
func (a *api) writeAgent(w http.ResponseWriter, r *http.Request, op, id, target string, in world.AgentEntry) {
	remove := op == "agent.remove"
	name := in.Name
	if target != "" {
		name = target
	}
	ev := a.Events.Begin(r.Context(), op).Set("project_id", id).Set("name", name)
	refuse := func(e *agentErr) {
		ev.End("error", e.reason(), e)
		e.write(w)
	}
	if !remove {
		if rule := world.ValidateAgentShape(in); rule != nil {
			refuse(ruleToErr(rule))
			return
		}
	}
	var snap world.Project
	var host *world.Host
	var found bool
	a.State.Read(func(m *state.Model) {
		snap, found = findProject(m, id)
		if found && snap.HostID != "" {
			if h, hok := findHost(m, snap.HostID); hok {
				c := *h
				host = &c
			}
		}
	})
	switch {
	case !found:
		refuse(&agentErr{status: http.StatusNotFound, code: "project_not_found", msg: "project not found"})
		return
	case snap.Kind == "remote":
		refuse(&agentErr{status: http.StatusBadRequest, code: "project_not_available", msg: "project is not available for agents"})
		return
	case !remove && in.Kind == "pi" && snap.HostID != "":
		refuse(&agentErr{status: http.StatusBadRequest, code: "kind_not_available_on_host", msg: "pi is not available on a host project"})
		return
	}
	var old world.AgentEntry
	if target == "" {
		if len(snap.Agents) >= world.MaxAgentEntries {
			refuse(tooMany())
			return
		}
	} else {
		var hit bool
		if old, _, hit = world.FindAgent(snap, target); !hit {
			refuse(&agentErr{status: http.StatusNotFound, code: "entry_not_found", msg: `no agent "` + target + `" in ` + snap.Name})
			return
		}
	}
	if !remove {
		if rule := world.AgentNameClash(snap, in.Name, target); rule != nil {
			refuse(ruleToErr(rule))
			return
		}
	}
	machine := localMachine
	if host != nil {
		machine = host.Name
	}
	if remove && old.Command != "" || !remove && in.Command != "" {
		ev.Set("machine", machine)
	}
	if !remove && in.Command != "" {
		abs, e := a.lookupAgentCommand(snap, host, machine, in.Command)
		if e != nil {
			refuse(e)
			return
		}
		in.Command = abs
	}
	if !a.gate(server.WithSubject(r.Context(), snap.Name), w, ev, "project.agent") {
		return
	}
	err := a.State.Write(func(m *state.Model) error {
		for i := range m.Projects {
			if m.Projects[i].ID != id {
				continue
			}
			cur := &m.Projects[i]
			if cur.HostID != snap.HostID || cur.Kind != snap.Kind {
				return changedErr(snap.Name)
			}
			at := -1
			if cur2, idx, hit := world.FindAgent(*cur, firstNonEmpty(target, in.Name)); hit {
				at = idx
				if target == "" || !reflect.DeepEqual(cur2, old) {
					return changedErr(snap.Name)
				}
			} else if target != "" {
				return changedErr(snap.Name)
			}
			switch {
			case target == "" && len(cur.Agents) >= world.MaxAgentEntries:
				return tooMany()
			case target == "":
				cur.Agents = append(cur.Agents, in)
			case remove:
				cur.Agents = append(cur.Agents[:at:at], cur.Agents[at+1:]...)
			default:
				cur.Agents = append([]world.AgentEntry(nil), cur.Agents...)
				cur.Agents[at] = in
			}
			return nil
		}
		return changedErr(snap.Name)
	})
	if err != nil {
		var ae *agentErr
		if errors.As(err, &ae) {
			refuse(ae)
		} else {
			refuse(&agentErr{status: http.StatusInternalServerError, code: "internal", msg: err.Error()})
		}
		return
	}
	ev.End("ok", "", nil)
	switch {
	case remove:
		ok(w, map[string]any{"projectId": id, "name": target})
	default:
		status := http.StatusOK
		if target == "" {
			status = http.StatusCreated
		}
		server.WriteJSON(w, status, map[string]any{"projectId": id, "projectName": snap.Name, "machine": machine, "entry": in})
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func tooMany() *agentErr {
	return &agentErr{status: http.StatusBadRequest, code: "too_many_entries", msg: "a project holds at most 32 agents"}
}

func changedErr(project string) *agentErr {
	return &agentErr{status: http.StatusConflict, code: "project_changed", msg: project + " changed while the request was being approved; try again"}
}

// lookupAgentCommand resolves a command the way the world's PATH sees it: a
// plain walk, no shell. A hosted project looks on host_path, else local_path.
func (a *api) lookupAgentCommand(p world.Project, host *world.Host, machine, command string) (string, *agentErr) {
	if host != nil && a.agentStatus(*host) == "unreachable" {
		why := a.Hosts.HostStatuses()[host.ID].Error
		if why == "" {
			why = "connection timed out"
		}
		return "", &agentErr{status: http.StatusBadGateway, code: "machine_unreachable", msg: machine + " could not be reached: " + why, machine: machine}
	}
	pathList := a.World.LocalPath
	if p.HostID != "" {
		pathList = a.World.HostPath
	}
	if abs := resolveCommand(pathList, command); abs != "" {
		return abs, nil
	}
	return "", &agentErr{status: http.StatusUnprocessableEntity, code: "command_not_found", msg: command + " was not found on " + machine, command: command, machine: machine}
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// resolveCommand returns the absolute path of an executable, or "". An
// absolute input must itself exist and be executable; a bare name is walked
// over pathList (fakerelay's own PATH when empty). Relative PATH entries are
// skipped, so the result is always absolute.
func resolveCommand(pathList, command string) string {
	if strings.HasPrefix(command, "/") {
		if isExecutable(command) {
			return command
		}
		return ""
	}
	if pathList == "" {
		pathList = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(pathList) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if p := filepath.Join(dir, command); isExecutable(p) {
			return p
		}
	}
	return ""
}
