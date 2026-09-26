package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

func TestSandboxSpec_RwFileGrantNeedsAFileName(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"acme", ".acme", ".acme.json", "acme-read", "acme-deny"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	tmpl := &config.TerminalTemplate{
		ID: "custom", Name: "Custom", Sandbox: ptr(true),
		ReadWrite: []string{"~/acme", "~/.acme", "~/.acme.json"},
		Read:      []string{"~/acme-read"},
		Deny:      []string{"~/acme-deny"},
	}

	spec, err := sandboxSpecForLaunch(store.Get(), &proj, proj.Path, KindPTY, tmpl)
	if err != nil {
		t.Fatalf("sandboxSpecForLaunch: %v", err)
	}

	for _, tc := range []struct {
		name     string
		file     string
		inList   []string
		notInAny [][]string
	}{
		{"rw undotted file is a subtree", "acme", spec.ReadWrite, [][]string{spec.ReadWriteFiles}},
		{"rw dot-prefixed undotted file is a subtree", ".acme", spec.ReadWrite, [][]string{spec.ReadWriteFiles}},
		{"rw dotted file is a file grant", ".acme.json", spec.ReadWriteFiles, [][]string{spec.ReadWrite}},
		{"read undotted file is a file grant", "acme-read", spec.ReadFiles, [][]string{spec.Read}},
		{"deny undotted file is denied", "acme-deny", spec.Deny, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(home, tc.file)
			if !slices.Contains(tc.inList, p) {
				t.Errorf("%s missing from its expected list %v", p, tc.inList)
			}
			for _, other := range tc.notInAny {
				if slices.Contains(other, p) {
					t.Errorf("%s also filed in %v", p, other)
				}
			}
		})
	}

	body, err := sandbox.Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	homeReal := sandboxRealPath(t, home)
	for _, name := range []string{"acme", ".acme"} {
		if want := `(subpath "` + filepath.Join(homeReal, name) + `")`; !strings.Contains(body, want) {
			t.Errorf("profile lacks %s\n%s", want, body)
		}
	}
	for _, sibling := range []string{`/acme(\.lock`, `/\.acme(\.lock`} {
		if strings.Contains(body, sibling) {
			t.Errorf("profile grants atomic-write siblings via %s\n%s", sibling, body)
		}
	}
	if want := `/\.acme\.json(\.lock|\.tmp\.[^/]*|\.backup)?$"`; !strings.Contains(body, want) {
		t.Errorf("dotted rw file lost its siblings; profile lacks %s\n%s", want, body)
	}
}
