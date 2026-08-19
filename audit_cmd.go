package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// runAuditCommand implements `relay audit` — a read-only tail of the tool-call
// log for the case where the tray isn't open, or you want to pipe events into
// something else.
//
// It reads the log file directly rather than going over the bridge. The log is
// owned by the tray process, but appending JSONL and reading it are independent;
// a reader never needs the writer's cooperation, and this keeps `relay audit`
// working when the tray is stopped, which is exactly when you'd reach for it.
func runAuditCommand(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	tail := fs.Int("tail", 50, "show the most recent N events")
	project := fs.String("project", "", "filter by project id")
	mcpID := fs.String("mcp", "", "filter by MCP id")
	outcome := fs.String("outcome", "", "filter by outcome: ok, error, denied, unauthorized")
	event := fs.String("event", "", "filter by event kind: call_tool, list_tools, list_skills")
	text := fs.String("grep", "", "substring match over tool, MCP, error, project, caller, args")
	asJSON := fs.Bool("json", false, "emit raw JSONL instead of a table")
	pathOnly := fs.Bool("path", false, "print the log file path and exit")
	fs.Parse(args)

	path, err := auditLogPath()
	if err != nil {
		exitError("audit: cannot resolve log path: %v", err)
	}
	if *pathOnly {
		fmt.Println(path)
		return
	}
	if _, err := os.Stat(path); err != nil {
		exitError("audit: no log at %s (auditing may be disabled, or relay has not run yet)", path)
	}

	q := AuditQuery{
		ProjectID: *project,
		McpID:     *mcpID,
		Outcome:   *outcome,
		Event:     *event,
		Text:      *text,
		Limit:     *tail,
	}

	events := readAuditTail(path, auditTailBudget)
	matched := make([]AuditEvent, 0, *tail)
	for i := range events {
		if q.matches(&events[i]) {
			matched = append(matched, events[i])
			if len(matched) >= *tail {
				break
			}
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		// Oldest-first so piping into another tool reads forwards in time.
		for i := len(matched) - 1; i >= 0; i-- {
			if err := enc.Encode(matched[i]); err != nil {
				exitError("audit: %v", err)
			}
		}
		return
	}

	if len(matched) == 0 {
		fmt.Println("no matching tool calls")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "TIME\tOUTCOME\tPROJECT\tMCP\tTOOL\tMS\tCALLER\tDETAIL")
	for i := len(matched) - 1; i >= 0; i-- {
		ev := matched[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			ev.TS.Local().Format("15:04:05"),
			ev.Outcome,
			dash(ev.Actor.ProjectName),
			dash(ev.McpID),
			dash(ev.Tool),
			ev.DurMs,
			dash(auditCallerLabel(ev.Actor)),
			auditDetail(ev),
		)
	}
	w.Flush()
}

// auditCallerLabel renders the actor as "parent→proc", falling back to whatever
// half is known. The parent is listed first because it's the agent that asked;
// the process is often just a short-lived `relay mcp` child.
func auditCallerLabel(a AuditActor) string {
	switch {
	case a.Parent != "" && a.Proc != "":
		return a.Parent + "→" + a.Proc
	case a.Proc != "":
		return a.Proc
	case a.Parent != "":
		return a.Parent
	default:
		return ""
	}
}

// auditDetail is the one-line summary: the error for a failure, the redacted
// args for a success.
func auditDetail(ev AuditEvent) string {
	if ev.Error != "" {
		return collapseWhitespace(ev.Error)
	}
	if len(ev.Args) > 0 {
		return collapseWhitespace(truncateRunes(string(ev.Args), 120))
	}
	if ev.ToolCount > 0 {
		return fmt.Sprintf("%d tools visible", ev.ToolCount)
	}
	return ""
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
