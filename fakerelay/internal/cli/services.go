package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/barelyworkingcode/relay/fakerelay/internal/launch"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

// RegisterServer installs the verbs that need the running instance's service
// manager: service list and service restart.
func RegisterServer(r server.Registrar, d server.Deps, m *launch.Manager) {
	r.Verb("service list", func(ctx context.Context, args []string) server.VerbResult {
		fs := flag.NewFlagSet("service list", flag.ContinueOnError)
		if res, ok := server.ParseVerbFlags(fs, args); !ok {
			return res
		}
		list := m.List()
		d.Events.Begin(ctx, "service.list").Set("count", len(list)).End("ok", "", nil)
		if len(list) == 0 {
			return server.VerbResult{Stdout: []byte("no services registered\n")}
		}
		var buf bytes.Buffer
		tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tCAPABILITIES\tSTATE")
		for _, s := range list {
			caps := strings.Join(s.Capabilities, ",")
			if caps == "" {
				caps = "none"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Name, caps, s.State)
		}
		tw.Flush()
		return server.VerbResult{Stdout: buf.Bytes()}
	})
	r.Verb("service restart", func(ctx context.Context, args []string) server.VerbResult {
		fs := flag.NewFlagSet("service restart", flag.ContinueOnError)
		id, name := fs.String("id", "", ""), fs.String("name", "", "")
		if res, ok := server.ParseVerbFlags(fs, args); !ok {
			return res
		}
		ev := d.Events.Begin(ctx, "service.restart")
		if *id == "" && *name == "" {
			ev.End("error", "invalid", fmt.Errorf("--id or --name is required"))
			return server.VerbResult{Code: 1, Stderr: []byte("error: --id or --name is required\n")}
		}
		rec, ok := m.Find(*id, *name)
		if !ok {
			ev.End("error", "not_found", fmt.Errorf("no service found"))
			return server.VerbResult{Code: 1, Stderr: []byte(fmt.Sprintf("error: no service found with %s\n", quoted(*id, *name)))}
		}
		ev.Set("service_id", rec.ID)
		if err := m.Start(rec.ID); err != nil {
			ev.End("error", "internal", err)
			return server.VerbResult{Code: 1, Stderr: []byte("error: " + err.Error() + "\n")}
		}
		ev.End("ok", "", nil)
		return server.VerbResult{Stdout: []byte(fmt.Sprintf("restarted %s\n", rec.Name))}
	})
}

func quoted(id, name string) string {
	if id != "" {
		return fmt.Sprintf("id %q", id)
	}
	return fmt.Sprintf("name %q", name)
}
