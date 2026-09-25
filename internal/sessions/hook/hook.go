// Package hook implements `relay-sessions hook` (plan-broker-and-sessions.md
// contract C6, "relay-sessions hook" subsection): the command Claude Code's
// PreToolUse hook runs. It reads Claude's hook JSON on stdin, asks the
// session host whether to allow the tool call, and prints Claude's expected
// hookSpecificOutput JSON on stdout.
//
// The hook fails closed: once RELAY_SESSIONS_HOOK_SOCKET is set, every
// failure from there on — a bad session id, unreadable or malformed stdin, a
// dial or transport error, a non-200 response, a body that isn't JSON, or a
// decision other than allow or deny — prints an explicit deny rather than
// exiting silently. Exit 0 with no output is reserved for the two cases
// where Claude Code's own checks are meant to decide instead: no hook socket
// configured (this process was not launched under a session host), and the
// interactive tools (ExitPlanMode, AskUserQuestion, ToolSearch) that have no
// side effects a permission gate needs to see.
//
// The client's HTTP timeout (90s) is set deliberately between the host's
// wait for a human decision (60s) and Claude Code's own hook timeout (120s):
// a slow human times out into the host's own deny before the client gives
// up, and that deny reaches Claude Code before Claude Code stops waiting on
// the hook process.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// EnvHookSocket names the Unix socket relay-sessions serves /permission on.
// Absent means this process was not launched under a session host: exit 0
// with no output, so Claude Code's own built-in checks decide instead.
const EnvHookSocket = "RELAY_SESSIONS_HOOK_SOCKET"

// EnvSessionID is C6's replacement for relayLLM's RELAY_LLM_SESSION_ID.
const EnvSessionID = "RELAY_SESSION_ID"

// clientTimeout bounds the hook's wait for the host's decision. It sits
// between the host's own 60s wait for a human and Claude Code's 120s hook
// timeout; see the package doc.
const clientTimeout = 90 * time.Second

// claudeHookInput is the subset of Claude Code's PreToolUse hook payload
// this command reads. Claude sends more fields; anything else is ignored.
type claudeHookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
}

// permissionRequestBody is today's /api/permission body verbatim (field
// names match relayLLM's cmd/hook/main.go and internal/sessions/permission's
// PermissionRequest), with sessionId now sourced from RELAY_SESSION_ID
// rather than a value the hook socket handshake supplied.
type permissionRequestBody struct {
	SessionID string `json:"sessionId"`
	ToolName  string `json:"toolName"`
	ToolInput string `json:"toolInput"`
	ToolUseID string `json:"toolUseId"`
}

type permissionResponseBody struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// Deps lets tests substitute stdin and the dialer without touching the
// process's real fds or environment.
type Deps struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Environ func(string) string
	Dial    func(ctx context.Context) (net.Conn, error)
}

// Run implements the full hook contract and returns the process exit code,
// which is always 0. Once a hook socket is configured, every outcome writes
// exactly one decision line to stdout; see the package doc for the two
// cases where it instead exits silently.
func Run(d Deps) int {
	if d.Stdin == nil {
		d.Stdin = os.Stdin
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Environ == nil {
		d.Environ = os.Getenv
	}

	hookSocket := d.Environ(EnvHookSocket)
	if hookSocket == "" {
		return 0
	}
	sessionID := d.Environ(EnvSessionID)

	raw, err := io.ReadAll(d.Stdin)
	if err != nil {
		return deny(d.Stdout, "reading stdin: %v", err)
	}
	var in claudeHookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return deny(d.Stdout, "decoding stdin: %v", err)
	}

	// Interactive UI tools have no side effects a permission gate needs to
	// see — skip the round trip entirely, same as relayLLM's hook.
	switch in.ToolName {
	case "ExitPlanMode", "AskUserQuestion", "ToolSearch":
		return 0
	}

	if sessionID == "" {
		return deny(d.Stdout, "RELAY_SESSION_ID is empty")
	}

	body, err := json.Marshal(permissionRequestBody{
		SessionID: sessionID,
		ToolName:  in.ToolName,
		ToolInput: string(in.ToolInput),
		ToolUseID: in.ToolUseID,
	})
	if err != nil {
		return deny(d.Stdout, "encoding request: %v", err)
	}

	if d.Dial == nil {
		d.Dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", hookSocket)
		}
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx) },
		},
		Timeout: clientTimeout,
	}

	req, err := http.NewRequest(http.MethodPost, "http://relay-sessions.localsocket/permission", bytes.NewReader(body))
	if err != nil {
		return deny(d.Stdout, "building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header: C6 admits this call by C3 membership on the
	// accepted connection's peer, never a bearer the hook process could leak.

	resp, err := client.Do(req)
	if err != nil {
		return deny(d.Stdout, "dialing host: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return deny(d.Stdout, "reading host response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return deny(d.Stdout, "host returned %s", resp.Status)
	}
	var result permissionResponseBody
	if err := json.Unmarshal(respBody, &result); err != nil {
		return deny(d.Stdout, "host response is not JSON: %v", err)
	}
	if result.Decision != "allow" && result.Decision != "deny" {
		return deny(d.Stdout, "host returned decision %q", result.Decision)
	}

	writeDecision(d.Stdout, result.Decision, result.Reason)
	return 0
}

// deny writes a "relay-sessions hook: <what failed>" deny line and returns
// the exit code Run always returns.
func deny(w io.Writer, format string, args ...any) int {
	writeDecision(w, "deny", fmt.Sprintf("relay-sessions hook: "+format, args...))
	return 0
}

// writeDecision prints Claude Code's PreToolUse hookSpecificOutput contract
// as a single stdout line.
func writeDecision(w io.Writer, decision, reason string) {
	out, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	})
	if err != nil {
		// decision and reason are plain strings; json.Marshal of this shape
		// cannot fail.
		return
	}
	fmt.Fprintln(w, string(out))
}
