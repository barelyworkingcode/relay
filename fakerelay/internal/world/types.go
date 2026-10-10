// Package world holds the fakerelay world spec: the types of DIR/world.json,
// its loader and its validator.
package world

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

// World is the whole spec. It is read once at serve and never mutated.
type World struct {
	Schema         int               `json:"schema"`
	Listeners      Listeners         `json:"listeners"`
	Credentials    []Credential      `json:"credentials"`
	Presence       map[string]string `json:"presence"`
	Faults         []Fault           `json:"faults"`
	Projects       []Project         `json:"projects"`
	DefaultProject *DefaultProject   `json:"default_project,omitempty"`
	ChiefOfStaff   *ChiefOfStaff     `json:"chief_of_staff,omitempty"`
	Hosts          []Host            `json:"hosts"`
	MCPs           []MCP             `json:"mcps"`
	Models         []Model           `json:"models"`
	Templates      []Template        `json:"terminal_templates"`
	Sessions       []Session         `json:"sessions"`
	Eve            Eve               `json:"eve"`
	Audit          []json.RawMessage `json:"audit"`
	Services       []Service         `json:"services"`
	// HostPath is the PATH a host probe looks node, claude and tmux up on.
	// Empty means fakerelay's own PATH.
	HostPath string `json:"host_path,omitempty"`
}

// Listeners are loopback host:port addresses; "" turns a listener off.
type Listeners struct {
	API   string `json:"api"`
	Model string `json:"model"`
}

type Credential struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Classes []string `json:"classes"`
	Token   string   `json:"token"`
	Expires string   `json:"expires,omitempty"`
}

// Fault is a route fault. Times 0 holds until cleared.
type Fault struct {
	ID      string          `json:"id,omitempty"`
	Route   string          `json:"route"`
	Mode    string          `json:"mode"`
	Times   int             `json:"times,omitempty"`
	DelayMS int             `json:"delay_ms,omitempty"`
	Name    string          `json:"name,omitempty"`
	Status  int             `json:"status,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

type Project struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Path             string            `json:"path"`
	Kind             string            `json:"kind"`
	HostID           string            `json:"host_id"`
	Mode             string            `json:"mode"`
	FilesReadOnly    bool              `json:"files_read_only"`
	AllowedMCPIDs    []string          `json:"allowed_mcp_ids"`
	AllowedModels    []string          `json:"allowed_models"`
	AllowedTemplates []string          `json:"allowed_templates"`
	ChatTemplates    []json.RawMessage `json:"chat_templates"`
	PermissionPolicy json.RawMessage   `json:"permission_policy,omitempty"`
	Files            map[string]File   `json:"files,omitempty"`
	Repos            []Repo            `json:"repos,omitempty"`
}

// Repo is a real git repository seeded under the project root.
type Repo struct {
	Dir     string   `json:"dir"`
	Branch  string   `json:"branch"`
	Commits []Commit `json:"commits"`
}

type Commit struct {
	Message string          `json:"message"`
	Files   map[string]File `json:"files"`
}

// File kinds.
const (
	FileDir     = "dir"
	FileText    = "text"
	FileBase64  = "base64"
	FileSymlink = "symlink"
)

// File is one seeded entry: null or a trailing-slash key is a directory.
type File struct {
	Kind   string
	Text   string
	Data   []byte
	Target string
}

func (f *File) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*f = File{Kind: FileDir}
	case string:
		*f = File{Kind: FileText, Text: x}
	case map[string]any:
		if s, ok := x["base64"].(string); ok && len(x) == 1 {
			d, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return errors.New("base64 is not valid")
			}
			*f = File{Kind: FileBase64, Data: d}
		} else if s, ok := x["symlink"].(string); ok && len(x) == 1 {
			*f = File{Kind: FileSymlink, Target: s}
		} else {
			return errors.New(`a file object holds exactly one of "base64" or "symlink" as a string`)
		}
	default:
		return errors.New("a file is null, a string, {\"base64\"} or {\"symlink\"}")
	}
	return nil
}

func (f File) MarshalJSON() ([]byte, error) {
	switch f.Kind {
	case FileText:
		return json.Marshal(f.Text)
	case FileBase64:
		return json.Marshal(map[string]string{"base64": base64.StdEncoding.EncodeToString(f.Data)})
	case FileSymlink:
		return json.Marshal(map[string]string{"symlink": f.Target})
	}
	return []byte("null"), nil
}

type DefaultProject struct {
	Home string `json:"home"`
	Work string `json:"work"`
}

type ChiefOfStaff struct {
	ProjectID       string `json:"project_id"`
	Model           string `json:"model"`
	DailyModelCalls int    `json:"daily_model_calls"`
}

// Host is a simulated SSH host. Root stands in for the remote machine.
type Host struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	Target             string              `json:"target"`
	Port               int                 `json:"port,omitempty"`
	IdentityFile       string              `json:"identity_file,omitempty"`
	Probe              *Probe              `json:"probe,omitempty"`
	TmuxPath           string              `json:"tmux_path,omitempty"`
	TerminalTemplates  []Template          `json:"terminal_templates,omitempty"`
	Root               string              `json:"root"`
	Agent              string              `json:"agent"`
	PersistentSessions []PersistentSession `json:"persistent_sessions,omitempty"`
}

type Probe struct {
	At            string `json:"at,omitempty"`
	OK            bool   `json:"ok"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Home          string `json:"home,omitempty"`
	Shell         string `json:"shell,omitempty"`
	NodePath      string `json:"node_path,omitempty"`
	NodeVersion   string `json:"node_version,omitempty"`
	ClaudePath    string `json:"claude_path,omitempty"`
	ClaudeVersion string `json:"claude_version,omitempty"`
	TmuxPath      string `json:"tmux_path,omitempty"`
	Error         string `json:"error,omitempty"`
}

type PersistentSession struct {
	Name     string `json:"name"`
	Created  int64  `json:"created"`
	Attached int    `json:"attached"`
}

// Template is relay's TerminalTemplate. Only id and name are read here; every
// other key is carried through untouched.
type Template struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Rest map[string]json.RawMessage
}

func (t *Template) UnmarshalJSON(b []byte) error {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = Template{}
	if v, ok := m["id"]; ok {
		if err := json.Unmarshal(v, &t.ID); err != nil {
			return errors.New("id must be a string")
		}
		delete(m, "id")
	}
	if v, ok := m["name"]; ok {
		if err := json.Unmarshal(v, &t.Name); err != nil {
			return errors.New("name must be a string")
		}
		delete(m, "name")
	}
	t.Rest = m
	return nil
}

func (t Template) MarshalJSON() ([]byte, error) {
	m := map[string]json.RawMessage{}
	for k, v := range t.Rest {
		m[k] = v
	}
	id, _ := json.Marshal(t.ID)
	name, _ := json.Marshal(t.Name)
	m["id"], m["name"] = id, name
	return json.Marshal(m)
}

type MCP struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Catalogue json.RawMessage `json:"catalogue,omitempty"`
	// Command and Args (stdio) or URL (http) are only what `relay mcp list`
	// prints in its ENDPOINT column; fakerelay never starts the command.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
}

type Model struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Provider string `json:"provider"`
	Reply    Reply  `json:"reply"`
}

// Reply scripts the echo agent: kind echo, text, permission or fail.
type Reply struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
}

type Session struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Model     string    `json:"model"`
	State     string    `json:"state"`
	Messages  []Message `json:"messages"`
}

type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Eve is the passkey enrolment state. WindowExpires is an RFC 3339 time set at
// run time when the enrolment window opens.
type Eve struct {
	Passkeys      []json.RawMessage `json:"passkeys"`
	Revocations   []json.RawMessage `json:"revocations"`
	EnrolmentOpen bool              `json:"enrolment_open"`
	WindowExpires string            `json:"window_expires,omitempty"`
}

// Service is relay's services[] record.
type Service struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	WorkingDir   string            `json:"working_dir"`
	Capabilities []string          `json:"capabilities"`
	Autostart    bool              `json:"autostart"`
}
