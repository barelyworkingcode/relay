package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// This file copies, as pure logic, relay's terminal-template folder rules
// (internal/config/templates.go validateGrantPath and ExpandGrantPath, and
// internal/project/scope_breadth.go TemplateFolderWarnings). fakerelay cannot
// import relay; the contract suite keeps the copy honest.

const projectPathMarker = "${PROJECT_PATH}"

var (
	errTemplateGrant      = errors.New("sandbox folder must be an absolute path or start with ~ or ${PROJECT_PATH}, and not be /")
	errTemplateGrantToken = errors.New("sandbox folder may use ${PROJECT_PATH}, and only at its start; no other ${...} expands")
)

func validateGrantPath(p string) error {
	if p == "" {
		return errTemplateGrant
	}
	marked := p == projectPathMarker || strings.HasPrefix(p, projectPathMarker+"/")
	if strings.Contains(p, "${") {
		if !marked || strings.Contains(p[len(projectPathMarker):], "${") {
			return errTemplateGrantToken
		}
	}
	if !marked && p != "~" && !strings.HasPrefix(p, "~/") && !filepath.IsAbs(p) {
		return errTemplateGrant
	}
	if filepath.Clean(p) == "/" {
		return errTemplateGrant
	}
	return nil
}

func expandGrantPath(entry, home, projectPath string) (string, error) {
	if err := validateGrantPath(entry); err != nil {
		return "", err
	}
	p := entry
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, projectPathMarker):
		if projectPath == "" {
			return "", errors.New("${PROJECT_PATH} has no value: this launch has no project folder")
		}
		p = filepath.Join(projectPath, p[len(projectPathMarker):])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	p = filepath.Clean(p)
	if p == "/" {
		return "", errors.New("grants the whole filesystem")
	}
	return p, nil
}

// templateFolders returns the stored folder lists of a template.
func templateFolders(t world.Template) (read, readWrite, deny []string) {
	list := func(key string) []string {
		var out []string
		if raw, ok := t.Rest[key]; ok {
			_ = json.Unmarshal(raw, &out)
		}
		return out
	}
	return list("read"), list("read_write"), list("deny")
}

// validTemplateFolders refuses a bad sandbox folder with relay's text.
func validTemplateFolders(t world.Template) *httpErr {
	read, readWrite, deny := templateFolders(t)
	for _, l := range []struct {
		field   string
		entries []string
	}{{"read", read}, {"read_write", readWrite}, {"deny", deny}} {
		for _, e := range l.entries {
			if err := validateGrantPath(e); err != nil {
				return &httpErr{http.StatusBadRequest, fmt.Sprintf("terminal template %q: %s %q: %v", t.ID, l.field, e, err)}
			}
		}
	}
	return nil
}

// templateSandboxed is true unless the template says "sandbox": false.
func templateSandboxed(t world.Template) bool {
	var v *bool
	if raw, ok := t.Rest["sandbox"]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v == nil || *v
}

const (
	breadthBounded = ""
	breadthHome    = "home"
	breadthRoot    = "root"
)

func breadthPhrase(kind string) string {
	switch kind {
	case breadthRoot:
		return "unrestricted (the whole filesystem)"
	case breadthHome:
		return "a whole home directory"
	}
	return ""
}

func entryBreadth(entry string) string {
	v := strings.TrimSpace(entry)
	if v == "" {
		return breadthBounded
	}
	if v == "~" || v == "~/" {
		return breadthHome
	}
	if !strings.HasPrefix(v, "/") {
		return breadthBounded
	}
	clean := filepath.Clean(v)
	if clean == "/" {
		return breadthRoot
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) <= 2 && (parts[0] == "Users" || parts[0] == "home") {
		return breadthHome
	}
	return breadthBounded
}

func widerBreadth(a, b string) string {
	if a == breadthRoot || b == breadthRoot {
		return breadthRoot
	}
	if a == breadthHome || b == breadthHome {
		return breadthHome
	}
	return breadthBounded
}

func templateFolderWarnings(t world.Template, projectPath, home string) []string {
	read, readWrite, _ := templateFolders(t)
	var out []string
	for _, l := range []struct {
		field   string
		entries []string
	}{{"read", read}, {"read_write", readWrite}} {
		for _, entry := range l.entries {
			expanded, err := expandGrantPath(entry, home, projectPath)
			if err != nil {
				continue
			}
			kind := widerBreadth(entryBreadth(entry), entryBreadth(expanded))
			if home != "" && (expanded == filepath.Clean(home) || strings.HasPrefix(filepath.Clean(home), expanded+"/")) {
				kind = widerBreadth(kind, breadthHome)
			}
			if phrase := breadthPhrase(kind); phrase != "" {
				out = append(out, fmt.Sprintf("%s %q is %s", l.field, entry, phrase))
			}
		}
	}
	return out
}

type grantTemplate struct {
	ID        string   `json:"id"`
	Sandbox   bool     `json:"sandbox"`
	Read      []string `json:"read,omitempty"`
	ReadWrite []string `json:"read_write,omitempty"`
	Deny      []string `json:"deny,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// grantTemplates lists the templates a local project may launch, sorted by id.
// A lone "*" allows every template and an empty list none, as relay's
// Project.AllowsTemplate does; a template that fails the folder rules is
// absent, as relay's resolution drops it.
func grantTemplates(all []world.Template, p world.Project) []grantTemplate {
	home, _ := os.UserHomeDir()
	var out []grantTemplate
	for _, t := range all {
		if !grantAllowsTemplate(p.AllowedTemplates, t.ID) || validTemplateFolders(t) != nil {
			continue
		}
		read, readWrite, deny := templateFolders(t)
		out = append(out, grantTemplate{
			ID: t.ID, Sandbox: templateSandboxed(t),
			Read: read, ReadWrite: readWrite, Deny: deny,
			Warnings: templateFolderWarnings(t, p.Path, home),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func grantAllowsTemplate(list []string, id string) bool {
	if len(list) == 1 && list[0] == "*" {
		return true
	}
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func writeGrantTemplates(b *strings.Builder, ts []grantTemplate) (warned bool) {
	for _, t := range ts {
		sandbox := "off"
		if t.Sandbox {
			sandbox = "on"
		}
		line := fmt.Sprintf("  template %-12s sandbox=%s", t.ID, sandbox)
		for _, l := range []struct {
			name    string
			folders []string
		}{{"read", t.Read}, {"read_write", t.ReadWrite}, {"deny", t.Deny}} {
			if len(l.folders) > 0 {
				line += fmt.Sprintf("  %s=%s", l.name, strings.Join(l.folders, ","))
			}
		}
		if len(t.Warnings) > 0 {
			line += "  !! " + strings.Join(t.Warnings, "; ")
			warned = true
		}
		b.WriteString(line + "\n")
	}
	return warned
}
