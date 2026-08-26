package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
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
	// A remote actor's project_id names an ACCESS PROFILE (ADR-011 decision 1);
	// it is the same field and the same ids, so one flag serves both.
	project := fs.String("project", "", "filter by project / access profile id")
	mcpID := fs.String("mcp", "", "filter by MCP id")
	outcome := fs.String("outcome", "", "filter by outcome: ok, error, tool_error, denied, unauthorized, throttled, pending. "+
		"'scope_violation' is also accepted here even though it is a FIELD, not an outcome (ADR-011 decision 7) — "+
		"it selects tool_error records the MCP marked as a resource-scope refusal")
	kind := fs.String("kind", "", "filter by actor kind: project, service, remote, relay, unknown")
	event := fs.String("event", "", "filter by event kind: call_tool, list_tools, list_skills, mcp_down, mcp_up")
	text := fs.String("grep", "", "substring match over tool, MCP, error, project / access profile, caller, args")
	asJSON := fs.Bool("json", false, "emit raw JSONL instead of a table")
	pathOnly := fs.Bool("path", false, "print the log file path and exit")
	// Off by default so the table's shape — one line per call, the same eight
	// columns — never changes under a script that already parses it; the
	// authority (ADR-011 decision 7) is real information nonetheless, so it is
	// one flag away rather than buried behind --json and a grep, which was the
	// gap this flag exists to close.
	authority := fs.Bool("authority", false, "print a second line per call showing the access mode, the outbound grant, and the injected scope")
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
		Kind:      *kind,
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
	writeAuditTable(w, matched, *authority)
	w.Flush()
}

// writeAuditTable renders matched events (newest-first, as returned by the
// query above) as the human-readable table, oldest-first so the table itself
// reads top-to-bottom in time order like the --json export does. Factored out
// of runAuditCommand so the rendering can be exercised without capturing
// os.Stdout.
func writeAuditTable(w io.Writer, matched []AuditEvent, authority bool) {
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
		if !authority {
			continue
		}
		if line, ok := auditAuthorityLine(ev); ok {
			// Seven leading (empty) cells, so this line stays inside the same
			// tabwriter block as the row above it and lands under DETAIL
			// instead of resetting column widths for every row that follows.
			// A script parsing the eight-column grid never has to account for
			// this either way — --authority is opt-in, and even when passed,
			// no real row ever has an empty OUTCOME cell to confuse it with.
			fmt.Fprintf(w, "\t\t\t\t\t\t\tauthority: %s\n", line)
		}
	}
}

// auditCallerLabel renders the actor as "parent→proc", falling back to whatever
// half is known. The parent is listed first because it's the agent that asked;
// the process is often just a short-lived `relay mcp` child.
//
// A remote caller has no process to name, so it is labelled by the enrolled
// client instead — otherwise every row of `relay audit --kind remote` would
// show a dash in the column that is supposed to say who called.
func auditCallerLabel(a AuditActor) string {
	if a.ClientID != "" {
		return a.ClientID
	}
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
// args for a success — with a scope_violation marker ahead of either, because
// that is the one signal on this record a reviewer must not have to expand the
// row to see (docs/access-profiles.md's "Checking that it worked"). It reads
// the same in --json (the field is right there) and in the table, and it is
// literally the field name and its value rather than an invented word, so
// `grep scope_violation` finds the same calls in both.
//
// tool_error alone does not get this treatment: a boundary probed and held is
// not the same finding as any other in-protocol refusal, and ev.Error is
// typically empty for a tool_error in the first place (the MCP's reason lives
// in the result content, not in this field), so without the marker a
// scope-violating row and an ordinary one can render identically.
func auditDetail(ev AuditEvent) string {
	detail := auditBaseDetail(ev)
	if !ev.ScopeViolation {
		return detail
	}
	if detail == "" {
		return "scope_violation: true"
	}
	return "scope_violation: true  " + detail
}

func auditBaseDetail(ev AuditEvent) string {
	// A supervision record (ADR-012) names no tool and carries no arguments:
	// the transition is the detail, and the reader error that caused it — when
	// there is one — is the rest of the same sentence.
	if ev.Supervision != "" {
		if ev.Error == "" {
			return ev.Supervision
		}
		return ev.Supervision + ": " + collapseWhitespace(ev.Error)
	}
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

// auditAuthorityLine renders the authority a call ran with (ADR-011 decision
// 7) for --authority: the access mode, the outbound grant, and the injected
// scope. ok is false when nothing was recorded for this record at all — a
// service token, a list event, or a refusal before an MCP was even resolved
// (an unknown tool name) — in which case the line is omitted rather than
// printed full of placeholders. Access is the field that says whether
// anything was recorded: it and Scope/AllowExternal are always set together
// by setAuthority.
func auditAuthorityLine(ev AuditEvent) (string, bool) {
	if ev.Access == "" {
		return "", false
	}
	parts := []string{"access=" + ev.Access}
	switch {
	case ev.AllowExternal == nil:
		parts = append(parts, "outbound=n/a")
	case *ev.AllowExternal:
		parts = append(parts, "outbound=allowed")
	default:
		parts = append(parts, "outbound=blocked")
	}
	parts = append(parts, "scope="+auditScopeSummary(ev.Scope))
	// root is a fact relay knows from its OWN spawn configuration, not a
	// scope the MCP declared -- kept a separate word so it can never be read
	// as filling in for "scope=(none declared)", which stays true and stays
	// meaningful for an MCP that really does publish no contextSchema.
	if ev.McpRoot != "" {
		parts = append(parts, "root="+ev.McpRoot)
	}
	// Two findings the summary above cannot carry, each appended rather than
	// substituted, because the coordinates stay on the line either way — the
	// operator is entitled to them (issue #41) and a warning is the second
	// sentence, not a replacement for the first.
	if len(ev.ScopeUnplaced) > 0 {
		parts = append(parts, "SCOPE NOT APPLIED: this grant sets "+
			quoteNames(ev.ScopeUnplaced)+", which this MCP does not declare — call denied")
	}
	if warnings := scopeBreadthWarnings(ev.Scope); len(warnings) > 0 {
		parts = append(parts, "SCOPE BREADTH: "+strings.Join(warnings, "; "))
	}
	return strings.Join(parts, "  "), true
}

// auditScopeSummary renders the injected scope for a human. nil and an empty,
// non-nil map are different facts and must read as different sentences: nil
// means this MCP declares no `scope: "restrict"` field at all, so there was
// nothing to inject; an empty map means it does declare one and this call's
// grant supplied no value for it — on a `denied` record that is the finding
// itself (ADR-011 decision 4's third defence), and collapsing the two would
// erase exactly the distinction the wire format was fixed to carry.
func auditScopeSummary(scope map[string]json.RawMessage) string {
	if scope == nil {
		return "(none declared)"
	}
	if len(scope) == 0 {
		return "(declared, none injected)"
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+string(scope[k]))
	}
	return strings.Join(parts, ",")
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
