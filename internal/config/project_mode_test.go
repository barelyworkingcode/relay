package config

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func pmSettings() *Settings {
	return &Settings{Projects: []Project{
		{ID: "p-both", Name: "Acme"},
		{ID: "p-home", Name: "Acme home", Mode: ProjectModeHome},
		{ID: "p-work", Name: "Acme work", Mode: ProjectModeWork},
		{ID: "p-remote", Name: "Acme remote", Kind: ProjectKindRemote},
	}}
}

func TestProjectMode_ValidateNormalizeEffective(t *testing.T) {
	cases := []struct {
		stored    ProjectMode
		valid     bool
		effective ProjectMode
	}{
		{"", true, ProjectModeBoth},
		{ProjectModeHome, true, ProjectModeHome},
		{ProjectModeWork, true, ProjectModeWork},
		{ProjectModeBoth, true, ProjectModeBoth},
		{"Home", false, ProjectModeBoth},
		{"office", false, ProjectModeBoth},
	}
	for _, c := range cases {
		t.Run(string(c.stored), func(t *testing.T) {
			err := ValidateProjectMode(c.stored)
			if c.valid != (err == nil) || (err != nil && !errors.Is(err, ErrInvalidProjectMode)) {
				t.Errorf("ValidateProjectMode(%q) = %v, want valid=%v wrapping ErrInvalidProjectMode", c.stored, err, c.valid)
			}
			p := Project{Mode: c.stored}
			if got := p.EffectiveMode(); got != c.effective {
				t.Errorf("EffectiveMode(%q) = %q, want %q", c.stored, got, c.effective)
			}
		})
	}
	if got := NormalizeProjectMode(ProjectModeBoth); got != "" {
		t.Errorf("NormalizeProjectMode(both) = %q, want \"\"", got)
	}
	if got := NormalizeProjectMode(ProjectModeWork); got != ProjectModeWork {
		t.Errorf("NormalizeProjectMode(work) = %q", got)
	}
	includes := map[[2]ProjectMode]bool{
		{ProjectModeBoth, ProjectModeHome}: true, {ProjectModeBoth, ProjectModeWork}: true, {"", ProjectModeHome}: true,
		{ProjectModeHome, ProjectModeHome}: true, {ProjectModeHome, ProjectModeWork}: false,
		{ProjectModeWork, ProjectModeWork}: true, {ProjectModeWork, ProjectModeHome}: false,
	}
	for pair, want := range includes {
		if got := pair[0].Includes(pair[1]); got != want {
			t.Errorf("%q.Includes(%q) = %v, want %v", pair[0], pair[1], got, want)
		}
	}
}

// On disk: both is never written, a hand-edited value is never rewritten,
// and neither a load nor an unrelated save adds a default_project block.
func TestProjectMode_OnDiskShapeAndNoWriteAtLoad(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatal(err)
	}
	if err := store.With(func(s *Settings) {
		s.Projects = []Project{
			{ID: "p1", Name: "Acme", Token: NewSecret("t1"), TokenHash: HashToken("t1")},
			{ID: "p2", Name: "Acme hand-edited", Mode: "office", Token: NewSecret("t2"), TokenHash: HashToken("t2")},
			{ID: "p3", Name: "Acme set both", Mode: ProjectModeHome, Token: NewSecret("t3"), TokenHash: HashToken("t3")},
		}
		s.SetProjectMode("p3", ProjectModeBoth)
	}); err != nil {
		t.Fatal(err)
	}
	before := sdRead(t, dir)
	if n := strings.Count(string(before), `"mode"`); n != 1 || !regexp.MustCompile(`"mode":\s*"office"`).Match(before) {
		t.Errorf("want exactly the hand-edited mode on disk, got %d mode keys:\n%s", n, before)
	}
	if strings.Contains(string(before), `"default_project"`) {
		t.Errorf("a never-configured block was written:\n%s", before)
	}

	reloaded := sealedSettingsStoreAt(dir)
	got := reloaded.Get()
	if after := sdRead(t, dir); string(after) != string(before) {
		t.Errorf("loading rewrote settings.json:\n before %s\n after  %s", before, after)
	}
	for _, p := range got.Projects {
		if p.EffectiveMode() != ProjectModeBoth {
			t.Errorf("%s loads as %q, want both", p.ID, p.EffectiveMode())
		}
	}
	if got.DefaultProject != nil {
		t.Errorf("load created a default_project block: %+v", got.DefaultProject)
	}
	if err := reloaded.With(func(s *Settings) { s.Projects[0].Name = "Acme renamed" }); err != nil {
		t.Fatal(err)
	}
	if after := string(sdRead(t, dir)); strings.Count(after, `"mode"`) != 1 || strings.Contains(after, `"default_project"`) {
		t.Errorf("an unrelated save rewrote modes or added a block:\n%s", after)
	}
}

func TestDefaultProject_SetRefusalsLeaveSettingsUntouched(t *testing.T) {
	cases := []struct {
		name string
		mode ProjectMode
		id   string
	}{
		{"mode both", ProjectModeBoth, "p-both"},
		{"mode empty", "", "p-both"},
		{"mode unknown", "office", "p-both"},
		{"no such project", ProjectModeHome, "p-missing"},
		{"access profile", ProjectModeHome, "p-remote"},
		{"mode mismatch", ProjectModeHome, "p-work"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := pmSettings()
			s.DefaultProject = &DefaultProjects{Home: "p-home"}
			before := s.Clone()
			if err := s.SetDefaultProject(c.mode, c.id); !errors.Is(err, ErrInvalidDefaultProject) {
				t.Fatalf("SetDefaultProject(%q, %q) = %v, want ErrInvalidDefaultProject", c.mode, c.id, err)
			}
			if !reflect.DeepEqual(s, before) {
				t.Errorf("a refusal mutated settings: %+v", s.DefaultProject)
			}
		})
	}
}

func TestDefaultProject_SetClearAndBlockLifecycle(t *testing.T) {
	s := pmSettings()
	if err := s.SetDefaultProject(ProjectModeHome, ""); err != nil || s.DefaultProject != nil {
		t.Fatalf("clearing on a nil block: err=%v block=%+v, want nil block", err, s.DefaultProject)
	}
	for _, mode := range []ProjectMode{ProjectModeWork, ProjectModeHome} {
		if err := s.SetDefaultProject(mode, "p-both"); err != nil {
			t.Fatalf("a both project as the %s default: %v", mode, err)
		}
	}
	if got := s.DefaultModesFor("p-both"); !reflect.DeepEqual(got, []ProjectMode{ProjectModeHome, ProjectModeWork}) {
		t.Errorf("DefaultModesFor = %v, want [home work]", got)
	}
	for _, mode := range DefaultModes {
		if err := s.SetDefaultProject(mode, ""); err != nil {
			t.Fatal(err)
		}
	}
	if s.DefaultProject == nil || *s.DefaultProject != (DefaultProjects{}) {
		t.Errorf("clearing both defaults left %+v, want an empty non-nil block", s.DefaultProject)
	}
}

func TestDefaultProject_ReadsOnlyValidDefaults(t *testing.T) {
	s := pmSettings()
	s.DefaultProject = &DefaultProjects{Home: "p-work", Work: "p-work"}
	if got := s.DefaultProjectFor(ProjectModeHome); got != "" {
		t.Errorf("an ineligible stored home default reads as %q", got)
	}
	if got := s.DefaultProjectFor(ProjectModeWork); got != "p-work" {
		t.Errorf("DefaultProjectFor(work) = %q", got)
	}
	s.DefaultProject = &DefaultProjects{Home: "p-remote", Work: "p-gone"}
	for _, mode := range DefaultModes {
		if got := s.DefaultProjectFor(mode); got != "" {
			t.Errorf("DefaultProjectFor(%s) = %q for a remote or missing project", mode, got)
		}
	}
}

func TestDefaultProject_PruneAndRemoveClearButKeepTheBlock(t *testing.T) {
	s := pmSettings()
	s.DefaultProject = &DefaultProjects{Home: "p-home", Work: "p-both"}
	s.RemoveProject("p-home")
	if s.DefaultProject == nil || s.DefaultProject.Home != "" || s.DefaultProject.Work != "p-both" {
		t.Fatalf("after removing the home default: %+v", s.DefaultProject)
	}
	s.SetProjectMode("p-both", ProjectModeHome)
	if s.DefaultProject == nil || s.DefaultProject.Work != "" {
		t.Fatalf("a mode change that made the work default ineligible left %+v", s.DefaultProject)
	}
	s.DefaultProject = &DefaultProjects{Home: "p-gone", Work: "p-work"}
	if got := s.PruneDefaultProjects(); !reflect.DeepEqual(got, []ProjectMode{ProjectModeHome}) {
		t.Errorf("PruneDefaultProjects = %v, want [home]", got)
	}
	s.DefaultProject.Work = "p-gone"
	s.PruneDefaultProjects()
	if s.DefaultProject == nil {
		t.Error("pruning every entry nilled the block")
	}
}

func TestProjectMode_SetProjectModeStoresBothAsEmpty(t *testing.T) {
	s := pmSettings()
	s.SetProjectMode("p-home", ProjectModeBoth)
	if p, _ := FindProjectByID(s, "p-home"); p.Mode != "" {
		t.Errorf("both stored as %q, want \"\"", p.Mode)
	}
}

func TestDefaultProject_CloneDeepCopies(t *testing.T) {
	s := pmSettings()
	s.DefaultProject = &DefaultProjects{Home: "p-home"}
	cp := s.Clone()
	cp.DefaultProject.Home = "p-both"
	cp.Projects[1].Mode = ProjectModeWork
	if s.DefaultProject.Home != "p-home" || s.Projects[1].Mode != ProjectModeHome {
		t.Errorf("mutating the clone reached the original: %+v %q", s.DefaultProject, s.Projects[1].Mode)
	}
}

func TestProjectMode_StoredTokenUnchangedAcrossModes(t *testing.T) {
	s := pmSettings()
	s.ExternalMcps = []ExternalMcp{{ID: "fsmcp"}, {ID: "macmcp"}}
	base := Project{ID: "p1", Name: "Acme", AllowedMcpIDs: []string{"fsmcp"},
		Access: map[string]string{"fsmcp": AccessRead}, AllowedTools: map[string][]string{"fsmcp": {"fs_read"}}}
	want := StoredTokenForProject(s, &base, "h")
	for _, mode := range []ProjectMode{ProjectModeHome, ProjectModeWork, ProjectModeBoth, "office"} {
		p := base
		p.Mode = mode
		if got := StoredTokenForProject(s, &p, "h"); !reflect.DeepEqual(got, want) {
			t.Errorf("mode %q changed the stored token: %+v, want %+v", mode, got, want)
		}
	}
}
