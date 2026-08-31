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

// runGrantCommand prints a project's or access profile's grant as authored.
// `disclose` governs only what reaches the CLIENT; this always prints scope
// values in full regardless of any field's `disclose` setting. It reads
// settings.json directly, like `relay audit`, so the question is answerable
// with the tray stopped — and describes the grant as authored, not as a live
// MCP currently enforces it.
func runGrantCommand(args []string) {
	fs := flag.NewFlagSet("grant", flag.ExitOnError)
	projectID := fs.String("project", "", "show one record by id or name (default: every record)")
	asJSON := fs.Bool("json", false, "emit the grants as JSON instead of a table")
	fs.Parse(args)

	s := NewSettingsStore().Get()
	records := selectGrantRecords(s.Projects, *projectID)
	if len(records) == 0 {
		if *projectID != "" {
			exitError("no project or access profile matching %q", *projectID)
		}
		fmt.Println("no projects or access profiles")
		return
	}

	views := make([]grantView, 0, len(records))
	for _, p := range records {
		views = append(views, newGrantView(s, p))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(views); err != nil {
			exitError("%v", err)
		}
		return
	}
	printGrantViews(os.Stdout, views)
}

// selectGrantRecords resolves --project against both the id and the name:
// Settings shows the name, an audit line shows the id.
func selectGrantRecords(projects []Project, selector string) []Project {
	if selector == "" {
		out := make([]Project, len(projects))
		copy(out, projects)
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	for _, p := range projects {
		if p.ID == selector || p.Name == selector {
			return []Project{p}
		}
	}
	return nil
}

type grantMcpView struct {
	Mcp      string            `json:"mcp"`
	Access   string            `json:"access"`
	Outbound string            `json:"outbound"`
	Tools    string            `json:"tools"`
	Scope    map[string]string `json:"scope,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

// grantEnrolmentView is one enrolment reaching a profile, and whether it
// currently holds configuration authority over its own grant (ADR-018).
// CLIAdmin has no omitempty: AC-22 requires an enrolment with the bit off
// to show as off, never be silently dropped from the list the way an
// omitted false would read.
type grantEnrolmentView struct {
	ClientID string `json:"client_id"`
	CLIAdmin bool   `json:"cli_admin"`
}

type grantView struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Path string         `json:"path,omitempty"`
	Mcps []grantMcpView `json:"mcps"`
	// Enrolments lists every enrolment whose ProjectIDs names this record,
	// so `relay grant` answers "who can reach this and can any of them
	// reconfigure it" in the same place it answers "what does this reach"
	// (ADR-018 decision 5). Empty for a local project, which no enrolment
	// can grant (ValidateEnrolmentGrants).
	Enrolments []grantEnrolmentView `json:"enrolments,omitempty"`
}

func newGrantView(s *Settings, p Project) grantView {
	// Built as a StoredToken and read through its methods, not the Project
	// fields directly, so the asymmetric defaults (ADR-011 decision 2) resolve
	// through the same code the router uses — a CLI that re-derived them would
	// be a second copy of the rule, free to disagree the day it changes.
	tok := &StoredToken{
		ProjectKind:   p.Kind,
		Access:        p.Access,
		AllowedTools:  p.AllowedTools,
		AllowExternal: p.AllowExternal,
		Context:       p.Context,
	}

	kind := "project"
	if p.IsRemote() {
		kind = "access profile"
	}
	out := grantView{ID: p.ID, Name: p.Name, Kind: kind, Path: p.Path}

	for _, mcpID := range grantedMcpIDs(s, p) {
		row := grantMcpView{
			Mcp:      mcpID,
			Access:   tok.AccessMode(mcpID),
			Outbound: "blocked",
			Tools:    grantToolText(tok, p, mcpID),
		}
		if tok.ExternalAllowed(mcpID) {
			row.Outbound = "allowed"
		}
		values := contextValues(p.Context[mcpID])
		if len(values) > 0 {
			row.Scope = make(map[string]string, len(values))
			for name, raw := range values {
				row.Scope[name] = strings.TrimSpace(string(raw))
			}
			row.Warnings = scopeBreadthWarnings(values)
		}
		out.Mcps = append(out.Mcps, row)
	}
	for _, e := range s.Enrolments {
		if e.GrantsProject(p.ID) {
			out.Enrolments = append(out.Enrolments, grantEnrolmentView{ClientID: e.ClientID, CLIAdmin: e.CLIAdmin})
		}
	}
	sort.Slice(out.Enrolments, func(i, j int) bool { return out.Enrolments[i].ClientID < out.Enrolments[j].ClientID })
	return out
}

// grantedMcpIDs expands the wildcard the way SyncProjectToken does, to every
// MCP relay knows about: that is what the grant actually reaches, and
// printing "*" would hide the number the operator needs.
func grantedMcpIDs(s *Settings, p Project) []string {
	ids := p.AllowedMcpIDs
	if isWildcard(ids) {
		ids = s.AllExternalMcpIDs()
	}
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Strings(out)
	return out
}

// grantToolText mirrors StoredToken.ToolAllowed's asymmetric default in
// words; keep the two in sync or this misdescribes what a call would do.
func grantToolText(tok *StoredToken, p Project, mcpID string) string {
	patterns := p.AllowedTools[mcpID]
	if len(patterns) > 0 {
		return strings.Join(patterns, ", ")
	}
	if tok.IsRemote() {
		return "no tools"
	}
	if n := len(p.DisabledTools[mcpID]); n > 0 {
		return fmt.Sprintf("all tools except %d", n)
	}
	return "all tools"
}

// printGrantEnrolments names every enrolment reaching an access profile and
// marks a cli-admin one loudly (AC-22): a certificate that can narrow this
// profile's own grant is a fact the operator reading `relay grant` needs on
// the same screen as the grant itself, not one they have to cross-reference
// against `relay enrol list` to find.
func printGrantEnrolments(w io.Writer, enrolments []grantEnrolmentView) {
	if len(enrolments) == 0 {
		fmt.Fprintln(w, "  no enrolments reach this profile")
		return
	}
	fmt.Fprintln(w, "  enrolments:")
	for _, e := range enrolments {
		state := "cli-admin: off"
		if e.CLIAdmin {
			state = "** CLI-ADMIN: ON — this certificate may narrow this profile's own grant **"
		}
		fmt.Fprintf(w, "    %-20s %s\n", e.ClientID, state)
	}
}

func printGrantViews(w io.Writer, views []grantView) {
	warned := false
	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(w)
		}
		header := fmt.Sprintf("%s  %s  (id: %s", strings.ToUpper(v.Kind), v.Name, v.ID)
		if v.Path != "" {
			header += ", path: " + v.Path
		}
		fmt.Fprintln(w, header+")")
		if len(v.Mcps) == 0 {
			fmt.Fprintf(w, "  no MCPs granted — this %s reaches nothing\n", v.Kind)
		} else {
			for _, m := range v.Mcps {
				fmt.Fprintf(w, "  %-14s access=%-5s  outbound=%-7s  tools=%s\n", m.Mcp, m.Access, m.Outbound, m.Tools)
				for _, name := range sortedKeys(m.Scope) {
					fmt.Fprintf(w, "  %-14s scope: %s = %s\n", "", name, m.Scope[name])
				}
				if len(m.Scope) == 0 {
					fmt.Fprintf(w, "  %-14s scope: (none set)\n", "")
				}
				// Spelled out, not abbreviated: an operator's eye can slide
				// over a single character like "/".
				for _, warning := range m.Warnings {
					fmt.Fprintf(w, "  %-14s ** %s **\n", "", strings.ToUpper(warning))
					warned = true
				}
			}
		}
		if v.Kind == "access profile" {
			printGrantEnrolments(w, v.Enrolments)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "This is the grant as stored. Whether each MCP still declares these scope")
	fmt.Fprintln(w, "fields is a live question — a value relay cannot place in an MCP's current")
	fmt.Fprintln(w, "schema is refused at call time and shown by `relay audit --authority`.")
	if warned {
		fmt.Fprintln(w, "A line in ** ** marks a scope that reaches further than a folder. fsMCP")
		fmt.Fprintln(w, "documents an allowed-dir of \"/\" as a deliberate opt-out that must be spelled")
		fmt.Fprintln(w, "out; if you did not mean to spell it out, narrow it before the next call.")
	}
}
