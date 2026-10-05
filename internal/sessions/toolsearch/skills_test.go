package toolsearch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillsRoot(t *testing.T, project string) string {
	t.Helper()
	d := filepath.Join(project, ".claude", "skills")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func writeSkill(t *testing.T, project, dir, content string) {
	t.Helper()
	d := filepath.Join(skillsRoot(t, project), dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, desc string, tools ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n# Title\n\n## Tools\n", name, desc)
	for _, tl := range tools {
		fmt.Fprintf(&b, "- **%s** — does %s\n", tl, tl)
	}
	return b.String()
}

func TestReadSkills_MissingDirIsEmpty(t *testing.T) {
	got, err := ReadSkills(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("ReadSkills(no .claude) = %v, %v; want none, nil", got, err)
	}
}

func TestReadSkills_FrontmatterAndTools(t *testing.T) {
	cases := []struct {
		name, md  string
		wantName  string
		wantDesc  string
		wantKw    []string
		wantTools []string
	}{
		{"plain with flow keywords",
			"---\nname: relay-a\ndescription: Plain words here\nkeywords: [alpha, beta]\n---\n## Tools\n- **t_one** — x\n- **t_two** — y\n",
			"relay-a", "Plain words here", []string{"alpha", "beta"}, []string{"t_one", "t_two"}},
		{"double quoted with escapes, block keywords",
			"---\nname: \"relay-b\"\ndescription: \"say \\\"hi\\\" and \\\\ bye\"\nkeywords:\n  - gamma\n  - delta\n---\n## Tools\n- **t_b** — x\n",
			"relay-b", `say "hi" and \ bye`, []string{"gamma", "delta"}, []string{"t_b"}},
		{"single quoted, comma keywords",
			"---\nname: 'relay-c'\ndescription: 'it works'\nkeywords: one, two, three\n---\n## Tools\n- **t_c** — x\n",
			"relay-c", "it works", []string{"one", "two", "three"}, []string{"t_c"}},
		{"nested keywords key ignored, other keys ignored",
			"---\nname: relay-d\nother: zzz\nmeta:\n  keywords: [nested]\ndescription: d\n---\n## Tools\n- **t_d** — x\n",
			"relay-d", "d", nil, []string{"t_d"}},
		{"tools stop at next heading and ignore other sections",
			"---\nname: relay-e\ndescription: e\n---\n## Intro\n- **not_a_tool** — x\n## Tools\n- **t_e1** — x\nplain line\n- **t_e2** — y\n## Notes\n- **t_after** — z\n",
			"relay-e", "e", nil, []string{"t_e1", "t_e2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proj := t.TempDir()
			writeSkill(t, proj, "dir-x", tc.md)
			got, err := ReadSkills(proj)
			if err != nil || len(got) != 1 {
				t.Fatalf("ReadSkills = %+v, %v; want one skill", got, err)
			}
			s := got[0]
			if s.Name != tc.wantName || s.Description != tc.wantDesc {
				t.Errorf("name/desc = %q/%q, want %q/%q", s.Name, s.Description, tc.wantName, tc.wantDesc)
			}
			if strings.Join(s.Keywords, "|") != strings.Join(tc.wantKw, "|") {
				t.Errorf("keywords = %v, want %v", s.Keywords, tc.wantKw)
			}
			if strings.Join(s.Tools, "|") != strings.Join(tc.wantTools, "|") {
				t.Errorf("tools = %v, want %v", s.Tools, tc.wantTools)
			}
		})
	}
}

func TestReadSkills_NameFallsBackToDirAndOrderIsByDir(t *testing.T) {
	proj := t.TempDir()
	writeSkill(t, proj, "zz-last", "---\ndescription: z\n---\n## Tools\n- **t_z** — x\n")
	writeSkill(t, proj, "aa-first", skillMD("named-first", "a", "t_a"))
	got, err := ReadSkills(proj)
	if err != nil || len(got) != 2 {
		t.Fatalf("ReadSkills = %+v, %v", got, err)
	}
	if got[0].Name != "named-first" || got[1].Name != "zz-last" {
		t.Fatalf("names/order = %q, %q; want named-first then zz-last (dir name fallback)", got[0].Name, got[1].Name)
	}
}

// A skill file is project-controlled input: links must never lead the
// reader outside the project's own tree.
func TestReadSkills_SkipsSymlinksAtEveryLevel(t *testing.T) {
	good := skillMD("outside", "o", "t_out")
	setups := map[string]func(t *testing.T, proj, outside string){
		"SKILL.md symlink": func(t *testing.T, proj, outside string) {
			d := filepath.Join(skillsRoot(t, proj), "linked")
			must(t, os.MkdirAll(d, 0o755))
			must(t, os.WriteFile(filepath.Join(outside, "real.md"), []byte(good), 0o644))
			must(t, os.Symlink(filepath.Join(outside, "real.md"), filepath.Join(d, "SKILL.md")))
		},
		"skill dir symlink": func(t *testing.T, proj, outside string) {
			must(t, os.MkdirAll(filepath.Join(outside, "sk"), 0o755))
			must(t, os.WriteFile(filepath.Join(outside, "sk", "SKILL.md"), []byte(good), 0o644))
			must(t, os.Symlink(filepath.Join(outside, "sk"), filepath.Join(skillsRoot(t, proj), "linked")))
		},
		"skills dir symlink": func(t *testing.T, proj, outside string) {
			must(t, os.MkdirAll(filepath.Join(outside, "skills", "sk"), 0o755))
			must(t, os.WriteFile(filepath.Join(outside, "skills", "sk", "SKILL.md"), []byte(good), 0o644))
			must(t, os.MkdirAll(filepath.Join(proj, ".claude"), 0o755))
			must(t, os.Symlink(filepath.Join(outside, "skills"), filepath.Join(proj, ".claude", "skills")))
		},
		".claude symlink": func(t *testing.T, proj, outside string) {
			must(t, os.MkdirAll(filepath.Join(outside, "dotclaude", "skills", "sk"), 0o755))
			must(t, os.WriteFile(filepath.Join(outside, "dotclaude", "skills", "sk", "SKILL.md"), []byte(good), 0o644))
			must(t, os.Symlink(filepath.Join(outside, "dotclaude"), filepath.Join(proj, ".claude")))
		},
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			proj, outside := t.TempDir(), t.TempDir()
			setup(t, proj, outside)
			got, _ := ReadSkills(proj)
			if len(got) != 0 {
				t.Fatalf("symlinked skill was read: %+v", got)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadSkills_Limits(t *testing.T) {
	t.Run("oversize file skipped, neighbours kept", func(t *testing.T) {
		proj := t.TempDir()
		writeSkill(t, proj, "a-big", skillMD("big", "b", "t_big")+strings.Repeat("x", 65*1024))
		writeSkill(t, proj, "b-ok", skillMD("ok", "o", "t_ok"))
		got, _ := ReadSkills(proj)
		if len(got) != 1 || got[0].Name != "ok" {
			t.Fatalf("got %+v, want only the small skill", got)
		}
	})
	t.Run("at most 256 skills", func(t *testing.T) {
		proj := t.TempDir()
		for i := 0; i < 300; i++ {
			writeSkill(t, proj, fmt.Sprintf("s%03d", i), skillMD(fmt.Sprintf("s%03d", i), "d", fmt.Sprintf("t_%03d", i)))
		}
		got, _ := ReadSkills(proj)
		if len(got) != 256 {
			t.Fatalf("read %d skills, want exactly 256", len(got))
		}
	})
}
