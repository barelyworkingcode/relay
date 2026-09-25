package config

import (
	"errors"
	"fmt"
)

// ProjectMode labels which Home|Work mode a project belongs to. It is a
// label, never a grant: nothing that decides what a project may reach reads
// it (docs/architecture.md, "Mode and default projects").
type ProjectMode string

const (
	ProjectModeHome ProjectMode = "home"
	ProjectModeWork ProjectMode = "work"
	ProjectModeBoth ProjectMode = "both"
)

// DefaultModes are the modes that carry a default project, in the order
// every surface lists them.
var DefaultModes = []ProjectMode{ProjectModeHome, ProjectModeWork}

var (
	ErrInvalidProjectMode    = errors.New("invalid project mode")
	ErrInvalidDefaultProject = errors.New("invalid default project")
)

// DefaultProjects is the per-mode default project. An empty id means the
// mode has none; a nil *DefaultProjects on Settings means the operator has
// never configured modes, which is not the same thing.
type DefaultProjects struct {
	Home string `json:"home,omitempty"`
	Work string `json:"work,omitempty"`
}

func (d *DefaultProjects) get(mode ProjectMode) string {
	switch mode {
	case ProjectModeHome:
		return d.Home
	case ProjectModeWork:
		return d.Work
	}
	return ""
}

func (d *DefaultProjects) set(mode ProjectMode, projectID string) {
	switch mode {
	case ProjectModeHome:
		d.Home = projectID
	case ProjectModeWork:
		d.Work = projectID
	}
}

// ValidateProjectMode accepts "", home, work and both, exactly and in lower
// case. "" is both.
func ValidateProjectMode(m ProjectMode) error {
	switch m {
	case "", ProjectModeHome, ProjectModeWork, ProjectModeBoth:
		return nil
	}
	return fmt.Errorf("%w %q: want home, work or both", ErrInvalidProjectMode, string(m))
}

// NormalizeProjectMode stores both as "", so every project that predates
// modes and every project set to both serialize identically.
func NormalizeProjectMode(m ProjectMode) ProjectMode {
	if m == ProjectModeBoth {
		return ""
	}
	return m
}

func effectiveMode(m ProjectMode) ProjectMode {
	switch m {
	case ProjectModeHome, ProjectModeWork:
		return m
	}
	return ProjectModeBoth
}

// EffectiveMode is the one place the stored value is read: home and work as
// stored, anything else (missing, both, or a hand-edited unknown value) as
// both.
func (p *Project) EffectiveMode() ProjectMode {
	return effectiveMode(p.Mode)
}

// Includes reports whether a project in mode m belongs in target. Both
// sides are read as effective modes, so both includes every mode.
func (m ProjectMode) Includes(target ProjectMode) bool {
	own := effectiveMode(m)
	return own == ProjectModeBoth || own == effectiveMode(target)
}

func isDefaultMode(mode ProjectMode) bool {
	return mode == ProjectModeHome || mode == ProjectModeWork
}

// SetProjectMode stores mode normalized, then clears any default the change
// made ineligible. The caller validates mode first.
func (s *Settings) SetProjectMode(id string, mode ProjectMode) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Mode = NormalizeProjectMode(mode)
	s.PruneDefaultProjects()
}

// SetDefaultProject makes projectID the default for mode, or clears it when
// projectID is "". It validates before mutating, so a refusal leaves s
// untouched. Only a set creates the block: clearing a nil block leaves it
// nil, so an install that never configured modes stays unconfigured. Every
// refusal wraps ErrInvalidDefaultProject.
func (s *Settings) SetDefaultProject(mode ProjectMode, projectID string) error {
	if !isDefaultMode(mode) {
		return fmt.Errorf("%w: mode %q has no default project; want home or work", ErrInvalidDefaultProject, string(mode))
	}
	if projectID != "" {
		proj, _ := s.findProjectByID(projectID)
		if proj == nil {
			return fmt.Errorf("%w: no project with id %q", ErrInvalidDefaultProject, projectID)
		}
		if proj.IsRemote() {
			return fmt.Errorf("%w: project %q is an access profile and cannot be a default project", ErrInvalidDefaultProject, projectID)
		}
		if !proj.EffectiveMode().Includes(mode) {
			return fmt.Errorf("%w: project %q is %s-only and cannot be the default for %s", ErrInvalidDefaultProject, projectID, proj.EffectiveMode(), mode)
		}
	}
	if s.DefaultProject == nil {
		if projectID == "" {
			return nil
		}
		s.DefaultProject = &DefaultProjects{}
	}
	s.DefaultProject.set(mode, projectID)
	return nil
}

// DefaultProjectFor returns the stored default for mode only while it is
// still valid: the project exists, is local and its mode includes mode.
// Otherwise "".
func (s *Settings) DefaultProjectFor(mode ProjectMode) string {
	if s.DefaultProject == nil || !isDefaultMode(mode) {
		return ""
	}
	id := s.DefaultProject.get(mode)
	if id == "" {
		return ""
	}
	proj, _ := s.findProjectByID(id)
	if proj == nil || proj.IsRemote() || !proj.EffectiveMode().Includes(mode) {
		return ""
	}
	return id
}

// DefaultModesFor lists, in DefaultModes order, the modes projectID is the
// valid default for.
func (s *Settings) DefaultModesFor(projectID string) []ProjectMode {
	if projectID == "" {
		return nil
	}
	var out []ProjectMode
	for _, mode := range DefaultModes {
		if s.DefaultProjectFor(mode) == projectID {
			out = append(out, mode)
		}
	}
	return out
}

// PruneDefaultProjects clears every stored default that is no longer valid
// and returns the modes it cleared. It never nils the block: an emptied
// block still records that the operator uses modes.
func (s *Settings) PruneDefaultProjects() []ProjectMode {
	if s.DefaultProject == nil {
		return nil
	}
	var cleared []ProjectMode
	for _, mode := range DefaultModes {
		if s.DefaultProject.get(mode) != "" && s.DefaultProjectFor(mode) == "" {
			s.DefaultProject.set(mode, "")
			cleared = append(cleared, mode)
		}
	}
	return cleared
}
