package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessionhistory"
)

type sessionHistoryRequest struct {
	ProjectID string `json:"project_id,omitempty"`
}

func adminSessionHistory(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req := sessionHistoryRequest{}
	if len(args) > 0 {
		var err error
		if req, err = decodeAdminArgs[sessionHistoryRequest]("session.history", args); err != nil {
			return nil, err
		}
	}
	ops := activeHistoryOps.Load()
	if ops == nil || ops.index == nil {
		return nil, errors.New("session.history: session history is not available in this relay process")
	}
	r.recordOperatorDecision(http.MethodGet, "/api/session-history")
	return marshalAdminResult(ops.List(req.ProjectID))
}

func sessionHistory(args []string) {
	fs := flag.NewFlagSet("session history", flag.ExitOnError)
	project := fs.String("project", "", "show only this project's sessions")
	asJSON := fs.Bool("json", false, "print the GET /api/session-history body as JSON")
	fs.Parse(args)
	raw := adminCall("relay session history", "session.history", sessionHistoryRequest{ProjectID: *project})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var resp sessionhistory.Response
	decodeCLIResult(raw, &resp)
	if !resp.Ready {
		fmt.Println("still scanning; titles may be missing")
	}
	tw := newTabWriter()
	fmt.Fprintln(tw, "TITLE\tLAST ACTIVE\tKIND\tSTATE\tRESUME\tID")
	for _, row := range resp.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafe(historyTitle(row)), historyTime(row.LastActiveAt),
			terminalSafe(row.Kind), terminalSafe(row.State), terminalSafe(historyResume(row)), terminalSafe(row.ID))
	}
	tw.Flush()
}

// historyTitle is the row's own title, else "Claude — <folder>" as the ended
// rows are named. Both come from transcript text and are escaped by the caller.
func historyTitle(row sessionhistory.Row) string {
	if row.Title != nil && *row.Title != "" {
		return *row.Title
	}
	name := "Claude"
	if row.Agent != nil && *row.Agent != "" && *row.Agent != "claude" {
		name = *row.Agent
	}
	if row.Directory == "" {
		return name
	}
	return name + " — " + filepath.Base(row.Directory)
}

func historyTime(at *string) string {
	if at == nil {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, *at)
	if err != nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func historyResume(row sessionhistory.Row) string {
	if row.ResumeReason != "" {
		return row.Resume + " (" + row.ResumeReason + ")"
	}
	return row.Resume
}
