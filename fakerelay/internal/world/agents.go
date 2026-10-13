package world

import (
	"encoding/json"
	"regexp"
	"strings"
)

// MaxAgentEntries is the most agents one project holds.
const MaxAgentEntries = 32

// AgentEntry is one named way to start a session in a project.
type AgentEntry struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Model   string   `json:"model,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// AgentRuleError is a refusal with the code the agent doors answer.
type AgentRuleError struct{ Code, Message string }

func (e *AgentRuleError) Error() string { return e.Message }

var (
	agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	bareCmdRe   = regexp.MustCompile(`^[A-Za-z0-9._+][A-Za-z0-9._+-]{0,63}$`)
)

func ruleErr(code, msg string) *AgentRuleError { return &AgentRuleError{code, msg} }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// ValidateAgentShape checks an entry as a door receives it: the command may be
// a bare name.
func ValidateAgentShape(e AgentEntry) *AgentRuleError {
	if !agentNameRe.MatchString(e.Name) {
		return ruleErr("name_invalid", "agent name must be 1 to 40 letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	if e.Kind != "claude" && e.Kind != "codex" && e.Kind != "pi" {
		return ruleErr("kind_invalid", "kind must be claude, codex or pi")
	}
	if len(e.Model) > 200 || hasControl(e.Model) || strings.IndexFunc(e.Model, isSpace) >= 0 {
		return ruleErr("model_invalid", "model must be at most 200 bytes with no spaces or control characters")
	}
	if e.Kind == "pi" && e.Model == "" {
		return ruleErr("model_required", "a pi agent needs a model")
	}
	if e.Command != "" && !commandShapeOK(e.Command) {
		return ruleErr("command_invalid", "command must be a program name or an absolute path")
	}
	if len(e.Args) > 0 && e.Command == "" {
		return ruleErr("args_need_command", "args need a command")
	}
	if len(e.Args) > 16 {
		return ruleErr("args_invalid", argsMsg)
	}
	for _, a := range e.Args {
		if len(a) > 256 || strings.ContainsRune(a, 0) || strings.Contains(a, "${") {
			return ruleErr("args_invalid", argsMsg)
		}
	}
	return nil
}

const argsMsg = "args: at most 16, each at most 256 bytes, with no NUL or ${"

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == 0x85 || r == 0xA0 || r == 0x2028 || r == 0x2029 || r == 0x3000
}

func commandShapeOK(c string) bool {
	if !strings.HasPrefix(c, "/") {
		return bareCmdRe.MatchString(c)
	}
	if len(c) > 1024 || hasControl(c) || strings.ContainsAny(c, "\\'\"$`") {
		return false
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// ValidateStoredAgent is the shape check plus: a stored command is absolute.
func ValidateStoredAgent(e AgentEntry) *AgentRuleError {
	if err := ValidateAgentShape(e); err != nil {
		return err
	}
	if e.Command != "" && (!strings.HasPrefix(e.Command, "/") || !commandShapeOK(e.Command)) {
		return ruleErr("command_invalid", "command must be a program name or an absolute path")
	}
	return nil
}

// AgentNameClash reports a project's agent or chat template that already
// holds name, ignoring the agent called except. Names compare
// case-insensitively.
func AgentNameClash(p Project, name, except string) *AgentRuleError {
	for _, a := range p.Agents {
		if strings.EqualFold(a.Name, name) && (except == "" || !strings.EqualFold(a.Name, except)) {
			return ruleErr("name_taken", `"`+name+`" is already an agent in `+p.Name)
		}
	}
	for _, n := range ChatTemplateNames(p) {
		if strings.EqualFold(n, name) {
			return ruleErr("name_taken", `"`+name+`" is already a chat template in `+p.Name)
		}
	}
	return nil
}

// ChatTemplateNames lists the names of a project's chat templates.
func ChatTemplateNames(p Project) []string {
	var out []string
	for _, raw := range p.ChatTemplates {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil && t.Name != "" {
			out = append(out, t.Name)
		}
	}
	return out
}

// FindAgent returns the agent called name (case-insensitive) and its index.
func FindAgent(p Project, name string) (AgentEntry, int, bool) {
	for i, a := range p.Agents {
		if strings.EqualFold(a.Name, name) {
			return a, i, true
		}
	}
	return AgentEntry{}, -1, false
}

// SessionModel is the model a start by this entry asks for.
func SessionModel(e AgentEntry) string {
	if e.Model == "" {
		return ""
	}
	switch e.Kind {
	case "codex":
		return "codex/" + e.Model
	case "pi":
		return "pi/relay-router/" + e.Model
	}
	return e.Model
}
