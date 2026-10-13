package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

var errAgentListOpsUnavailable = errors.New("agent list operations are not available in this relay process")

func requireAgentListOps(r *appRouter) (*AgentListOps, error) {
	if r.agentListOps == nil {
		return nil, errAgentListOpsUnavailable
	}
	return r.agentListOps, nil
}

type agentListRequest struct {
	ID string `json:"id"`
}

type agentAddRequest struct {
	ID    string            `json:"id"`
	Entry config.AgentEntry `json:"entry"`
}

type agentUpdateRequest struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Entry config.AgentEntry `json:"entry"`
}

type agentRemoveRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type agentListResult struct {
	ProjectID   string              `json:"projectId"`
	ProjectName string              `json:"projectName"`
	Entries     []config.AgentEntry `json:"entries"`
}

func adminProjectAgentsList(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireAgentListOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[agentListRequest]("project.agents.list", args)
	if err != nil {
		return nil, err
	}
	entries, err := ops.List(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	name := ""
	if p, _ := config.FindProjectByID(config.FreshSettings(ops.Store), req.ID); p != nil {
		name = p.Name
	}
	return marshalAdminResult(agentListResult{ProjectID: req.ID, ProjectName: name, Entries: entries})
}

func adminProjectAgentsAdd(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireAgentListOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[agentAddRequest]("project.agents.add", args)
	if err != nil {
		return nil, err
	}
	res, err := ops.Add(ctx, req.ID, req.Entry, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(res)
}

func adminProjectAgentsUpdate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireAgentListOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[agentUpdateRequest]("project.agents.update", args)
	if err != nil {
		return nil, err
	}
	res, err := ops.Update(ctx, req.ID, req.Name, req.Entry, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(res)
}

func adminProjectAgentsRemove(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireAgentListOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[agentRemoveRequest]("project.agents.remove", args)
	if err != nil {
		return nil, err
	}
	if err := ops.Remove(ctx, req.ID, req.Name, auditViaCLI, ""); err != nil {
		return nil, err
	}
	return marshalAdminResult(map[string]string{"projectId": req.ID, "name": req.Name})
}

func agentRunsText(e config.AgentEntry) string {
	if e.Command == "" {
		return "the " + e.Kind + " default"
	}
	return e.Command
}

func projectAgentsList(args []string) {
	fs := flag.NewFlagSet("project agents list", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	asJSON := fs.Bool("json", false, "print the GET /api/projects/{id}/agents body as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}
	raw := adminCall("relay project agents list", "project.agents.list", agentListRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res agentListResult
	decodeCLIResult(raw, &res)
	if len(res.Entries) == 0 {
		fmt.Println("no agents")
		return
	}
	w := newTabWriter()
	fmt.Fprintln(w, "NAME\tKIND\tMODEL\tCOMMAND\tARGS")
	for _, e := range res.Entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Kind, e.Model, e.Command, strings.Join(e.Args, " "))
	}
	w.Flush()
}

func printAgentWritten(raw json.RawMessage, asJSON bool, verb string) {
	if asJSON {
		printJSONLine(raw)
		return
	}
	var res AgentWriteResult
	decodeCLIResult(raw, &res)
	fmt.Printf("%s agent %s in %s (runs %s on %s)\n", verb, res.Entry.Name, res.ProjectName, agentRunsText(res.Entry), res.Machine)
}

func projectAgentsAdd(args []string) {
	fs := flag.NewFlagSet("project agents add", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	name := fs.String("name", "", "agent name (required)")
	kind := fs.String("kind", "", "claude, codex or pi (required)")
	model := fs.String("model", "", "model (required for pi)")
	command := fs.String("command", "", "program name or absolute path; stored as the resolved absolute path")
	var cmdArgs repeatedFlag
	fs.Var(&cmdArgs, "arg", "argument passed to the command, in order (repeatable)")
	asJSON := fs.Bool("json", false, "print the created entry's body as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}
	if *name == "" || *kind == "" {
		exitError("--name and --kind are required")
	}
	raw := adminCall("relay project agents add", "project.agents.add", agentAddRequest{
		ID:    *id,
		Entry: config.AgentEntry{Name: *name, Kind: *kind, Model: *model, Command: *command, Args: cmdArgs},
	})
	printAgentWritten(raw, *asJSON, "added")
}

// projectAgentsUpdate lists the entries, merges the given flags into the
// current one and sends the whole entry: the update door replaces, it does
// not patch.
func projectAgentsUpdate(args []string) {
	fs := flag.NewFlagSet("project agents update", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	name := fs.String("name", "", "agent to change (required)")
	newName := fs.String("new-name", "", "rename the agent")
	kind := fs.String("kind", "", "claude, codex or pi")
	model := fs.String("model", "", "model")
	noModel := fs.Bool("no-model", false, "clear the model")
	command := fs.String("command", "", "program name or absolute path")
	noCommand := fs.Bool("no-command", false, "clear the command and its args")
	var cmdArgs repeatedFlag
	fs.Var(&cmdArgs, "arg", "argument passed to the command; replaces the current args (repeatable)")
	noArgs := fs.Bool("no-args", false, "clear the args")
	asJSON := fs.Bool("json", false, "print the updated entry's body as JSON")
	fs.Parse(args)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *id == "" {
		exitError("%s", projectIDRequired)
	}
	if *name == "" {
		exitError("--name is required")
	}
	switch {
	case set["model"] && *noModel:
		exitError("--model cannot be combined with --no-model")
	case set["command"] && *noCommand:
		exitError("--command cannot be combined with --no-command")
	case set["arg"] && *noArgs:
		exitError("--arg cannot be combined with --no-args")
	case set["arg"] && *noCommand:
		exitError("--arg cannot be combined with --no-command")
	}

	var listed agentListResult
	decodeCLIResult(adminCall("relay project agents update", "project.agents.list", agentListRequest{ID: *id}), &listed)
	cur, ok := findListedAgent(listed.Entries, *name)
	if !ok {
		exitError("no agent %q in %s", *name, listed.ProjectName)
	}
	entry := cur
	if set["new-name"] {
		entry.Name = *newName
	}
	if set["kind"] {
		entry.Kind = *kind
	}
	if set["model"] {
		entry.Model = *model
	}
	if *noModel {
		entry.Model = ""
	}
	if set["command"] {
		entry.Command = *command
	}
	if *noCommand {
		entry.Command, entry.Args = "", nil
	}
	if set["arg"] {
		entry.Args = cmdArgs
	}
	if *noArgs {
		entry.Args = nil
	}
	raw := adminCall("relay project agents update", "project.agents.update", agentUpdateRequest{ID: *id, Name: cur.Name, Entry: entry})
	printAgentWritten(raw, *asJSON, "updated")
}

func findListedAgent(entries []config.AgentEntry, name string) (config.AgentEntry, bool) {
	p := config.Project{Agents: entries}
	e, _, ok := project.FindAgentEntry(&p, name)
	return e, ok
}

func projectAgentsRemove(args []string) {
	fs := flag.NewFlagSet("project agents remove", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	name := fs.String("name", "", "agent name (required)")
	asJSON := fs.Bool("json", false, "print the removed entry's project id and name as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}
	if *name == "" {
		exitError("--name is required")
	}
	raw := adminCall("relay project agents remove", "project.agents.remove", agentRemoveRequest{ID: *id, Name: *name})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	fmt.Printf("removed agent %s\n", *name)
}
