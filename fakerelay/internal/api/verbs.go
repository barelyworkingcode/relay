package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func (a *api) verbs(r server.Registrar) {
	r.Verb("grant", a.grantVerb)
	r.Verb("project update", a.projectUpdateVerb)
	r.Verb("eve enrol", a.eveEnrolVerb)
	r.Verb("eve list", a.eveListVerb)
	r.Verb("eve revoke", a.eveRevokeVerb)
	r.Verb("mcp list", a.mcpListVerb)
}

func fail(code int, format string, args ...any) server.VerbResult {
	return server.VerbResult{Code: code, Stderr: []byte("error: " + fmt.Sprintf(format, args...) + "\n")}
}

func out(format string, args ...any) server.VerbResult {
	return server.VerbResult{Stdout: []byte(fmt.Sprintf(format, args...))}
}

type grantMCP struct {
	MCP      string `json:"mcp"`
	Access   string `json:"access"`
	Outbound string `json:"outbound"`
	Tools    string `json:"tools"`
}

type grantRecord struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	Kind string     `json:"kind"`
	Path string     `json:"path,omitempty"`
	MCPs []grantMCP `json:"mcps"`
}

func (a *api) grantVerb(ctx context.Context, args []string) server.VerbResult {
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	project := fs.String("project", "", "")
	asJSON := fs.Bool("json", false, "")
	if res, parsed := server.ParseVerbFlags(fs, args); !parsed {
		return res
	}
	var recs []grantRecord
	a.State.Read(func(m *state.Model) {
		for _, p := range m.Projects {
			if *project != "" && p.ID != *project && p.Name != *project {
				continue
			}
			kind := "project"
			if p.Kind == "remote" {
				kind = "access profile"
			}
			seen := map[string]bool{}
			for _, id := range p.AllowedMCPIDs {
				ids := []string{id}
				if id == "*" {
					ids = nil
					for _, mc := range m.MCPs {
						ids = append(ids, mc.ID)
					}
				}
				for _, x := range ids {
					seen[x] = true
				}
			}
			rec := grantRecord{ID: p.ID, Name: p.Name, Kind: kind, Path: p.Path, MCPs: []grantMCP{}}
			for x := range seen {
				rec.MCPs = append(rec.MCPs, grantMCP{MCP: x, Access: "read", Outbound: "blocked", Tools: "all tools"})
			}
			sort.Slice(rec.MCPs, func(i, j int) bool { return rec.MCPs[i].MCP < rec.MCPs[j].MCP })
			recs = append(recs, rec)
		}
	})
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	ev := a.Events.Begin(ctx, "grant.view").Set("count", len(recs))
	switch {
	case len(recs) == 0 && *project != "":
		ev.End("error", "not_found", errors.New("no match"))
		return fail(1, "no project or access profile matching %q", *project)
	case len(recs) == 0:
		ev.End("ok", "", nil)
		return out("no projects or access profiles\n")
	}
	ev.End("ok", "", nil)
	if *asJSON {
		b, _ := json.MarshalIndent(recs, "", "  ")
		return out("%s\n", b)
	}
	var buf bytes.Buffer
	for _, r := range recs {
		label := "PROJECT"
		if r.Kind != "project" {
			label = "ACCESS PROFILE"
		}
		fmt.Fprintf(&buf, "%s  %s  (id: %s)\n", label, r.Name, r.ID)
		for _, g := range r.MCPs {
			fmt.Fprintf(&buf, "  %-12s access=%s  outbound=%s  tools=%s\n", g.MCP, g.Access, g.Outbound, g.Tools)
		}
	}
	return server.VerbResult{Stdout: buf.Bytes()}
}

func (a *api) projectUpdateVerb(ctx context.Context, args []string) server.VerbResult {
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	id := fs.String("id", "", "")
	ro := fs.String("files-read-only", "", "")
	if res, parsed := server.ParseVerbFlags(fs, args); !parsed {
		return res
	}
	ev := a.Events.Begin(ctx, "project.update").Set("project_id", *id).Set("gated", false)
	if *id == "" || (*ro != "true" && *ro != "false") {
		ev.End("error", "invalid", errors.New("usage"))
		return fail(2, "usage: project update --id ID --files-read-only=true|false")
	}
	var name string
	found := false
	_ = a.State.Write(func(m *state.Model) error {
		for i := range m.Projects {
			if m.Projects[i].ID == *id {
				m.Projects[i].FilesReadOnly = *ro == "true"
				name, found = m.Projects[i].Name, true
			}
		}
		return nil
	})
	if !found {
		ev.End("error", "not_found", errors.New("project not found"))
		return fail(1, "project not found: %s", *id)
	}
	ev.End("ok", "", nil)
	if *ro == "true" {
		return out("project %s (%s): file changes are refused (files_read_only)\n", name, *id)
	}
	return out("project %s (%s): file changes are allowed\n", name, *id)
}

func (a *api) eveEnrolVerb(ctx context.Context, args []string) server.VerbResult {
	if res, parsed := server.ParseVerbFlags(flag.NewFlagSet("eve enrol", flag.ContinueOnError), args); !parsed {
		return res
	}
	ev := a.Events.Begin(ctx, "eve.enrolment.open")
	if err := a.Presence.Require(ctx, "eve.enrolment.open"); err != nil {
		reason := server.PresenceReason(err)
		ev.End("denied", reason, err)
		return fail(1, "%v", err)
	}
	exp := a.now().Add(eveWindow)
	_ = a.State.Write(func(m *state.Model) error { m.Eve.WindowExpires = rfc3339(exp); m.Eve.EnrolmentOpen = true; return nil })
	ev.End("ok", "", nil)
	return out("eve passkey enrolment open until %s (%s, single use)\n  on the new browser, open Eve, and tap \"Add this browser\"\n",
		exp.Local().Format("15:04:05"), eveWindow)
}

func (a *api) eveListVerb(ctx context.Context, args []string) server.VerbResult {
	if res, parsed := server.ParseVerbFlags(flag.NewFlagSet("eve list", flag.ContinueOnError), args); !parsed {
		return res
	}
	var keys []passkey
	var pending []string
	a.State.Read(func(m *state.Model) { keys, pending = passkeys(m), revocations(m) })
	a.Events.Begin(ctx, "eve.list").Set("count", len(keys)).End("ok", "", nil)
	if len(keys) == 0 {
		return out("no eve passkeys reported\n")
	}
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tCREDENTIAL ID\tCREATED\tLAST USED\tSTATUS")
	for _, k := range keys {
		status := "-"
		for _, p := range pending {
			if p == k.ID {
				status = "revocation pending"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.Label, abbrev(k.ID), k.Created, k.LastUsed, status)
	}
	_ = tw.Flush()
	return server.VerbResult{Stdout: buf.Bytes()}
}

func (a *api) eveRevokeVerb(ctx context.Context, args []string) server.VerbResult {
	fs := flag.NewFlagSet("eve revoke", flag.ContinueOnError)
	id := fs.String("id", "", "")
	if res, parsed := server.ParseVerbFlags(fs, args); !parsed {
		return res
	}
	ev := a.Events.Begin(ctx, "eve.passkey.revoke").Set("passkey_id", *id)
	refuse := func(reason, msg string) server.VerbResult {
		ev.End("error", reason, errors.New(msg))
		if reason == "invalid" {
			return fail(1, "%s", msg)
		}
		// The service refuses over the bridge, and the CLI prints that error as
		// the bridge client words it.
		return fail(1, "bridge error (code -32603): %s", msg)
	}
	if *id == "" {
		return refuse("invalid", "--id is required")
	}
	var known bool
	var already bool
	var live int
	a.State.Read(func(m *state.Model) {
		pend := map[string]bool{}
		for _, p := range revocations(m) {
			pend[p] = true
		}
		already = pend[*id]
		for _, k := range passkeys(m) {
			known = known || k.ID == *id
			if !pend[k.ID] {
				live++
			}
		}
	})
	switch {
	case !known:
		return refuse("not_found", fmt.Sprintf("no Eve passkey with id %q", *id))
	case already:
		return refuse("conflict", "a revocation for that passkey is already pending")
	case live <= 1:
		return refuse("conflict", "the last Eve passkey cannot be revoked from relay; enrol another browser first, or delete eve's auth.json on the console to start over")
	}
	if err := a.Presence.Require(server.WithSubject(ctx, *id), "eve.passkey.revoke"); err != nil {
		ev.End("denied", server.PresenceReason(err), err)
		return fail(1, "%v", err)
	}
	_ = a.State.Write(func(m *state.Model) error {
		b, _ := json.Marshal(map[string]string{"id": *id, "requested": rfc3339(a.now())})
		m.Eve.Revocations = append(m.Eve.Revocations, b)
		return nil
	})
	ev.End("ok", "", nil)
	return out("eve passkey %s: revocation pending\n  it will stop working on its next use\n  eve signs out every session that passkey minted when it applies the revocation\n", abbrev(*id))
}

func (a *api) mcpListVerb(ctx context.Context, args []string) server.VerbResult {
	if res, parsed := server.ParseVerbFlags(flag.NewFlagSet("mcp list", flag.ContinueOnError), args); !parsed {
		return res
	}
	list := a.mcps()
	a.Events.Begin(ctx, "mcp.list").Set("count", len(list)).End("ok", "", nil)
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTRANSPORT\tENDPOINT")
	for _, m := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.Name, m.Transport, endpoint(m))
	}
	_ = tw.Flush()
	return server.VerbResult{Stdout: buf.Bytes()}
}

// endpoint is the ENDPOINT column: a stdio MCP's command with its arguments,
// an HTTP MCP's URL.
func endpoint(m world.MCP) string {
	switch {
	case m.Transport == "http" && m.URL != "":
		return m.URL
	case m.Command != "":
		return strings.Join(append([]string{m.Command}, m.Args...), " ")
	}
	return "-"
}
