// Package hook implements `relay-sessions hook` (plan-broker-and-sessions.md
// contract C6, "relay-sessions hook" subsection): the command Claude Code's
// PreToolUse hook runs. It reads Claude's hook JSON on stdin, asks the
// session host whether to allow the tool call, and prints Claude's expected
// hookSpecificOutput JSON on stdout.
//
// This mirrors relayLLM's cmd/hook/main.go body-for-body (same stdin shape,
// same POST body field names, same fail-open-on-any-local-error posture,
// same stdout contract) with the two changes C6 makes: the route is
// POST /permission (not /api/permission) on RELAY_SESSIONS_HOOK_SOCKET (not
// relayLLM's internal socket), the session id comes from RELAY_SESSION_ID
// (not RELAY_LLM_SESSION_ID), and there is no bearer token — the host admits
// the call by C3 process-ancestry membership against the session's root
// instead (see internal/sessions/hostapi's HookServer).
//
// One piece of relayLLM's hook is deliberately not ported yet:
// RELAY_LLM_HEADLESS=true's auto-approve branch (no human in the loop,
// every tool call allowed without a round trip). C6 says this package's
// stdout contract must stay identical to today's cmd/hook, which includes
// that branch — whichever unit adds real headless sessions (R-S7b) must
// re-add it here, or headless sessions will silently stop auto-approving.
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
// with no output, same as relayLLM's hook when RELAY_LLM_HOOK_SOCKET is
// unset, so Claude Code's own built-in checks decide instead.
const EnvHookSocket = "RELAY_SESSIONS_HOOK_SOCKET"

// EnvSessionID is C6's replacement for relayLLM's RELAY_LLM_SESSION_ID.
const EnvSessionID = "RELAY_SESSION_ID"

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
// which is always 0: every failure mode here is fail-open (Claude Code falls
// back to its own built-in permission checks), matching relayLLM's hook.
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
	sessionID := d.Environ(EnvSessionID)

	if hookSocket == "" {
		return 0
	}
	if d.Dial == nil {
		d.Dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", hookSocket)
		}
	}

	raw, err := io.ReadAll(d.Stdin)
	if err != nil {
		return 0
	}
	var in claudeHookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return 0
	}

	// Interactive UI tools have no side effects a permission gate needs to
	// see — skip the round trip entirely, same as relayLLM's hook.
	switch in.ToolName {
	case "ExitPlanMode", "AskUserQuestion":
		return 0
	}

	body, err := json.Marshal(permissionRequestBody{
		SessionID: sessionID,
		ToolName:  in.ToolName,
		ToolInput: string(in.ToolInput),
		ToolUseID: in.ToolUseID,
	})
	if err != nil {
		return 0
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx) },
		},
		Timeout: 120 * time.Second,
	}

	req, err := http.NewRequest(http.MethodPost, "http://relay-sessions.localsocket/permission", bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header: C6 admits this call by C3 membership on the
	// accepted connection's peer, never a bearer the hook process could leak.

	resp, err := client.Do(req)
	if err != nil {
		return 0 // network error, including a membership refusal — fail open
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var result permissionResponseBody
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0
	}

	out, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       result.Decision,
			"permissionDecisionReason": result.Reason,
		},
	})
	if err != nil {
		return 0
	}
	fmt.Fprintln(d.Stdout, string(out))
	return 0
}
