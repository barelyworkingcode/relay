package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/barelyworkingcode/relay/internal/project"
)

func init() {
	// Registered here rather than in adminOps' literal so the verb owns its
	// own wiring; the table is still the only way a CLI reaches the tray.
	adminOps["project.update"] = adminProjectUpdate
}

// runProjectCommand is `relay project`'s dispatcher.
func runProjectCommand(args []string) {
	runSubcommands("project", []cliSubcommand{
		{"update", projectUpdate},
	}, args)
}

// projectUpdateRequest is project.update's admin_op payload. A nil
// FilesReadOnly is "not in the request", as in PUT /api/projects/{id}.
type projectUpdateRequest struct {
	ID            string `json:"id"`
	FilesReadOnly *bool  `json:"files_read_only,omitempty"`
}

type projectUpdateResult struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	FilesReadOnly bool   `json:"files_read_only"`
}

// projectUpdate brokers project.update over admin_op: this process holds no
// store, so it asks the running tray, whose ProjectOps is the same core
// PUT /api/projects/{id} calls.
func projectUpdate(args []string) {
	fs := flag.NewFlagSet("project update", flag.ExitOnError)
	id := fs.String("id", "", "project id (required)")
	readOnly := fs.String("files-read-only", "", "true refuses file writes, renames, moves, deletes and mkdir for the project; false clears it")
	fs.Parse(args)

	if *id == "" {
		exitError("--id is required")
	}
	req := projectUpdateRequest{ID: *id}
	switch *readOnly {
	case "":
		exitError("nothing to update: pass --files-read-only=true|false")
	default:
		v, err := strconv.ParseBool(*readOnly)
		if err != nil {
			exitError("--files-read-only must be true or false, got %q", *readOnly)
		}
		req.FilesReadOnly = &v
	}

	res := adminRead[projectUpdateResult]("relay project update", "project.update", req)
	if res.FilesReadOnly {
		fmt.Printf("project %s (%s): file changes are refused (files_read_only)\n", res.Name, res.ID)
	} else {
		fmt.Printf("project %s (%s): file changes are allowed\n", res.Name, res.ID)
	}
}

// adminProjectUpdate calls the same ProjectOps.Update as PUT
// /api/projects/{id}. files_read_only only narrows what a project may do, so
// it touches no grant and raises no presence prompt.
func adminProjectUpdate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[projectUpdateRequest]("project.update", args)
	if err != nil {
		return nil, err
	}
	if r.projectOps == nil {
		return nil, errors.New("project operations are not available in this relay process")
	}
	if req.FilesReadOnly == nil {
		return nil, errors.New("project.update: nothing to update")
	}
	updated, found, err := r.projectOps.Update(ctx, req.ID, project.UpdateFields{FilesReadOnly: req.FilesReadOnly},
		func() project.McpSurfaces { return nil }, auditViaIPC, "")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("project not found")
	}
	return marshalAdminResult(projectUpdateResult{ID: updated.ID, Name: updated.Name, FilesReadOnly: updated.FilesReadOnly})
}
