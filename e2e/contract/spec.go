package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"relaye2e/harness"
)

// Spec is the one world both targets are built from. "{remote}" in a path is
// replaced by each target's remote root, and "{instance}" by its instance
// directory.
type Spec struct {
	Credentials      []harness.CredentialSpec // name and classes; no expiry
	Presence         map[string]harness.Outcome
	Projects         []Project
	Hosts            []Host
	MCPs             []MCP             // stdio only
	ConsoleTemplates []json.RawMessage // relay TerminalTemplate records
	Services         []Service         // each runs e2e/fakes/fakeservice
	DefaultProject   *struct{ Home, Work string }
	ChiefOfStaff     *struct {
		ProjectID, Model string
		DailyModelCalls  int
	}
}

// Project is one project record. A console project has no HostID.
type Project struct {
	ID, Name, Mode, HostID string
	Path                   string // host: absolute, under "{remote}"; console: "" = <instance>/projects/<id>
	FilesReadOnly          bool
	Files                  map[string]File // seeded on disk by the runner, for both targets
	Repo                   *Repo           // git init and commits with a fixed identity and dates, so SHAs match
}

// File is one seeded path. Set exactly one of Text, Base64, Symlink or Dir.
type File struct {
	Text, Base64, Symlink string
	Dir                   bool
}

// Repo is a git repository seeded at the project root.
type Repo struct {
	Branch  string
	Commits []Commit
}

// Commit is one commit; Files maps a relative path to its text.
type Commit struct {
	Message string
	Files   map[string]string
}

// Host is an ssh host, reached on the real target through the ssh stub.
type Host struct {
	ID, Name, Target string // Target is neutral, such as "acme@testbox"
	Probed           bool   // seed one probe record on both targets
	Templates        []json.RawMessage
	Persistent       []string // tmux session names made before boot
}

// MCP is a stdio fake MCP server.
type MCP struct {
	ID, Name  string
	Catalogue harness.Catalogue
}

// Service is a fakeservice. Config, when set, is written to the file passed
// as its --config.
type Service struct {
	ID, Name string
	Config   json.RawMessage
}

const (
	fixedStamp  = "2026-01-01T00:00:00Z"
	tokRemote   = "{remote}"
	tokInstance = "{instance}"
)

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("contract: encoding %T: %v", v, err))
	}
	return b
}

func (sp Spec) validate() error {
	seen := map[string]bool{}
	for _, h := range sp.Hosts {
		if h.ID == "" || h.Target == "" {
			return fmt.Errorf("a Host needs an ID and a Target: %+v", h)
		}
		seen[h.ID] = true
	}
	for _, p := range sp.Projects {
		if p.ID == "" || p.Name == "" {
			return fmt.Errorf("a Project needs an ID and a Name: %+v", p)
		}
		if p.HostID != "" && !seen[p.HostID] {
			return fmt.Errorf("project %s names host %s, which is not in Spec.Hosts", p.ID, p.HostID)
		}
		if p.HostID != "" && !strings.HasPrefix(p.Path, "/") && !strings.HasPrefix(p.Path, tokRemote) {
			return fmt.Errorf("host project %s needs an absolute Path under %s, got %q", p.ID, tokRemote, p.Path)
		}
	}
	return nil
}

func (p Project) dir() string {
	if p.Path != "" {
		return p.Path
	}
	return tokInstance + "/projects/" + p.ID
}

// toolEntry is the test process's own tool, found once.
type toolEntry struct {
	once          sync.Once
	path, version string
	err           error
}

var toolCache sync.Map // name -> *toolEntry

func findTool(name string, versionArgs ...string) (string, string, error) {
	v, _ := toolCache.LoadOrStore(name, &toolEntry{})
	e := v.(*toolEntry)
	e.once.Do(func() {
		p, err := exec.LookPath(name)
		if err == nil {
			p, err = filepath.Abs(p)
		}
		if err != nil {
			e.err = fmt.Errorf("host scenarios need %s on PATH: %w", name, err)
			return
		}
		e.path = p
		if len(versionArgs) > 0 {
			out, verr := exec.Command(p, versionArgs...).Output()
			if verr != nil {
				e.err = fmt.Errorf("running %s %v: %w", p, versionArgs, verr)
				return
			}
			e.version = strings.TrimSpace(string(out))
		}
	})
	return e.path, e.version, e.err
}

func unameField(flag string) (string, error) {
	out, err := exec.Command("uname", flag).Output()
	return strings.TrimSpace(string(out)), err
}

// probeRecord is the one probe both targets are seeded with. The dynamic
// fields are normalised, so the values only need to be plausible.
func probeRecord() (map[string]any, error) {
	node, nodeVersion, err := findTool("node", "--version")
	if err != nil {
		return nil, err
	}
	tmux, _, err := findTool("tmux")
	if err != nil {
		return nil, err
	}
	osName, err := unameField("-s")
	if err != nil {
		return nil, fmt.Errorf("uname -s: %w", err)
	}
	arch, err := unameField("-m")
	if err != nil {
		return nil, fmt.Errorf("uname -m: %w", err)
	}
	rec := map[string]any{
		"at": fixedStamp, "ok": true, "os": osName, "arch": arch,
		"home": tokRemote, "shell": "/bin/sh",
		"node_path": node, "tmux_path": tmux,
	}
	rec["node_version"] = nodeVersion
	return rec, nil
}

// hostRecords renders Spec.Hosts. fake selects the world.json field set.
func (sp Spec) hostRecords(fake bool) ([]any, error) {
	var out []any
	for _, h := range sp.Hosts {
		rec := map[string]any{"id": h.ID, "name": h.Name, "target": h.Target}
		if h.Probed {
			p, err := probeRecord()
			if err != nil {
				return nil, err
			}
			rec["probe"] = p
		}
		if len(h.Templates) > 0 {
			rec["terminal_templates"] = h.Templates
		}
		if fake {
			rec["root"] = "/"
			var ps []any
			for _, name := range h.Persistent {
				ps = append(ps, map[string]any{"name": name, "created": 1789000000, "attached": 0})
			}
			if ps != nil {
				rec["persistent_sessions"] = ps
			}
		} else {
			rec["created_at"] = fixedStamp
			if len(h.Persistent) > 0 {
				if _, _, err := findTool("tmux"); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

func (sp Spec) projectRecords(fake bool) []any {
	var out []any
	for _, p := range sp.Projects {
		rec := map[string]any{
			"id": p.ID, "name": p.Name, "path": p.dir(),
			"allowed_models": []string{"*"}, "allowed_templates": []string{"*"},
		}
		if p.Mode != "" {
			rec["mode"] = p.Mode
		}
		if p.HostID != "" {
			rec["host_id"] = p.HostID
		}
		if p.FilesReadOnly {
			rec["files_read_only"] = true
		}
		if !fake {
			// relay refuses to start on a project with no token. The token
			// is derived from the id and nothing reads it back.
			tok := "acme-project-token-" + p.ID
			sum := sha256.Sum256([]byte(tok))
			rec["token"], rec["token_hash"] = tok, hex.EncodeToString(sum[:])
			rec["allowed_mcp_ids"] = []string{}
			rec["created_at"] = fixedStamp
		}
		out = append(out, rec)
	}
	return out
}

func (sp Spec) serviceRecords(fake bool) []any {
	var out []any
	for _, s := range sp.Services {
		args := []string{"--call-log", tokInstance + "/fakes/" + s.ID + ".calls.jsonl"}
		if len(s.Config) > 0 {
			args = append(args, "--config", tokInstance+"/fakes/"+s.ID+".config.json")
		}
		rec := map[string]any{
			"id": s.ID, "command": harness.BundlePaths().FakeService, "args": args,
			"env": map[string]string{}, "autostart": true, "capabilities": []string{"manifest"},
		}
		if fake {
			rec["name"] = s.Name
		} else {
			rec["display_name"] = s.Name
		}
		out = append(out, rec)
	}
	return out
}

func (sp Spec) mcpName(m MCP) string {
	if m.Name != "" {
		return m.Name
	}
	return "Fake MCP " + m.ID
}

// common returns the keys settings.json and world.json share.
func (sp Spec) common(fake bool) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	set := func(k string, v any, n int) {
		if n > 0 {
			m[k] = raw(v)
		}
	}
	hosts, err := sp.hostRecords(fake)
	if err != nil {
		return nil, err
	}
	set("projects", sp.projectRecords(fake), len(sp.Projects))
	set("hosts", hosts, len(sp.Hosts))
	set("services", sp.serviceRecords(fake), len(sp.Services))
	if len(sp.ConsoleTemplates) > 0 {
		m["terminal_templates"] = raw(sp.ConsoleTemplates)
	}
	if d := sp.DefaultProject; d != nil {
		m["default_project"] = raw(map[string]string{"home": d.Home, "work": d.Work})
	}
	if c := sp.ChiefOfStaff; c != nil {
		m["chief_of_staff"] = raw(map[string]any{"project_id": c.ProjectID, "model": c.Model, "daily_model_calls": c.DailyModelCalls})
	}
	return m, nil
}

func (sp Spec) realSettings() (map[string]json.RawMessage, []harness.FakeMCPSpec, error) {
	m, err := sp.common(false)
	if err != nil {
		return nil, nil, err
	}
	var mcps []harness.FakeMCPSpec
	for _, c := range sp.MCPs {
		mcps = append(mcps, harness.FakeMCPSpec{ID: c.ID, Transport: "stdio", Catalogue: c.Catalogue})
	}
	return m, mcps, nil
}

func (sp Spec) fakeWorld() (map[string]json.RawMessage, error) {
	m, err := sp.common(true)
	if err != nil {
		return nil, err
	}
	var mcps []any
	for _, c := range sp.MCPs {
		mcps = append(mcps, map[string]any{"id": c.ID, "name": sp.mcpName(c), "transport": "stdio", "catalogue": c.Catalogue,
			"command": harness.BundlePaths().FakeMCP,
			"args": []string{"--catalogue", tokInstance + "/fakes/" + c.ID + ".catalogue.json",
				"--call-log", tokInstance + "/fakes/" + c.ID + ".calls.jsonl", "--transport", "stdio"}})
	}
	if mcps != nil {
		m["mcps"] = raw(mcps)
	}
	// The real target lists the harness's fake pi and codex CLIs as models
	// (their --list-models output), so the fake world carries the same rows.
	m["models"] = raw([]map[string]any{
		{"value": "pi/fake/fake-echo", "label": "pi/fake/fake-echo", "group": "Pi · fake", "provider": "pi", "reply": map[string]string{"kind": "echo"}},
		{"value": "codex/fake-echo", "label": "Fake Echo", "group": "Codex", "provider": "codex", "reply": map[string]string{"kind": "echo"}},
	})
	// The real target's ssh stub looks tools up on the test process's PATH.
	m["host_path"] = raw(os.Getenv("PATH"))
	return m, nil
}

// patchTokens rewrites the placeholder tokens in a file the harness wrote.
func patchTokens(path string, repl map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(b)
	for k, v := range repl {
		s = strings.ReplaceAll(s, k, v)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
