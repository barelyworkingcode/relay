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

// `relay grant` — the operator-side answer to "what did I actually grant?"
// (issue #41).
//
// It exists because the operator guide's FIRST verification step used to be
// `relayremote list --schema`, which is a CLIENT-side tool reading a
// CLIENT-facing string. Since `disclose: "count"` (issue #33) that string says
// "confined to 1 value" for a grant of /Users/me/project and, byte for byte,
// for a grant of "/". An operator following the documented procedure could not
// tell a correct grant from a catastrophic one, and the check could not fail.
//
// The asymmetry that makes this a command rather than a change to that one is
// the whole of issue #41: `disclose` governs what reaches the CLIENT, and it
// should never have governed what relay shows the person who typed the grant.
// The client is correctly told only that it is confined and to how many roots.
// The operator is entitled to the coordinates — it is their machine and their
// grant — so this prints them, in full, always, whatever any field's
// `disclose` says.
//
// It reads settings.json directly, exactly as `relay audit` does and for the
// same reason: the question "what does this profile grant" must be answerable
// with the tray stopped, and a check an operator cannot run during an incident
// is not a check. The cost is that it describes the grant AS AUTHORED and
// cannot ask a live MCP whether it still declares these fields — so it says so
// once, in its own footer, rather than letting a reader mistake a stored value
// for an enforced one (that question is issue #42's, and relay now refuses the
// call outright rather than dropping the value).
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

// selectGrantRecords resolves --project against BOTH the id and the name,
// because an operator reading the Settings list has the name in front of them
// and an operator reading an audit line has the id. An empty selector means
// every record, which is the form to run when the question is "is anything on
// this machine granted more than I think".
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

// grantMcpView is one MCP's line of a grant: the four allowlists ADR-011
// names, with the resource scope shown as the values themselves.
type grantMcpView struct {
	Mcp      string            `json:"mcp"`
	Access   string            `json:"access"`
	Outbound string            `json:"outbound"`
	Tools    string            `json:"tools"`
	Scope    map[string]string `json:"scope,omitempty"`
	// Warnings names each scope value that reaches further than a folder, in
	// the same words every other surface uses (scopeBreadthWarnings).
	Warnings []string `json:"warnings,omitempty"`
}

// grantView is one project or access profile as an operator reads it.
type grantView struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Path string         `json:"path,omitempty"`
	Mcps []grantMcpView `json:"mcps"`
}

func newGrantView(s *Settings, p Project) grantView {
	// The token view is built rather than the Project fields read directly, so
	// this command resolves the two asymmetric defaults (ADR-011 decision 2)
	// through the SAME code the router does. A CLI that re-derived "absent
	// access means read for a profile and write for a project" would be a
	// second copy of the rule, free to disagree the day it changes — and this
	// command exists to be believed.
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
	return out
}

// grantedMcpIDs expands the wildcard the way SyncProjectToken does — to every
// MCP relay knows about — because that is what the grant actually reaches. A
// summary that printed "*" would be hiding the number the operator needs.
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

// grantToolText mirrors StoredToken.ToolAllowed's asymmetric default in words:
// an access profile holds only what allowed_tools enumerates (absent means
// NOTHING), a local project holds everything minus its denylist.
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
			continue
		}
		for _, m := range v.Mcps {
			fmt.Fprintf(w, "  %-14s access=%-5s  outbound=%-7s  tools=%s\n", m.Mcp, m.Access, m.Outbound, m.Tools)
			for _, name := range sortedKeys(m.Scope) {
				fmt.Fprintf(w, "  %-14s scope: %s = %s\n", "", name, m.Scope[name])
			}
			if len(m.Scope) == 0 {
				fmt.Fprintf(w, "  %-14s scope: (none set)\n", "")
			}
			// The warning is a separate line and is spelled out rather than
			// abbreviated, because the failure this command exists to catch is
			// an operator's eye sliding over a single character (issue #41: a
			// profile card rendered "/" inline and nobody saw it).
			for _, warning := range m.Warnings {
				fmt.Fprintf(w, "  %-14s ** %s **\n", "", strings.ToUpper(warning))
				warned = true
			}
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
