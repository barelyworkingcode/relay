package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
)

var apiJourneys = []journey{
	{"blank-model-refused", []string{"sessions", "audit"}, phaseAPI, 30 * time.Second, runBlankModel},
	{"permission-mode-restart", []string{"sessions", "hosts"}, phaseAPI, 5 * time.Second, func(context.Context, env) result {
		return result{"permission-mode-restart", stateNotRun, "restart path runs only for SSH-host projects; local sessions answer resume_required"}
	}},
	{"oversized-launch-audit-capped", []string{"sessions", "sandbox", "audit"}, phaseAPI, 30 * time.Second, runOversized},
	{"acme-sandbox-reach", []string{"sandbox", "grants", "sessions"}, phaseAPI, 30 * time.Second, runReach},
	{"v1-conversion-refusal", []string{"projects"}, phaseAPI, 5 * time.Second, func(context.Context, env) result {
		return result{"v1-conversion-refusal", stateNotRun, "a local-to-remote conversion is a kind change, which the presence gate prompts for before it validates; the refusal is reachable only after a human approves, and no v1 MCP is registered here"}
	}},
	{toolsID, []string{"mcps", "grants", "sandbox"}, phaseAPI, 60 * time.Second, runToolsThroughBridge},
	{auditedID, []string{"audit", "mcps"}, phaseAPI, 60 * time.Second, runToolCallAudited},
}

const acmeName = "Acme Corp"

func blocked(id, detail string) result { return result{id, stateBlocked, detail} }

func runBlankModel(ctx context.Context, e env) result {
	const id = "blank-model-refused"
	fi, err := os.Stat(e.CredentialFile)
	if err != nil {
		return blocked(id, "credential file missing")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return blocked(id, "credential file not mode 0600")
	}
	raw, err := os.ReadFile(e.CredentialFile)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		return blocked(id, "credential file empty or unreadable")
	}
	acmeID, err := acmeProjectID(ctx, e)
	if err != nil {
		return blocked(id, err.Error())
	}
	name := "verify-" + e.Nonce
	body, _ := json.Marshal(map[string]string{"projectId": acmeID, "name": name, "model": ""})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay/api/sessions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		return blocked(id, "frontend socket unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var row *audit.AuditEvent
	if resp.StatusCode == http.StatusBadRequest {
		row = auditRow(ctx, e, name)
	}
	return classifyBlankModel(resp.StatusCode, respBody, row, name, acmeID)
}

func classifyBlankModel(status int, body []byte, row *audit.AuditEvent, name, acmeID string) result {
	const id = "blank-model-refused"
	fail := func(d string) result { return result{id, stateFail, d} }
	var b struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &b)
	want := fmt.Sprintf("chat session %q has no model; choose a model and try again", name)
	switch {
	case status == http.StatusUnauthorized:
		return blocked(id, "credential refused (401)")
	case status == http.StatusForbidden && strings.Contains(b.Error, `template "chat" is not available`):
		return blocked(id, "template chat not allowed for "+acmeName)
	case status/100 == 2:
		return fail(fmt.Sprintf("launch accepted; chat session %s left running in %s", name, acmeName))
	case status != http.StatusBadRequest:
		return fail(fmt.Sprintf("status %d, want 400", status))
	case b.Error != want:
		return fail("400 with an unexpected error message")
	case row == nil:
		return fail("no audit row")
	case row.Outcome != "error" || row.Error != want:
		return fail(fmt.Sprintf("audit row outcome %q or error mismatch", row.Outcome))
	case row.Actor.Kind != "control" || row.Actor.ProjectID != acmeID:
		return fail(fmt.Sprintf("audit actor kind %q, or project not %s", row.Actor.Kind, acmeName))
	}
	var args struct {
		SessionID   string `json:"session_id"`
		SessionKind string `json:"session_kind"`
	}
	if err := json.Unmarshal(row.Args, &args); err != nil || args.SessionKind != "chat" || args.SessionID != "" {
		return fail("audit args are not a chat launch without a session id")
	}
	return result{id, statePass, "refused with 400 and audited"}
}

func runOversized(ctx context.Context, e env) result {
	const id = "oversized-launch-audit-capped"
	cwd, err := filepath.EvalSymlinks(filepath.Join(e.WorldRoot, acmeName))
	if err != nil {
		return blocked(id, "Acme folder missing")
	}
	prefix := "verify-" + e.Nonce + "-"
	s, reason, err := sandboxAttach(ctx, e, prefix+strings.Repeat("A", 300000), cwd)
	if err != nil {
		return blocked(id, err.Error())
	}
	if s != nil {
		_ = s.Close()
	}
	var row *audit.AuditEvent
	n := 0
	if reason == bridge.SandboxReasonUnknownTemplate {
		if row = auditRow(ctx, e, prefix); row != nil {
			if n, err = auditLineBytes(ctx, e, row.ID); err != nil {
				return result{id, stateFail, "audit row not found on disk"}
			}
		}
	}
	return classifyOversized(reason, row, n)
}

func classifyOversized(reason string, row *audit.AuditEvent, rowBytes int) result {
	const id = "oversized-launch-audit-capped"
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case reason == "":
		return fail("attached, want template_unknown")
	case reason != bridge.SandboxReasonUnknownTemplate:
		return fail(fmt.Sprintf("refused %s, want template_unknown", reason))
	case row == nil:
		return fail("no audit row")
	case row.Actor.Kind != "operator" || row.Outcome != "denied":
		return fail(fmt.Sprintf("audit actor kind %q outcome %q", row.Actor.Kind, row.Outcome))
	case utf8.RuneCountInString(row.Error) > 257 || !strings.HasSuffix(row.Error, "…"):
		return fail(fmt.Sprintf("audit error uncapped, row %d bytes", rowBytes))
	case rowBytes > 4096:
		return fail(fmt.Sprintf("audit row %d bytes, over 4096", rowBytes))
	}
	return result{id, statePass, fmt.Sprintf("refused template_unknown; audit row %d bytes", rowBytes)}
}

func runReach(ctx context.Context, e env) result {
	const id = "acme-sandbox-reach"
	root, err := filepath.EvalSymlinks(e.WorldRoot)
	cwd, cwdErr := filepath.EvalSymlinks(filepath.Join(e.WorldRoot, acmeName))
	if err != nil || cwdErr != nil {
		return blocked(id, "Acme folder missing")
	}
	s, reason, err := sandboxAttach(ctx, e, "world-probe", cwd)
	if err != nil {
		return blocked(id, err.Error())
	}
	if s == nil {
		return classifyReach(reason, "", false, 0, nil)
	}
	defer func() { _ = s.Close() }()
	if s.SessionID == "" {
		return result{id, stateFail, "attach answer named no session"}
	}
	if err := json.NewEncoder(s).Encode(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte(reachScript(root))}); err != nil {
		return blocked(id, "could not send input")
	}
	var transcript strings.Builder
	exited, code := false, 0
	for !exited {
		line, err := s.r.ReadBytes('\n')
		if err != nil {
			break
		}
		var f bridge.StreamFrame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		switch f.Type {
		case bridge.StreamOutput:
			transcript.Write(f.Data)
		case bridge.StreamExit:
			exited, code = true, f.Code
		}
	}
	row := auditRow(ctx, e, s.SessionID)
	return classifyReach("", transcript.String(), exited, code, row)
}

// reachScript builds its markers with printf at run time so the terminal's
// echo of this input can never contain them.
func reachScript(root string) string {
	return "(: < PROJECT.md) && printf '%s_%s\\n' ACME OK || printf '%s_%s\\n' ACME DENIED\n" +
		"(: < \"" + filepath.Join(root, "Globex", "PROJECT.md") + "\") && printf '%s_%s\\n' GLOBEX OK || printf '%s_%s\\n' GLOBEX DENIED\n" +
		"exit\n"
}

func classifyReach(reason, transcript string, exited bool, exitCode int, row *audit.AuditEvent) result {
	const id = "acme-sandbox-reach"
	fail := func(d string) result { return result{id, stateFail, d} }
	switch reason {
	case "":
	case bridge.SandboxReasonUnknownTemplate, bridge.SandboxReasonTemplateDenied,
		bridge.SandboxReasonInsideSession, bridge.SandboxReasonPeerConfined:
		return blocked(id, "refused "+reason)
	default:
		return fail("refused " + reason)
	}
	switch {
	case strings.Contains(transcript, "GLOBEX_OK"):
		return fail("Globex readable from an Acme session")
	case !strings.Contains(transcript, "ACME_OK"):
		return fail("Acme not readable from its own session")
	case !strings.Contains(transcript, "GLOBEX_DENIED") || !strings.Contains(transcript, "Operation not permitted"):
		return fail("Globex read not denied by the sandbox")
	case !exited:
		return fail("no exit frame")
	case exitCode != 0:
		return fail(fmt.Sprintf("exit code %d", exitCode))
	case row == nil:
		return fail("no audit row")
	case row.Outcome != "ok":
		return fail(fmt.Sprintf("audit outcome %q", row.Outcome))
	}
	var args struct {
		Sandbox bool `json:"sandbox"`
	}
	if json.Unmarshal(row.Args, &args) != nil || !args.Sandbox {
		return fail("audit row not sandboxed")
	}
	return result{id, statePass, "Acme read, Globex denied, sandboxed and audited"}
}

func auditRow(ctx context.Context, e env, key string) *audit.AuditEvent {
	return auditEventRow(ctx, e, audit.AuditEventSessionLaunch, key)
}

// auditEventRow polls because relay records launches and exits
// asynchronously. It returns the newest row of event matching key.
func auditEventRow(ctx context.Context, e env, event, key string) *audit.AuditEvent {
	for deadline := time.Now().Add(5 * time.Second); ; {
		out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", event, "--grep", key, "--json", "--tail", "1").Output()
		line := bytes.TrimSpace(out)
		var row audit.AuditEvent
		if err == nil && len(line) > 0 && json.Unmarshal(line, &row) == nil {
			return &row
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// auditLineBytes sizes the row as stored in the audit file, which is what
// the cap protects; relay audit --json output is a rendering of it.
func auditLineBytes(ctx context.Context, e env, rowID string) (int, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--path").Output()
	if err != nil || rowID == "" {
		return 0, errors.New("no audit path or row id")
	}
	f, err := os.Open(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\n")
		var ev struct {
			ID string `json:"id"`
		}
		if bytes.Contains(line, []byte(rowID)) && json.Unmarshal(line, &ev) == nil && ev.ID == rowID {
			return len(line), nil
		}
		if err != nil {
			return 0, errors.New("row not in audit file")
		}
	}
}

func acmeProjectID(ctx context.Context, e env) (string, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "grant", "--json").Output()
	if err != nil {
		return "", errors.New("relay grant failed")
	}
	var projects []struct{ ID, Name string }
	if err := json.Unmarshal(out, &projects); err != nil {
		return "", errors.New("relay grant printed unreadable JSON")
	}
	for _, p := range projects {
		if p.Name == acmeName {
			return p.ID, nil
		}
	}
	return "", errors.New("no " + acmeName + " in relay grant")
}
