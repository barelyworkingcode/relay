package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/barelyworkingcode/relay/internal/project"
)

// Reads the log file directly rather than going over the bridge: appending
// JSONL and reading it are independent, so a reader never needs the writer's
// cooperation, and this keeps `relay audit` working when the tray is
// stopped, which is exactly when you'd reach for it.
func runAuditCommand(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	tail := fs.Int("tail", 50, "show the most recent N events")
	// A remote actor's project_id names an ACCESS PROFILE (ADR-011 decision 1);
	// it is the same field and the same ids, so one flag serves both.
	projectID := fs.String("project", "", "filter by project / access profile id")
	mcpID := fs.String("mcp", "", "filter by MCP id")
	outcome := fs.String("outcome", "", "filter by outcome: ok, error, tool_error, denied, unauthorized, throttled, pending. "+
		"'scope_violation' is also accepted here even though it is a FIELD, not an outcome (ADR-011 decision 7) — "+
		"it selects tool_error records the MCP marked as a resource-scope refusal")
	kind := fs.String("kind", "", "filter by actor kind: project, service, remote, relay, control, operator, unknown")
	event := fs.String("event", "", "filter by event kind: call_tool, list_tools, list_skills, mcp_down, mcp_up, control_decision, credential_issued, credential_revoked")
	text := fs.String("grep", "", "substring match over tool, MCP, error, project / access profile, caller, args, "+
		"and an issuance record's credential kind, identifier, name and grants")
	asJSON := fs.Bool("json", false, "emit raw JSONL instead of a table")
	pathOnly := fs.Bool("path", false, "print the log file path and exit")
	// Off by default so the table's shape — one line per call, the same eight
	// columns — never changes under a script that already parses it.
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
		ProjectID: *projectID,
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

// Renders oldest-first so the table reads top-to-bottom in time order, like
// the --json export. Factored out of runAuditCommand so the rendering can be
// exercised without capturing os.Stdout.
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
			// Seven leading (empty) cells so this line stays inside the same
			// tabwriter block as the row above it, landing under DETAIL
			// instead of resetting column widths for every row that follows.
			fmt.Fprintf(w, "\t\t\t\t\t\t\tauthority: %s\n", line)
		}
	}
}

// The parent is listed first because it's the agent that asked; the process
// is often just a short-lived `relay mcp` child.
//
// A remote caller has no process to name, so it is labelled by the enrolled
// client instead — otherwise every row of `relay audit --kind remote` would
// show a dash in the column that is supposed to say who called.
func auditCallerLabel(a AuditActor) string {
	if a.ClientID != "" {
		return a.ClientID
	}
	if a.CredID != "" {
		return a.CredID
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

// A scope_violation marker goes ahead of the error/args summary because it is
// the one signal on this record a reviewer must not have to expand the row to
// see (docs/access-profiles.md's "Checking that it worked"). tool_error alone
// does not get this treatment: ev.Error is typically empty for a tool_error
// (the MCP's reason lives in the result content, not this field), so without
// the marker a scope-violating row and an ordinary one render identically.
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
	// the transition itself is the detail.
	if ev.Supervision != "" {
		if ev.Error == "" {
			return ev.Supervision
		}
		return ev.Supervision + ": " + collapseWhitespace(ev.Error)
	}
	// An issuance row names no MCP or tool either, and unlike a
	// control_decision it names no route: what was issued, to what identifier,
	// with what grant, and through which door IS the whole record.
	if ev.Credential != "" {
		return auditIssuanceDetail(ev)
	}
	// A control_decision row (ADR-015) names no MCP or tool, so the
	// method/path/class/transport it carries instead is the detail — every
	// other kind of event leaves Method and Path empty.
	if ev.Method != "" || ev.Path != "" {
		detail := fmt.Sprintf("%s %s  class=%s  transport=%s", ev.Method, ev.Path, ev.Class, ev.Transport)
		if ev.Error != "" {
			detail += "  " + collapseWhitespace(ev.Error)
		}
		return detail
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

// auditIssuanceDetail renders a credential_issued / credential_revoked row.
// Nothing it prints comes from a field that could hold a secret: Credential,
// Subject, SubjectName, Grants and Via are the only ones it reads, and
// CredentialIssuance has no plaintext, hash or key material to put in any of
// them.
func auditIssuanceDetail(ev AuditEvent) string {
	parts := []string{ev.Credential}
	if ev.Subject != "" {
		parts = append(parts, ev.Subject)
	}
	if ev.SubjectName != "" {
		parts = append(parts, fmt.Sprintf("(%s)", collapseWhitespace(ev.SubjectName)))
	}
	if len(ev.Grants) > 0 {
		parts = append(parts, "grants="+strings.Join(ev.Grants, ","))
	}
	if ev.Via != "" {
		parts = append(parts, "via="+ev.Via)
	}
	if ev.IssuanceTruncated {
		parts = append(parts, "(truncated)")
	}
	return strings.Join(parts, "  ")
}

// ok is false when nothing was recorded for this record at all — a service
// token, a list event, or a refusal before an MCP was even resolved — in
// which case the line is omitted rather than printed full of placeholders.
// Access is the field that says whether anything was recorded: it and
// Scope/AllowExternal are always set together by setAuthority.
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
	// Appended rather than substituted: a warning is the second sentence,
	// not a replacement for the first.
	if len(ev.ScopeUnplaced) > 0 {
		parts = append(parts, "SCOPE NOT APPLIED: this grant sets "+
			project.QuoteNames(ev.ScopeUnplaced)+", which this MCP does not declare — call denied")
	}
	if warnings := project.ScopeBreadthWarnings(ev.Scope); len(warnings) > 0 {
		parts = append(parts, "SCOPE BREADTH: "+strings.Join(warnings, "; "))
	}
	return strings.Join(parts, "  "), true
}

// nil and an empty, non-nil map are different facts: nil means this MCP
// declares no `scope: "restrict"` field at all, so there was nothing to
// inject; an empty map means it does declare one and this call's grant
// supplied no value for it — on a `denied` record that is the finding itself
// (ADR-011 decision 4's third defence).
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
