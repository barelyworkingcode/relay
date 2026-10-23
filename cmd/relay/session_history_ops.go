package main

import (
	"context"
	"sync"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessionhistory"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// historyOps is the one core behind GET /api/session-history and the
// session.history admin op.
type historyOps struct {
	index *sessionhistory.Index
	store config.SettingsStore

	mu sync.Mutex
	// hostRoots holds each host project's resolved folder from the last scan, so
	// a row's project match costs no round trip to the host.
	hostRoots map[string]string
}

func (o *historyOps) List(projectID string) sessionhistory.Response {
	return o.index.Rows(projectID)
}

func newHistoryOps(store config.SettingsStore, clock serverClock, deps sessionRouteDeps, inv *outsidesessions.Inventory) *historyOps {
	o := &historyOps{store: store, hostRoots: map[string]string{}}
	opts := sessionhistory.Options{
		Clock:   clock,
		Ledger:  deps.sessions,
		Readers: []sessionhistory.Reader{sessionhistory.NewClaude()},
		Machines: func() []sessionhistory.Machine {
			s := config.FreshSettings(store)
			machines := []sessionhistory.Machine{o.machine(nil, s.Projects)}
			for _, h := range s.Hosts {
				if h.Probe != nil && (sshhost.IsWindowsOS(h.Probe.OS) || h.Probe.OS == "Windows") {
					continue
				}
				for _, p := range s.Projects {
					if p.HostID == h.ID {
						host := h
						machines = append(machines, o.machine(&host, s.Projects))
						break
					}
				}
			}
			return machines
		},
		View: func() sessionhistory.View { return o.view(inv) },
	}
	if deps.ready() {
		opts.Managed = deps.host().ManagedClaudeIdentities
	}
	o.index = sessionhistory.New(opts)
	return o
}

// view takes one settings snapshot, resolves the console's project folders
// once, and answers every question of one Rows call or scan from them.
func (o *historyOps) view(inv *outsidesessions.Inventory) sessionhistory.View {
	s := config.FreshSettings(o.store)
	var consoleRoots []string
	hostOf := map[string]string{}
	for _, p := range s.Projects {
		hostOf[p.ID] = p.HostID
	}
	v := sessionhistory.View{
		Project: func(machineKey, directory string) *string {
			var h *config.Host
			roots := consoleRoots
			if machineKey != "console" {
				h = &config.Host{ID: machineKey}
				roots = o.resolved(h, s.Projects)
			} else if roots == nil {
				consoleRoots = o.resolved(nil, s.Projects)
				roots = consoleRoots
			}
			p := matchOutsideProject(s.Projects, roots, h, directory)
			if p == nil {
				return nil
			}
			id := p.ID
			return &id
		},
		RecordMachine: func(r ledger.Record) string {
			if r.Detached != nil && r.Detached.HostID != "" {
				return r.Detached.HostID
			}
			if host := hostOf[r.ProjectID]; r.ProjectID != "" && host != "" {
				return host
			}
			return "console"
		},
	}
	if inv != nil {
		v.Held = inv.Held
	}
	return v
}

// machine builds the console (h nil) or one host. A host reads only the folders
// of its projects; the resolved names come from the host at scan time.
func (o *historyOps) machine(h *config.Host, projects []config.Project) sessionhistory.Machine {
	m := sessionhistory.Machine{Key: "console", Name: "Console"}
	if h != nil {
		id := h.ID
		m = sessionhistory.Machine{Key: h.ID, Name: h.Name, HostID: &id, Prefixes: o.prefixes(h, projects)}
		m.Scope = func(ctx context.Context) []string {
			s := config.FreshSettings(o.store)
			current, err := freshOutsideHost(o.store, h)
			if err != nil || current == nil {
				return m.Prefixes
			}
			roots := resolveOutsideProjectPaths(ctx, current, s.Projects)
			o.mu.Lock()
			for j, p := range s.Projects {
				if roots[j] != "" {
					o.hostRoots[p.ID] = roots[j]
				}
			}
			o.mu.Unlock()
			return o.prefixes(current, s.Projects)
		}
	}
	m.Run = func(ctx context.Context, script string) ([]byte, error) {
		current, err := freshOutsideHost(o.store, h)
		if err != nil {
			return nil, err
		}
		return outsideRun(ctx, current, script)
	}
	return m
}

// prefixes lists the mangled folder names of one host's projects, raw and
// resolved, the way the ended scan scopes a host.
func (o *historyOps) prefixes(h *config.Host, projects []config.Project) []string {
	out := []string{}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range projects {
		if p.IsRemote() || p.HostID != hostKey(h) {
			continue
		}
		for _, root := range []string{p.Path, o.hostRoots[p.ID]} {
			if root != "" {
				out = append(out, outsidesessions.FolderName(root))
			}
		}
	}
	return out
}

// resolved parallels projects: the console resolves symbolic links locally, a
// host answers from the last scan's cache.
func (o *historyOps) resolved(h *config.Host, projects []config.Project) []string {
	if h == nil {
		return resolveOutsideProjectPaths(context.Background(), nil, projects)
	}
	out := make([]string, len(projects))
	o.mu.Lock()
	defer o.mu.Unlock()
	for j, p := range projects {
		out[j] = o.hostRoots[p.ID]
	}
	return out
}
