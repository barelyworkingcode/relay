package project

import (
	"errors"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func pmCreate(t *testing.T, s *config.Settings, name string, mode config.ProjectMode) config.Project {
	t.Helper()
	p, err := ApplyCreate(s, CreateFields{Name: name, Path: t.TempDir(), Mode: mode}, nil)
	if err != nil {
		t.Fatalf("ApplyCreate %s: %v", name, err)
	}
	return p
}

func pmNoSurfaces() McpSurfaces { return nil }

func TestApplyCreate_ProjectMode(t *testing.T) {
	cases := []struct {
		mode   config.ProjectMode
		stored config.ProjectMode
		ok     bool
	}{
		{config.ProjectModeWork, config.ProjectModeWork, true},
		{config.ProjectModeBoth, "", true},
		{"", "", true},
		{"Work", "", false},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			s := &config.Settings{Version: 1}
			created, err := ApplyCreate(s, CreateFields{Name: "Acme", Path: t.TempDir(), Mode: c.mode}, nil)
			if !c.ok {
				if !errors.Is(err, config.ErrInvalidProjectMode) || len(s.Projects) != 0 {
					t.Fatalf("err=%v projects=%d, want ErrInvalidProjectMode and nothing persisted", err, len(s.Projects))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p, _ := config.FindProjectByID(s, created.ID); p.Mode != c.stored {
				t.Errorf("stored mode %q, want %q", p.Mode, c.stored)
			}
		})
	}
}

func TestProjectMode_ApplyUpdateRefusesInvalidModeWithoutMutation(t *testing.T) {
	s := &config.Settings{Version: 1}
	p := pmCreate(t, s, "Acme", config.ProjectModeHome)
	bad := config.ProjectMode("office")
	name := "Acme renamed"
	if _, _, err := ApplyUpdate(s, p.ID, UpdateFields{Name: &name, Mode: &bad}, pmNoSurfaces); !errors.Is(err, config.ErrInvalidProjectMode) {
		t.Fatalf("err = %v, want ErrInvalidProjectMode", err)
	}
	if got, _ := config.FindProjectByID(s, p.ID); got.Mode != config.ProjectModeHome || got.Name != "Acme" {
		t.Errorf("a refused update mutated the record: %+v", got)
	}
}

func TestProjectMode_StoredUnknownModeDoesNotBlockRename(t *testing.T) {
	s := &config.Settings{Version: 1}
	p := pmCreate(t, s, "Acme", "")
	s.Projects[0].Mode = "office"
	name := "Acme renamed"
	if _, _, err := ApplyUpdate(s, p.ID, UpdateFields{Name: &name}, pmNoSurfaces); err != nil {
		t.Fatalf("rename refused by a hand-edited mode: %v", err)
	}
}

func TestDefaultProject_ApplyUpdateThatInvalidatesItClearsIt(t *testing.T) {
	work := config.ProjectMode(config.ProjectModeWork)
	remote := config.ProjectKindRemote
	empty := ""
	cases := []struct {
		name string
		f    UpdateFields
	}{
		{"mode change", UpdateFields{Mode: &work}},
		{"kind change to access profile", UpdateFields{Kind: &remote, Path: &empty}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &config.Settings{Version: 1}
			p := pmCreate(t, s, "Acme", config.ProjectModeHome)
			if err := s.SetDefaultProject(config.ProjectModeHome, p.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ApplyUpdate(s, p.ID, c.f, pmNoSurfaces); err != nil {
				t.Fatalf("the edit was refused instead of clearing the default: %v", err)
			}
			if s.DefaultProject == nil || s.DefaultProject.Home != "" {
				t.Errorf("default_project after the edit = %+v, want an empty non-nil block", s.DefaultProject)
			}
		})
	}
}

func TestProjectMode_NotAGrant(t *testing.T) {
	home := config.ProjectMode(config.ProjectModeHome)
	stored := config.Project{ID: "p1", Name: "Acme", Mode: config.ProjectModeWork, AllowedMcpIDs: []string{"fsmcp"}}
	if got := UpdateWidensGrant(stored, UpdateFields{Mode: &home}); len(got) != 0 {
		t.Errorf("a mode change widens %v, want nothing", got)
	}
	if _, err := DecodeNarrowFields([]byte(`{"mode":"home"}`)); err == nil {
		t.Error("a remote narrowing request may carry mode; it is not a narrowable grant field")
	}
}
