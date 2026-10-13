package project

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/barelyworkingcode/relay/internal/config"
)

// MaxAgentEntries caps a project's agent list.
const MaxAgentEntries = 32

const (
	maxAgentModelBytes   = 200
	maxAgentCommandBytes = 1024
	maxAgentArgs         = 16
	maxAgentArgBytes     = 256
)

var (
	agentNameRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	agentBareCommandRE = regexp.MustCompile(`^[A-Za-z0-9._+][A-Za-z0-9._+-]{0,63}$`)
)

// AgentRuleError is a refusal from the agent-entry rules. Code is the wire
// error code; Error() is the message the person reads.
type AgentRuleError struct{ Code, Message string }

func (e *AgentRuleError) Error() string { return e.Message }

func agentRule(code, msg string) *AgentRuleError {
	return &AgentRuleError{Code: code, Message: msg}
}

// ValidateAgentEntryShape checks the input grammar of an entry, before any
// command lookup: Command may still be a bare name here.
func ValidateAgentEntryShape(e config.AgentEntry) *AgentRuleError {
	if !agentNameRE.MatchString(e.Name) {
		return agentRule("name_invalid", "agent name must be 1 to 40 letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	switch e.Kind {
	case "claude", "codex", "pi":
	default:
		return agentRule("kind_invalid", "kind must be claude, codex or pi")
	}
	if len(e.Model) > maxAgentModelBytes || strings.ContainsFunc(e.Model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return agentRule("model_invalid", "model must be at most 200 bytes with no spaces or control characters")
	}
	if e.Kind == "pi" && e.Model == "" {
		return agentRule("model_required", "a pi agent needs a model")
	}
	if e.Command != "" && !CommandIsBareName(e.Command) && !validAgentPath(e.Command) {
		return agentRule("command_invalid", "command must be a program name or an absolute path")
	}
	if len(e.Args) > 0 && e.Command == "" {
		return agentRule("args_need_command", "args need a command")
	}
	if len(e.Args) > maxAgentArgs {
		return agentRule("args_invalid", argsInvalidMessage)
	}
	for _, a := range e.Args {
		if len(a) > maxAgentArgBytes || strings.ContainsRune(a, 0) || strings.Contains(a, "${") {
			return agentRule("args_invalid", argsInvalidMessage)
		}
	}
	return nil
}

const argsInvalidMessage = "args: at most 16, each at most 256 bytes, with no NUL or ${"

// validAgentPath reports whether c is an absolute path of bounded length with
// no control character, no ".." segment and none of \ ' " $ or backtick. The
// lookup interpolates the path into a login-shell command line, and those
// characters break its quoting under some shells.
func validAgentPath(c string) bool {
	if !strings.HasPrefix(c, "/") || len(c) > maxAgentCommandBytes {
		return false
	}
	if strings.ContainsFunc(c, unicode.IsControl) || strings.ContainsAny(c, "\\'\"$`") {
		return false
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// ValidateStoredAgentEntry is the shape check plus the stored form of
// Command: absolute, which a lookup result always is.
func ValidateStoredAgentEntry(e config.AgentEntry) *AgentRuleError {
	if err := ValidateAgentEntryShape(e); err != nil {
		return err
	}
	if e.Command != "" && !validAgentPath(e.Command) {
		return agentRule("command_invalid", "command must be a program name or an absolute path")
	}
	return nil
}

// FindAgentEntry returns the entry named name (case-insensitive) and its index.
func FindAgentEntry(p *config.Project, name string) (config.AgentEntry, int, bool) {
	for i, e := range p.Agents {
		if strings.EqualFold(e.Name, name) {
			return e, i, true
		}
	}
	return config.AgentEntry{}, -1, false
}

// AgentNameClash reports whether name collides, case-insensitively, with a
// chat template or another agent entry of p. except names the entry being
// replaced, which never clashes with itself; "" excludes none.
func AgentNameClash(p *config.Project, name, except string) *AgentRuleError {
	for _, t := range p.ChatTemplates {
		if strings.EqualFold(t.Name, name) {
			return agentRule("name_taken", fmt.Sprintf("%q is already a chat template in %s", name, p.Name))
		}
	}
	for _, e := range p.Agents {
		if except != "" && strings.EqualFold(e.Name, except) {
			continue
		}
		if strings.EqualFold(e.Name, name) {
			return agentRule("name_taken", fmt.Sprintf("%q is already an agent in %s", name, p.Name))
		}
	}
	return nil
}

// AgentNames lists the entry names in stored order.
func AgentNames(p *config.Project) []string {
	names := make([]string, 0, len(p.Agents))
	for _, e := range p.Agents {
		names = append(names, e.Name)
	}
	return names
}

// SessionModel is the model string a start sends for e. Each kind's prefix
// is what modelAllowedForProject and the pi broker already expect.
func SessionModel(e config.AgentEntry) string {
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

// CommandIsBareName reports whether c is a program name to look up rather
// than a path.
func CommandIsBareName(c string) bool {
	return agentBareCommandRE.MatchString(c)
}
