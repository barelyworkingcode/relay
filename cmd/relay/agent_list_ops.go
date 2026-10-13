package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/cmdlookup"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/project"
)

const (
	auditCredentialAgentEntry = "agent_entry"

	// agentMachineLocal names the console in prompts, errors and the wire.
	agentMachineLocal = "this Mac"

	agentActionAdd    = "add"
	agentActionUpdate = "update"
	agentActionRemove = "remove"
)

// AgentListOps is the one core behind the agent-list HTTP routes, the
// Settings IPC messages, the admin ops and the CLI verbs. Every add, update
// and remove is gated: an entry's command is what a later session start runs,
// so a write is an execute-class grant for the project.
type AgentListOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	// Gate is the presence check every write demands. A nil Gate refuses.
	Gate     *presence.Gate
	Issuance IssuanceAuditor
	// Lookup resolves a command name to an absolute path on the project's
	// machine and names that machine. An empty name resolves nothing and only
	// names the machine.
	Lookup func(ctx context.Context, proj *config.Project, name string) (abs, machine string, err error)
}

// AgentWriteResult is what a successful add or update answers.
type AgentWriteResult struct {
	ProjectID   string            `json:"projectId"`
	ProjectName string            `json:"projectName"`
	Machine     string            `json:"machine"`
	Entry       config.AgentEntry `json:"entry"`
}

// AgentError is a coded refusal. Command and Machine are set by the lookup
// refusals only.
type AgentError struct {
	Status  int
	Code    string
	Message string
	Command string
	Machine string
}

func (e *AgentError) Error() string { return e.Message }

func agentErr(status int, code, format string, a ...any) *AgentError {
	return &AgentError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func agentRuleErr(r *project.AgentRuleError) *AgentError {
	status := 400
	if r.Code == "name_taken" {
		status = 409
	}
	return &AgentError{Status: status, Code: r.Code, Message: r.Message}
}

var errAgentLookupUnwired = errors.New("agent command lookup is not wired for this operation")

// agentUnrecordedError means the audit write failed, so the change was not
// made. It classes as an internal failure for the door and as an audit
// refusal for the event.
type agentUnrecordedError struct{}

func (agentUnrecordedError) Error() string {
	return "the change could not be recorded in the audit log, so it was not made"
}
func (agentUnrecordedError) Unwrap() []error { return []error{errProjectSaveFailed, errEventAudit} }

// agentEventErr classes a coded refusal for its event line; everything else
// (presence, audit, internal) already classes itself.
func agentEventErr(err error) error {
	var ae *AgentError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case ae.Status == 404:
		return &eventError{kind: errEventNotFound, cause: err}
	case ae.Status == 409:
		return &eventError{kind: errEventConflict, cause: err}
	case ae.Status == 502:
		return upstreamErr(err)
	case ae.Status >= 400 && ae.Status < 500:
		return markInvalid(err)
	}
	return err
}

func (o *AgentListOps) runCommitted(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

func cloneAgentEntry(e config.AgentEntry) config.AgentEntry {
	e.Args = slices.Clone(e.Args)
	return e
}

func agentEntriesEqual(a, b config.AgentEntry) bool {
	return a.Name == b.Name && a.Kind == b.Kind && a.Model == b.Model && a.Command == b.Command &&
		slices.Equal(a.Args, b.Args)
}

// agentPlan is a validated, resolved request: the project as read, the entry
// being replaced or removed, and the entry to store with its command resolved.
type agentPlan struct {
	action  string
	proj    config.Project
	name    string // the entry addressed (update, remove)
	old     config.AgentEntry
	entry   config.AgentEntry
	machine string
}

func projectSnapshot(p *config.Project) config.Project {
	snap := *p
	snap.Agents = make([]config.AgentEntry, 0, len(p.Agents))
	for _, e := range p.Agents {
		snap.Agents = append(snap.Agents, cloneAgentEntry(e))
	}
	snap.ChatTemplates = slices.Clone(p.ChatTemplates)
	return snap
}

// prepare runs validation order steps 2 to 8.
func (o *AgentListOps) prepare(ctx context.Context, action, projectID, name string, in config.AgentEntry) (agentPlan, error) {
	plan := agentPlan{action: action, name: name}
	if action != agentActionRemove {
		if rule := project.ValidateAgentEntryShape(in); rule != nil {
			return plan, agentRuleErr(rule)
		}
	}
	p, _ := config.FindProjectByID(config.FreshSettings(o.Store), projectID)
	if p == nil {
		return plan, agentErr(404, "project_not_found", "project not found")
	}
	plan.proj = projectSnapshot(p)
	if plan.proj.IsRemote() {
		return plan, agentErr(400, "project_not_available", "project is not available for agents")
	}
	if action != agentActionRemove && in.Kind == "pi" && plan.proj.IsHosted() {
		return plan, agentErr(400, "kind_not_available_on_host", "pi is not available on a host project")
	}
	switch action {
	case agentActionAdd:
		if len(plan.proj.Agents) >= project.MaxAgentEntries {
			return plan, agentErr(400, "too_many_entries", "a project holds at most 32 agents")
		}
	default:
		old, _, ok := project.FindAgentEntry(&plan.proj, name)
		if !ok {
			return plan, agentErr(404, "entry_not_found", "no agent %q in %s", name, plan.proj.Name)
		}
		plan.old = old
	}
	if action == agentActionRemove {
		_, machine, err := o.lookup(ctx, &plan.proj, "")
		if err != nil {
			return plan, err
		}
		plan.machine = machine
		return plan, nil
	}
	except := ""
	if action == agentActionUpdate {
		except = plan.old.Name
	}
	if rule := project.AgentNameClash(&plan.proj, in.Name, except); rule != nil {
		return plan, agentRuleErr(rule)
	}
	plan.entry = cloneAgentEntry(in)
	abs, machine, err := o.lookup(ctx, &plan.proj, in.Command)
	if err != nil {
		return plan, err
	}
	plan.machine = machine
	plan.entry.Command = abs
	return plan, nil
}

// lookup translates the lookup's errors into the coded refusals.
func (o *AgentListOps) lookup(ctx context.Context, proj *config.Project, command string) (string, string, error) {
	if o.Lookup == nil {
		return "", "", errAgentLookupUnwired
	}
	abs, machine, err := o.Lookup(ctx, proj, command)
	switch {
	case err == nil:
		return abs, machine, nil
	case errors.Is(err, cmdlookup.ErrNotFound):
		return "", machine, &AgentError{Status: 422, Code: "command_not_found",
			Message: fmt.Sprintf("%s was not found on %s", command, machine), Command: command, Machine: machine}
	default:
		text := strings.TrimPrefix(err.Error(), cmdlookup.ErrUnreachable.Error()+": ")
		return "", machine, &AgentError{Status: 502, Code: "machine_unreachable",
			Message: fmt.Sprintf("%s could not be reached: %s", machine, text), Machine: machine}
	}
}

// List answers the project's entries. A remote project has none.
func (o *AgentListOps) List(ctx context.Context, projectID string) (entries []config.AgentEntry, err error) {
	ev := logging.BeginEvent(ctx, "agent.list")
	defer func() {
		ev.Set("project_id", projectID).Set("count", len(entries))
		endEvent(ev, agentEventErr(err))
	}()
	p, _ := config.FindProjectByID(config.FreshSettings(o.Store), projectID)
	if p == nil {
		return nil, agentErr(404, "project_not_found", "project not found")
	}
	entries = make([]config.AgentEntry, 0, len(p.Agents))
	if p.IsRemote() {
		return entries, nil
	}
	for _, e := range p.Agents {
		entries = append(entries, cloneAgentEntry(e))
	}
	return entries, nil
}

// Preview validates and resolves an entry as Add would, and writes nothing
// and asks for nothing.
func (o *AgentListOps) Preview(ctx context.Context, projectID string, in config.AgentEntry) (AgentWriteResult, error) {
	plan, err := o.prepare(ctx, agentActionAdd, projectID, "", in)
	if err != nil {
		return AgentWriteResult{}, err
	}
	return plan.result(projectID), nil
}

func (p agentPlan) result(projectID string) AgentWriteResult {
	return AgentWriteResult{ProjectID: projectID, ProjectName: p.proj.Name, Machine: p.machine, Entry: p.entry}
}

// Add appends an entry.
func (o *AgentListOps) Add(ctx context.Context, projectID string, in config.AgentEntry, via, credID string) (res AgentWriteResult, err error) {
	ev := logging.BeginEvent(ctx, "agent.add")
	defer func() { endAgentWriteEvent(ev, projectID, in.Name, res.Machine, err) }()
	return o.write(ctx, agentActionAdd, projectID, "", in, via, credID)
}

// Update replaces the entry called name with in, which may rename it.
func (o *AgentListOps) Update(ctx context.Context, projectID, name string, in config.AgentEntry, via, credID string) (res AgentWriteResult, err error) {
	ev := logging.BeginEvent(ctx, "agent.update")
	defer func() { endAgentWriteEvent(ev, projectID, in.Name, res.Machine, err) }()
	return o.write(ctx, agentActionUpdate, projectID, name, in, via, credID)
}

// Remove deletes the entry called name.
func (o *AgentListOps) Remove(ctx context.Context, projectID, name string, via, credID string) (err error) {
	ev := logging.BeginEvent(ctx, "agent.remove")
	machine := ""
	defer func() { endAgentWriteEvent(ev, projectID, name, machine, err) }()
	res, err := o.write(ctx, agentActionRemove, projectID, name, config.AgentEntry{}, via, credID)
	machine = res.Machine
	return err
}

func endAgentWriteEvent(ev *logging.Event, projectID, name, machine string, err error) {
	ev.Set("project_id", projectID).Set("name", name)
	if machine != "" {
		ev.Set("machine", machine)
	}
	endEvent(ev, agentEventErr(err))
}

func (o *AgentListOps) write(ctx context.Context, action, projectID, name string, in config.AgentEntry, via, credID string) (AgentWriteResult, error) {
	plan, err := o.prepare(ctx, action, projectID, name, in)
	if err != nil {
		return AgentWriteResult{Machine: plan.machine}, err
	}
	res := plan.result(projectID)
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return res, err
	}
	grant, err := requireGate(o.Gate, ctx, "project.agent", plan.digest(projectID), plan.reason(),
		presenceAttempt{auditor: o.Issuance, via: via, credID: credID, subject: projectID})
	if err != nil {
		return res, err
	}

	var refusal error
	commitErr := o.runCommitted(ctx, func() error {
		saveErr := config.WithDeclinable(o.Store, func(s *config.Settings) error {
			refusal = plan.apply(s, projectID)
			if refusal != nil {
				return refusal
			}
			iss := audit.CredentialIssuance{
				ConfigChange: true,
				Credential:   auditCredentialAgentEntry,
				Subject:      projectID,
				Name:         plan.proj.Name,
				Grants:       plan.grants(),
				Via:          via,
				CredID:       credID,
				PresenceID:   grant.ID(),
			}
			if auditErr := recordIssuance(o.Issuance, iss); auditErr != nil {
				slog.Error("agent entry change declined: audit write failed", "project_id", projectID, "error", auditErr)
				refusal = agentUnrecordedError{}
				return refusal
			}
			return nil
		})
		if refusal != nil {
			return refusal
		}
		if saveErr != nil {
			return fmt.Errorf("%w: %w", errProjectSaveFailed, saveErr)
		}
		return nil
	})
	if commitErr != nil {
		return res, commitErr
	}
	return res, nil
}

// apply re-checks the live record and sets the new list. This is subtle: the
// prompt was up while the record could move, so everything the approval
// covered is compared again here, in the queue.
func (p agentPlan) apply(s *config.Settings, projectID string) error {
	live, _ := config.FindProjectByID(s, projectID)
	changed := agentErr(409, "project_changed", "%s changed while the request was being approved; try again", p.proj.Name)
	if live == nil || live.HostID != p.proj.HostID || live.Kind != p.proj.Kind {
		return changed
	}
	list := make([]config.AgentEntry, 0, len(live.Agents)+1)
	for _, e := range live.Agents {
		list = append(list, cloneAgentEntry(e))
	}
	switch p.action {
	case agentActionAdd:
		if len(list) >= project.MaxAgentEntries {
			return agentErr(400, "too_many_entries", "a project holds at most 32 agents")
		}
		if rule := project.AgentNameClash(live, p.entry.Name, ""); rule != nil {
			return agentRuleErr(rule)
		}
		list = append(list, p.entry)
	default:
		cur, i, ok := project.FindAgentEntry(live, p.old.Name)
		if !ok || !agentEntriesEqual(cur, p.old) {
			return changed
		}
		if p.action == agentActionRemove {
			list = slices.Delete(list, i, i+1)
			break
		}
		if rule := project.AgentNameClash(live, p.entry.Name, p.old.Name); rule != nil {
			return agentRuleErr(rule)
		}
		list[i] = p.entry
	}
	s.SetProjectAgents(projectID, list)
	return nil
}

// runs names what the entry will execute, for the prompt.
func runsText(e config.AgentEntry) string {
	if e.Command != "" {
		return e.Command
	}
	return "the " + e.Kind + " default"
}

func (p agentPlan) reason() string {
	switch p.action {
	case agentActionAdd:
		return fmt.Sprintf("Add agent %q to %s: runs %s on %s", p.entry.Name, p.proj.Name, runsText(p.entry), p.machine)
	case agentActionUpdate:
		r := fmt.Sprintf("Change agent %q in %s: runs %s on %s", p.old.Name, p.proj.Name, runsText(p.entry), p.machine)
		if p.entry.Name != p.old.Name {
			r += fmt.Sprintf(" (renamed to %q)", p.entry.Name)
		}
		return r
	}
	r := fmt.Sprintf("Remove agent %q from %s", p.old.Name, p.proj.Name)
	if p.old.Command != "" {
		r += fmt.Sprintf(": it ran %s on %s", p.old.Command, p.machine)
	}
	return r
}

// digest binds the approval to the resolved entry, so an answer for one
// command cannot be spent on another.
func (p agentPlan) digest(projectID string) presence.Digest {
	b := presence.NewDigestBuilder("project.agent").
		StringField("project_id", true, projectID).
		StringField("action", true, p.action)
	if p.action == agentActionRemove {
		return b.StringField("name", true, p.old.Name).
			StringField("new_name", false, "").
			StringField("kind", false, "").
			StringField("model", false, "").
			StringField("command", false, "").
			StringSeqField("args", false, nil).
			Build()
	}
	name, newName := p.entry.Name, ""
	newNamePresent := false
	if p.action == agentActionUpdate {
		name, newName, newNamePresent = p.old.Name, p.entry.Name, true
	}
	return b.StringField("name", true, name).
		StringField("new_name", newNamePresent, newName).
		StringField("kind", true, p.entry.Kind).
		StringField("model", true, p.entry.Model).
		StringField("command", true, p.entry.Command).
		StringSeqField("args", true, p.entry.Args).
		Build()
}

func (p agentPlan) grants() []string {
	if p.action == agentActionRemove {
		g := []string{"action=remove", "entry=" + p.old.Name}
		if p.old.Command != "" {
			g = append(g, "command="+p.old.Command)
		}
		return g
	}
	g := []string{"action=" + p.action, "entry=" + p.entry.Name, "kind=" + p.entry.Kind}
	if p.entry.Model != "" {
		g = append(g, "model="+p.entry.Model)
	}
	if p.entry.Command != "" {
		g = append(g, "command="+p.entry.Command)
	}
	if len(p.entry.Args) > 0 {
		args, _ := json.Marshal(p.entry.Args)
		g = append(g, "args="+string(args))
	}
	if p.action == agentActionUpdate && p.entry.Name != p.old.Name {
		g = append(g, "renamed_from="+p.old.Name)
	}
	return g
}

// newAgentLookup chooses where a command is looked up: the project's host
// over ssh, else this Mac. This is deliberate: cmdlookup never decides the
// machine, so the choice lives in one place.
func newAgentLookup(store config.SettingsStore) func(ctx context.Context, proj *config.Project, name string) (string, string, error) {
	return func(ctx context.Context, proj *config.Project, name string) (string, string, error) {
		if !proj.IsHosted() {
			if name == "" {
				return "", agentMachineLocal, nil
			}
			abs, err := cmdlookup.Local(ctx, name)
			return abs, agentMachineLocal, err
		}
		host, _ := config.FindHostByID(config.FreshSettings(store), proj.HostID)
		if host == nil {
			return "", proj.HostID, fmt.Errorf("%w: the host is not configured", cmdlookup.ErrUnreachable)
		}
		if name == "" {
			return "", host.Name, nil
		}
		abs, err := cmdlookup.OnHost(ctx, *host, name)
		return abs, host.Name, err
	}
}
