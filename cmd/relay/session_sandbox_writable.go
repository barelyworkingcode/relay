package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
)

// sandboxWritableRoots is every root a sandboxed session could write under the
// current settings: home, the always-granted read-write directories, the
// read_write entries of every sandboxed console template and of the kind
// templates, and every local project. A read grant through a link these cover
// refuses the launch.
//
// This is deliberate: the kind templates count whatever their sandbox flag,
// because a claude, pi or chat session on a console project always sandboxes
// with its kind template's folders. Templates come from the same validated
// view a launch uses, so a template the launch path drops is absent here too.
// Hosted and remote projects are left out; no sandboxed session on this
// machine writes them.
func sandboxWritableRoots(settings *config.Settings, home string) ([]string, error) {
	roots := []string{home, os.TempDir()}
	if darwin := darwinUserTempDir(); darwin != "" {
		roots = append(roots, filepath.Clean(darwin))
	}
	roots = append(roots, "/dev", sessionPiSessionsDir())
	if settings == nil {
		return roots, nil
	}

	kindTemplates := map[string]bool{}
	for _, id := range kindTemplateIDs {
		kindTemplates[id] = true
	}
	for _, t := range config.EffectiveTerminalTemplates(settings) {
		if !t.Sandboxed() && !kindTemplates[t.ID] {
			continue
		}
		for _, entry := range t.ReadWrite {
			// A ${PROJECT_PATH} entry is writable under every local project
			// the launch could name it for.
			projectPaths := []string{""}
			if strings.HasPrefix(entry, config.ProjectPathMarker) {
				projectPaths = localProjectPaths(settings)
			}
			for _, projectPath := range projectPaths {
				// Deliberate: an entry no launch can grant is skipped, not fatal.
				// The template's own launch refuses at the same check, so no
				// session ever writes it, and refusing here would refuse every
				// unrelated launch too.
				dirs, files, err := addTemplateGrants(nil, nil, "read_write", []string{entry}, home, projectPath)
				if err != nil {
					slog.Warn("session sandbox: writable root skipped", "template", t.ID, "entry", entry, "error", err)
					continue
				}
				roots = append(append(roots, dirs...), files...)
			}
		}
	}

	for _, path := range localProjectPaths(settings) {
		roots = append(roots, filepath.Clean(path))
	}
	return roots, nil
}

// localProjectPaths is the folder of every project a session on this machine
// can write: not remote, not hosted, with a path set.
func localProjectPaths(settings *config.Settings) []string {
	var paths []string
	for i := range settings.Projects {
		p := &settings.Projects[i]
		if p.IsRemote() || p.IsHosted() || p.Path == "" {
			continue
		}
		paths = append(paths, p.Path)
	}
	return paths
}
