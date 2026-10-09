// Package state holds the in-memory model built from the world and creates the
// project and host folders it names.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// Model is the mutable copy of the world. Project paths and host roots are
// resolved: an empty path or root has become a folder under the config dir.
type Model struct {
	Projects       []world.Project
	Hosts          []world.Host
	MCPs           []world.MCP
	Models         []world.Model
	Templates      []world.Template
	DefaultProject *world.DefaultProject
	ChiefOfStaff   *world.ChiefOfStaff
	Eve            world.Eve
}

// Store guards the model. Read and Write callbacks must not call back into the
// store.
type Store struct {
	mu   sync.RWMutex
	m    *Model
	subs []func(old, new *world.Project)
}

// New builds the model, creates every project and host folder and seeds files
// and git repos.
func New(w *world.World, dir string) (*Store, error) {
	m := &Model{
		Projects:       append([]world.Project(nil), w.Projects...),
		Hosts:          append([]world.Host(nil), w.Hosts...),
		MCPs:           append([]world.MCP(nil), w.MCPs...),
		Models:         append([]world.Model(nil), w.Models...),
		Templates:      append([]world.Template(nil), w.Templates...),
		DefaultProject: w.DefaultProject,
		ChiefOfStaff:   w.ChiefOfStaff,
		Eve:            w.Eve,
	}
	for i := range m.Hosts {
		if m.Hosts[i].Root == "" {
			m.Hosts[i].Root = filepath.Join(dir, "hosts", m.Hosts[i].ID)
		}
		if err := os.MkdirAll(m.Hosts[i].Root, 0o755); err != nil {
			return nil, fmt.Errorf("host %s: %w", m.Hosts[i].ID, err)
		}
	}
	for i := range m.Projects {
		if m.Projects[i].Path == "" {
			m.Projects[i].Path = filepath.Join(dir, "projects", m.Projects[i].ID)
		}
	}
	for _, sub := range []string{"home", "trash"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	for _, p := range m.Projects {
		if err := seed(m, dir, p); err != nil {
			return nil, fmt.Errorf("project %s: %w", p.ID, err)
		}
	}
	return &Store{m: m}, nil
}

// RootOf is the folder a project's files live in: its path on the console, or
// the path under the host's root for a host project.
func RootOf(m *Model, p world.Project) string {
	if p.HostID == "" {
		return p.Path
	}
	for _, h := range m.Hosts {
		if h.ID == p.HostID {
			return filepath.Join(h.Root, p.Path)
		}
	}
	return filepath.Join(os.TempDir(), "fakerelay-missing-host", p.HostID, p.Path)
}

func (s *Store) Read(fn func(*Model)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.m)
}

// Write runs fn on a copy and keeps the copy only when fn returns nil. Project
// change callbacks run after the lock is released.
func (s *Store) Write(fn func(*Model) error) error {
	s.mu.Lock()
	next, err := clone(s.m)
	if err == nil {
		err = fn(next)
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.m
	s.m = next
	subs := s.subs
	s.mu.Unlock()
	for _, c := range diff(old.Projects, next.Projects) {
		for _, fn := range subs {
			fn(c.old, c.new)
		}
	}
	return nil
}

// OnProjectChange registers fn for every project created, changed or deleted.
// new is nil on delete.
func (s *Store) OnProjectChange(fn func(old, new *world.Project)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

type change struct{ old, new *world.Project }

func diff(before, after []world.Project) []change {
	var out []change
	seen := map[string]bool{}
	for i := range before {
		seen[before[i].ID] = true
		var n *world.Project
		for j := range after {
			if after[j].ID == before[i].ID {
				n = &after[j]
			}
		}
		if n == nil || !reflect.DeepEqual(before[i], *n) {
			out = append(out, change{&before[i], n})
		}
	}
	for j := range after {
		if !seen[after[j].ID] {
			out = append(out, change{nil, &after[j]})
		}
	}
	return out
}

func clone(m *Model) (*Model, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var c Model
	return &c, json.Unmarshal(b, &c)
}
