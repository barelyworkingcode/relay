package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

// The project verbs are the CLI door to ProjectOps, the core the Projects tab
// and the /api/projects routes call. The gate, the audit record and the event
// live in the core; nothing here decides them.

var (
	errProjectOpsUnavailable  = errors.New("project operations are not available in this relay process")
	errProjectNotFound        = errors.New("project not found")
	errMcpSurfacesUnavailable = errors.New("MCP surfaces are not available in this relay process")
)

const projectIDRequired = "--id is required"

func requireProjectOps(r *appRouter) (*ProjectOps, error) {
	if r.projectOps == nil {
		return nil, errProjectOpsUnavailable
	}
	return r.projectOps, nil
}

type projectIDRequest struct {
	ID string `json:"id"`
}

type projectEditRequest struct {
	ID     string               `json:"id"`
	Fields project.UpdateFields `json:"fields"`
}

type projectRemoveResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type projectTokenResult struct {
	Token string `json:"token"`
}

type projectSkillResult struct {
	Path string `json:"path"`
}

// projectViewResult is the slice of projectView the text forms print.
type projectViewResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func adminProjectCreate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	if r.mcpSurfaces == nil || r.store == nil {
		return nil, errMcpSurfacesUnavailable
	}
	body, err := decodeAdminArgs[project.CreateFields]("project.create", args)
	if err != nil {
		return nil, err
	}
	created, err := createProjectFromDoor(ctx, ops, body, r.mcpSurfaces(), r, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(projectToView(config.DisplaySettings(r.store), created))
}

func adminProjectEdit(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	if r.mcpSurfaces == nil || r.store == nil {
		return nil, errMcpSurfacesUnavailable
	}
	req, err := decodeAdminArgs[projectEditRequest]("project.edit", args)
	if err != nil {
		return nil, err
	}
	updated, found, err := updateProjectFromDoor(ctx, ops, req.ID, req.Fields, r.mcpSurfaces, r, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProjectNotFound
	}
	return marshalAdminResult(projectToView(config.DisplaySettings(r.store), updated))
}

func adminProjectRemove(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[projectIDRequest]("project.remove", args)
	if err != nil {
		return nil, err
	}
	// The name is read before the project goes, so the answer can name what
	// was removed.
	var name string
	if proj, _ := config.FindProjectByID(config.FreshSettings(ops.Store), req.ID); proj != nil {
		name = proj.Name
	}
	found, err := ops.Remove(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProjectNotFound
	}
	return marshalAdminResult(projectRemoveResult{ID: req.ID, Name: name})
}

func adminProjectTokenRotate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[projectIDRequest]("project.token.rotate", args)
	if err != nil {
		return nil, err
	}
	token, found, err := ops.RotateToken(ctx, req.ID, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProjectNotFound
	}
	return marshalAdminResult(projectTokenResult{Token: token})
}

func adminProjectTokenReveal(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[projectIDRequest]("project.token.reveal", args)
	if err != nil {
		return nil, err
	}
	token, found, err := ops.RevealToken(ctx, req.ID, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProjectNotFound
	}
	return marshalAdminResult(projectTokenResult{Token: token})
}

func adminProjectSkillRegen(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireProjectOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[projectIDRequest]("project.skill.regen", args)
	if err != nil {
		return nil, err
	}
	dir, found, err := ops.RegenSkill(ctx, r, req.ID)
	if !found {
		return nil, errProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(projectSkillResult{Path: dir})
}

// projectCreate creates a project. --name and --path build the common body;
// --file takes the POST /api/projects body for everything else.
func projectCreate(args []string) {
	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	name := fs.String("name", "", "project name (with --path)")
	path := fs.String("path", "", "project folder (with --name); a relative path is resolved against the current directory")
	file := fs.String("file", "", "POST /api/projects body as a JSON file, or - for stdin (instead of --name and --path)")
	asJSON := fs.Bool("json", false, "print the created project as JSON")
	fs.Parse(args)

	var body any
	switch {
	case *file != "" && (*name != "" || *path != ""):
		exitError("--file cannot be combined with --name or --path")
	case *file != "":
		body = json.RawMessage(readBodyArg(*file))
	case *name == "" || *path == "":
		exitError("pass --name and --path, or --file")
	default:
		abs, err := filepath.Abs(*path)
		if err != nil {
			exitError("--path: %v", err)
		}
		body = struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}{*name, abs}
	}

	raw := adminCall("relay project create", "project.create", body)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var p projectViewResult
	decodeCLIResult(raw, &p)
	fmt.Printf("created project %s (%s)\n", p.Name, p.ID)
}

// projectEdit applies a PUT /api/projects/{id} body to a project. Widening the
// grant prompts; narrowing does not.
func projectEdit(args []string) {
	fs := flag.NewFlagSet("project edit", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	file := fs.String("file", "", "PUT /api/projects/{id} body as a JSON file, or - for stdin (required)")
	asJSON := fs.Bool("json", false, "print the updated project as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}
	if *file == "" {
		exitError("--file is required")
	}

	raw := adminCall("relay project edit", "project.edit", struct {
		ID     string          `json:"id"`
		Fields json.RawMessage `json:"fields"`
	}{*id, readBodyArg(*file)})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var p projectViewResult
	decodeCLIResult(raw, &p)
	fmt.Printf("updated project %s (%s)\n", p.Name, p.ID)
}

func projectRemove(args []string) {
	fs := flag.NewFlagSet("project remove", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	asJSON := fs.Bool("json", false, "print the removed project's id and name as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}

	raw := adminCall("relay project remove", "project.remove", projectIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res projectRemoveResult
	decodeCLIResult(raw, &res)
	fmt.Printf("removed project %s (%s)\n", res.Name, res.ID)
}

// projectTokenVerb runs the two verbs that print a project token: the token is
// the whole text output, so it can be captured with $(...).
func projectTokenVerb(name, op string, args []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	asJSON := fs.Bool("json", false, `print {"token": ...} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}

	raw := adminCall("relay "+name, op, projectIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res projectTokenResult
	decodeCLIResult(raw, &res)
	fmt.Println(res.Token)
}

func projectRotateToken(args []string) {
	projectTokenVerb("project rotate-token", "project.token.rotate", args)
}

func projectToken(args []string) {
	projectTokenVerb("project token", "project.token.reveal", args)
}

func projectRegenSkill(args []string) {
	fs := flag.NewFlagSet("project regen-skill", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	asJSON := fs.Bool("json", false, `print {"path": ...} as JSON`)
	fs.Parse(args)
	if *id == "" {
		exitError("%s", projectIDRequired)
	}

	raw := adminCall("relay project regen-skill", "project.skill.regen", projectIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res projectSkillResult
	decodeCLIResult(raw, &res)
	fmt.Printf("regenerated the skill in %s\n", res.Path)
}

// decodeCLIResult decodes an admin_op answer into v, or exits with the
// parse failure.
func decodeCLIResult(raw json.RawMessage, v any) {
	if err := json.Unmarshal(raw, v); err != nil {
		exitError("parse response: %v", err)
	}
}
