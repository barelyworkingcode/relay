package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessionhistory"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// historyOps is the one core behind GET /api/session-history and the
// session.history admin op.
type historyOps struct {
	index *sessionhistory.Index
	store config.SettingsStore
	// deps and inv back the resume door; zero values refuse it.
	deps sessionRouteDeps
	inv  *outsidesessions.Inventory

	mu sync.Mutex
	// hostRoots holds each host project's resolved folder from the last scan, so
	// a row's project match costs no round trip to the host.
	hostRoots map[string]string
}

func (o *historyOps) List(projectID string) sessionhistory.Response {
	return o.index.Rows(projectID)
}

func newHistoryOps(store config.SettingsStore, clock serverClock, deps sessionRouteDeps, inv *outsidesessions.Inventory) *historyOps {
	o := &historyOps{store: store, deps: deps, inv: inv, hostRoots: map[string]string{}}
	opts := sessionhistory.Options{
		Purge:   o.purgeDeps(),
		Clock:   clock,
		Ledger:  deps.sessions,
		Readers: []sessionhistory.Reader{sessionhistory.NewClaude()},
		Machines: func() []sessionhistory.Machine {
			s := config.FreshSettings(store)
			machines := []sessionhistory.Machine{o.machine(nil, s.Projects)}
			for _, h := range s.Hosts {
				if h.Probe != nil && (sshhost.IsWindowsOS(h.Probe.OS) || h.Probe.OS == "Windows") {
					continue
				}
				for _, p := range s.Projects {
					if p.HostID == h.ID {
						host := h
						machines = append(machines, o.machine(&host, s.Projects))
						break
					}
				}
			}
			return machines
		},
		View: func() sessionhistory.View { return o.view(inv) },
	}
	if deps.ready() {
		opts.Managed = deps.host().ManagedClaudeIdentities
	}
	o.index = sessionhistory.New(opts)
	return o
}

// purgeDeps wires the purge pass: the grace period from fresh settings, the
// auditing switch, and the durable session_purge row written before a removal.
// An out-of-range setting turns purge off and warns once per distinct value.
func (o *historyOps) purgeDeps() sessionhistory.PurgeDeps {
	rec := o.deps.auditor
	var warned *int
	return sessionhistory.PurgeDeps{
		Grace: func() time.Duration {
			s := config.FreshSettings(o.store)
			grace := s.PurgeGrace()
			var days *int
			if grace == 0 && s.SessionHistory != nil {
				days = s.SessionHistory.PurgeAfterDays
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			if days != nil && (*days < 0 || *days > config.SessionHistoryPurgeMaxDays) && (warned == nil || *warned != *days) {
				slog.Warn("session history: purge_after_days out of range, purge is off", "value", *days, "max", config.SessionHistoryPurgeMaxDays)
			}
			if days != nil {
				v := *days
				warned = &v
			} else {
				warned = nil
			}
			return grace
		},
		Auditing: rec.Enabled,
		Audit: func(r ledger.Record) error {
			grace := config.FreshSettings(o.store).PurgeGrace()
			args := map[string]any{
				"session_id": r.SessionID, "record": "launched", "kind": r.Kind, "agent": r.Agent,
				"conversation_id": r.ConversationID, "grace_days": int(grace / (24 * time.Hour)),
			}
			if r.State == ledger.StateDiscovered {
				args["record"] = "discovered"
			}
			if r.HostID != "" {
				args["host_id"] = r.HostID
			}
			if r.MissingSince != nil {
				args["missing_since"] = r.MissingSince.UTC().Format(time.RFC3339)
			}
			raw, err := json.Marshal(args)
			if err != nil {
				return err
			}
			return rec.RecordDurable(audit.AuditEvent{
				ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventSessionPurge,
				Actor:   audit.AuditActor{Kind: audit.AuditActorService, Auth: audit.AuditAuthNone, ServiceID: config.RelaySessionsServiceID},
				Outcome: audit.AuditOutcomeOK, Args: raw, ArgsBytes: len(raw),
			})
		},
	}
}

// view takes one settings snapshot, resolves the console's project folders
// once, and answers every question of one Rows call or scan from them.
func (o *historyOps) view(inv *outsidesessions.Inventory) sessionhistory.View {
	s := config.FreshSettings(o.store)
	var consoleRoots []string
	hostOf := map[string]string{}
	for _, p := range s.Projects {
		hostOf[p.ID] = p.HostID
	}
	v := sessionhistory.View{
		Project: func(machineKey, directory string) *string {
			var h *config.Host
			roots := consoleRoots
			if machineKey != "console" {
				h = &config.Host{ID: machineKey}
				roots = o.resolved(h, s.Projects)
			} else if roots == nil {
				consoleRoots = o.resolved(nil, s.Projects)
				roots = consoleRoots
			}
			p := matchOutsideProject(s.Projects, roots, h, directory)
			if p == nil {
				return nil
			}
			id := p.ID
			return &id
		},
		RecordMachine: func(r ledger.Record) string {
			if r.Detached != nil && r.Detached.HostID != "" {
				return r.Detached.HostID
			}
			if host := hostOf[r.ProjectID]; r.ProjectID != "" && host != "" {
				return host
			}
			return "console"
		},
	}
	if inv != nil {
		v.Held = inv.Held
	}
	return v
}

// machine builds the console (h nil) or one host. A host reads only the folders
// of its projects; the resolved names come from the host at scan time.
func (o *historyOps) machine(h *config.Host, projects []config.Project) sessionhistory.Machine {
	m := sessionhistory.Machine{Key: "console", Name: "Console"}
	if h != nil {
		id := h.ID
		m = sessionhistory.Machine{Key: h.ID, Name: h.Name, HostID: &id, Prefixes: o.prefixes(h, projects)}
		m.Scope = func(ctx context.Context) []string {
			s := config.FreshSettings(o.store)
			current, err := freshOutsideHost(o.store, h)
			if err != nil || current == nil {
				return m.Prefixes
			}
			roots := resolveOutsideProjectPaths(ctx, current, s.Projects)
			o.mu.Lock()
			for j, p := range s.Projects {
				if roots[j] != "" {
					o.hostRoots[p.ID] = roots[j]
				}
			}
			o.mu.Unlock()
			return o.prefixes(current, s.Projects)
		}
	}
	m.Run = func(ctx context.Context, script string) ([]byte, error) {
		current, err := freshOutsideHost(o.store, h)
		if err != nil {
			return nil, err
		}
		return outsideRun(ctx, current, script)
	}
	return m
}

// prefixes lists the mangled folder names of one host's projects, raw and
// resolved, the way the ended scan scopes a host.
func (o *historyOps) prefixes(h *config.Host, projects []config.Project) []string {
	out := []string{}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range projects {
		if p.IsRemote() || p.HostID != hostKey(h) {
			continue
		}
		for _, root := range []string{p.Path, o.hostRoots[p.ID]} {
			if root != "" {
				out = append(out, outsidesessions.FolderName(root))
			}
		}
	}
	return out
}

// resolved parallels projects: the console resolves symbolic links locally, a
// host answers from the last scan's cache.
func (o *historyOps) resolved(h *config.Host, projects []config.Project) []string {
	if h == nil {
		return resolveOutsideProjectPaths(context.Background(), nil, projects)
	}
	out := make([]string, len(projects))
	o.mu.Lock()
	defer o.mu.Unlock()
	for j, p := range projects {
		out[j] = o.hostRoots[p.ID]
	}
	return out
}

// recordMachineKey is the machine a launched record lives on: its detached
// host, else its project's host, else the console.
func recordMachineKey(store config.SettingsStore, r ledger.Record) string {
	if r.Detached != nil && r.Detached.HostID != "" {
		return r.Detached.HostID
	}
	if r.ProjectID != "" {
		if p, _ := config.FindProjectByID(config.FreshSettings(store), r.ProjectID); p != nil && p.HostID != "" {
			return p.HostID
		}
	}
	return "console"
}

// historyResumeBody is the resume door's request: the new terminal's size.
type historyResumeBody struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// historyResumeOutcome is a status and a JSON body; Code is the stable refusal
// code, and the body's "error" is the message.
type historyResumeOutcome struct {
	Status int
	Body   any
	Code   string
}

func historyRefusal(status int, code, message string) historyResumeOutcome {
	return historyResumeOutcome{Status: status, Code: code, Body: map[string]string{"error": message, "code": code}}
}

// Resume opens a claude-code terminal on the conversation of a dormant terminal
// record or a discovered one. The checks run in a fixed order and the first
// failure answers; nothing starts before AuthorizeLaunch passes. A discovered
// record never carries authority: its folder is derived from a transcript read
// now and must lie in a registered local project.
func (o *historyOps) Resume(ctx context.Context, caller LaunchCaller, id string, body historyResumeBody) historyResumeOutcome {
	ev := logging.BeginEvent(ctx, "session.history.resume").Set("session_id", id)
	out := o.resume(ctx, ev, caller, id, body)
	switch {
	case out.Status < 300:
		ev.End(logging.OutcomeOK, "", nil)
	case out.Status >= 500:
		ev.End(logging.OutcomeError, out.Code, nil)
	default:
		ev.End(logging.OutcomeDenied, out.Code, nil)
	}
	return out
}

func (o *historyOps) resume(ctx context.Context, ev *logging.Event, caller LaunchCaller, id string, body historyResumeBody) historyResumeOutcome {
	d := o.deps
	if o.index == nil || !d.ready() || d.sessions == nil || o.inv == nil {
		return historyRefusal(http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
	}
	rec, ok := d.sessions.Get(id)
	if !ok {
		return historyRefusal(http.StatusNotFound, "history_not_found", "history entry not found")
	}
	if rec.Kind != KindPTY && rec.State != ledger.StateDiscovered {
		return historyRefusal(http.StatusConflict, "use_session_resume", "resume this session with the session resume door")
	}
	if rec.State == ledger.StateLive {
		return historyRefusal(http.StatusConflict, "live", "this terminal is still running")
	}
	if !d.resumeGuard.tryAcquire(id) {
		return historyRefusal(http.StatusConflict, "resume_in_progress", "a resume for this entry is already in progress")
	}
	defer d.resumeGuard.release(id)
	if (rec.State == ledger.StateDiscovered && rec.HostID != "") || (rec.State != ledger.StateDiscovered && recordMachineKey(o.store, rec) != "console") {
		return historyRefusal(http.StatusConflict, "host_unsupported", "resume on an SSH host is not supported")
	}
	if rec.ConversationID == "" {
		return historyRefusal(http.StatusConflict, "no_conversation", "this entry has no conversation to resume")
	}
	held, fresh := o.inv.Held("console")
	managed, err := d.host().ManagedClaudeIdentities(ctx)
	if !fresh || err != nil {
		return historyRefusal(http.StatusBadGateway, "unavailable", "relay could not check whether the conversation is running")
	}
	if held[rec.ConversationID] || managed[rec.ConversationID] {
		return historyRefusal(http.StatusConflict, "held", "a running session holds this conversation")
	}
	entry, err := o.index.Verify(ctx, id)
	switch {
	case errors.Is(err, sessionhistory.ErrTranscriptAbsent):
		return historyRefusal(http.StatusConflict, "transcript_absent", "the transcript no longer exists")
	case errors.Is(err, sessionhistory.ErrNoFolder):
		return historyRefusal(http.StatusConflict, "no_project", "the transcript names no folder of its own")
	case err != nil:
		return historyRefusal(http.StatusBadGateway, "unavailable", "relay could not read the transcript")
	}
	projectID, directory := rec.ProjectID, rec.Directory
	if rec.State == ledger.StateDiscovered {
		settings := config.FreshSettings(o.store)
		p := matchOutsideProject(settings.Projects, resolveOutsideProjectPaths(ctx, nil, settings.Projects), nil, entry.Directory)
		if p == nil {
			return historyRefusal(http.StatusConflict, "no_project", "the conversation's folder is in no registered project")
		}
		projectID, directory = p.ID, entry.Directory
	} else if entry.Directory != rec.Directory {
		return historyRefusal(http.StatusConflict, "no_project", "the transcript is no longer in this terminal's folder")
	}
	result, resp, refusal, err := d.launch(ctx, LaunchRequest{
		Caller: caller, ProjectID: projectID, Kind: KindPTY, Directory: directory,
		TemplateID: kindTemplateIDs[KindClaude], Cols: body.Cols, Rows: body.Rows,
		ResumeConversation: rec.ConversationID,
	})
	if refusal != nil {
		return historyRefusal(refusal.Status, refusal.Code, refusal.Message)
	}
	if err != nil {
		return historyRefusal(http.StatusBadGateway, "unavailable", "launch failed")
	}
	// The new record already carries the conversation, so linking removes the old
	// one in the same save.
	if _, err := d.sessions.LinkConversation(result.SessionID, "console", rec.ConversationID, directory, func(r ledger.Record) string { return recordMachineKey(o.store, r) }); err != nil {
		slog.Warn("session history: old record not removed after resume", "session", id, "error", err)
	}
	ev.Set("new_session_id", result.SessionID)
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(createdBody(resp.Body), &fields)
	superseded, _ := json.Marshal(id)
	fields["supersedes"] = superseded
	return historyResumeOutcome{Status: http.StatusCreated, Body: fields}
}
