package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
)

const (
	toolsID      = "acme-tools-through-bridge"
	auditedID    = "tool-call-audited"
	acmeMcp      = "macmcp"
	allowedTool  = "mail_list_accounts"
	deniedTool   = "contacts_list"
	accessDenied = "access denied"
)

// execOut is one shell line run inside a live session.
type execOut struct {
	Out  string
	Exit int
	Err  string // transport failure; Out and Exit are then partial
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func toolCallLine(relayBin, tool string) string {
	return shellQuote(relayBin) + " mcp call --tool " + tool + " --args '{}'"
}

func sessionExec(ctx context.Context, s *liveSession, line string) execOut {
	out, code, err := s.Exec(ctx, line)
	o := execOut{Out: out, Exit: code}
	if err != nil {
		o.Err = err.Error()
	}
	return o
}

// attachRefusal is shared by the journeys that attach a world-probe session
// in Acme: a refusal that says the world is not as expected is BLOCKED; any
// other refusal is the journey's failure.
func attachRefusal(id, reason string) (result, bool) {
	switch reason {
	case "":
		return result{}, false
	case bridge.SandboxReasonUnknownTemplate, bridge.SandboxReasonTemplateDenied,
		bridge.SandboxReasonInsideSession, bridge.SandboxReasonPeerConfined:
		return blocked(id, "attach refused "+reason), true
	}
	return result{id, stateFail, "attach refused " + reason}, true
}

func openAcmeProbe(ctx context.Context, e env, id string) (*liveSession, result, bool) {
	acme, err := e.World.project("acme")
	if err != nil {
		return nil, blocked(id, err.Error()), false
	}
	cwd, err := filepath.EvalSymlinks(acme.Folder)
	if err != nil {
		return nil, blocked(id, acme.Name+" folder missing"), false
	}
	s, reason, err := openSession(ctx, e, "world-probe", cwd)
	if err != nil {
		return nil, blocked(id, err.Error()), false
	}
	if res, refused := attachRefusal(id, reason); refused {
		return nil, res, false
	}
	return s, result{}, true
}

type toolsRun struct {
	List, Allowed, Denied execOut
}

func runToolsThroughBridge(ctx context.Context, e env) result {
	s, res, ok := openAcmeProbe(ctx, e, toolsID)
	if !ok {
		return res
	}
	defer s.Close(context.WithoutCancel(ctx))
	var r toolsRun
	r.List = sessionExec(ctx, s, shellQuote(e.RelayBin)+" mcp call --list")
	r.Allowed = sessionExec(ctx, s, toolCallLine(e.RelayBin, allowedTool))
	r.Denied = sessionExec(ctx, s, toolCallLine(e.RelayBin, deniedTool))
	return classifyToolsThroughBridge(r)
}

// parseToolList reads the TOOL/DESCRIPTION table relay mcp call --list
// prints; the echoed command line before it is skipped.
func parseToolList(out string) []string {
	var tools []string
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case !inTable:
			inTable = len(f) >= 2 && f[0] == "TOOL" && f[1] == "DESCRIPTION"
		case len(f) == 0 || strings.Contains(line, "tools available"):
			return tools
		default:
			tools = append(tools, f[0])
		}
	}
	return tools
}

func classifyToolsThroughBridge(r toolsRun) result {
	fail := func(d string) result { return result{toolsID, stateFail, d} }
	for _, x := range []execOut{r.List, r.Allowed, r.Denied} {
		if x.Err != "" {
			return blocked(toolsID, "session: "+x.Err)
		}
	}
	tools := parseToolList(r.List.Out)
	switch {
	case r.List.Exit != 0:
		return fail(fmt.Sprintf("mcp call --list exit %d", r.List.Exit))
	case len(tools) == 0:
		return fail("no tools listed in the probe session")
	}
	for _, t := range tools {
		if !strings.HasPrefix(t, "mail_") {
			return fail("tool " + t + " listed outside mail_*")
		}
	}
	switch {
	case r.Allowed.Exit != 0:
		return fail(fmt.Sprintf("%s exit %d", allowedTool, r.Allowed.Exit))
	case r.Denied.Exit == 0:
		return fail(deniedTool + " answered in the probe session")
	case !strings.Contains(r.Denied.Out, accessDenied):
		return fail(deniedTool + " failed without an access denial")
	}
	return result{toolsID, statePass, fmt.Sprintf("%d mail tools listed; %s answered; %s denied", len(tools), allowedTool, deniedTool)}
}

type auditedRun struct {
	SessionID, AcmeID string
	AcmeName          string
	Allowed, Denied   execOut
	Rows              []audit.AuditEvent // call_tool rows newer than the baseline
}

func runToolCallAudited(ctx context.Context, e env) result {
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(auditedID, err.Error())
	}
	baseline, err := callToolRows(ctx, e, 1, "")
	if err != nil {
		return blocked(auditedID, err.Error())
	}
	baselineID := ""
	if len(baseline) > 0 {
		baselineID = baseline[len(baseline)-1].ID
	}
	s, res, ok := openAcmeProbe(ctx, e, auditedID)
	if !ok {
		return res
	}
	r := auditedRun{SessionID: s.ID, AcmeID: acmeID, AcmeName: acme.Name}
	r.Allowed = sessionExec(ctx, s, toolCallLine(e.RelayBin, allowedTool))
	r.Denied = sessionExec(ctx, s, toolCallLine(e.RelayBin, deniedTool))
	s.Close(context.WithoutCancel(ctx))
	if r.Allowed.Err == "" && r.Denied.Err == "" && r.SessionID != "" {
		// Relay records a local call after it returns, off the caller's path.
		for deadline := time.Now().Add(5 * time.Second); ; {
			if r.Rows, err = callToolRows(ctx, e, 200, baselineID); err != nil {
				return blocked(auditedID, err.Error())
			}
			if len(sessionRows(r.Rows, r.SessionID)) >= 2 || time.Now().After(deadline) || ctx.Err() != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return classifyToolCallAudited(r)
}

// callToolRows answers the newest n project_session call_tool rows, oldest
// first, dropping every row up to and including after when it is set.
func callToolRows(ctx context.Context, e env, n int, after string) ([]audit.AuditEvent, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", "call_tool", "--kind", audit.AuditActorProjectSession,
		"--json", "--tail", fmt.Sprint(n)).Output()
	if err != nil {
		return nil, errors.New("relay audit call_tool failed")
	}
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, errors.New("relay audit printed unreadable JSON")
		}
		rows = append(rows, row)
	}
	if after == "" {
		return rows, nil
	}
	for i, row := range rows {
		if row.ID == after {
			return rows[i+1:], nil
		}
	}
	return nil, errors.New("the baseline call_tool row fell out of the audit window")
}

func sessionRows(rows []audit.AuditEvent, sessionID string) []audit.AuditEvent {
	var out []audit.AuditEvent
	for _, r := range rows {
		if r.Actor.SessionID == sessionID {
			out = append(out, r)
		}
	}
	return out
}

func classifyToolCallAudited(r auditedRun) result {
	fail := func(d string) result { return result{auditedID, stateFail, d} }
	for _, x := range []execOut{r.Allowed, r.Denied} {
		if x.Err != "" {
			return blocked(auditedID, "session: "+x.Err)
		}
	}
	if r.SessionID == "" {
		return fail("attach answer named no session")
	}
	rows := sessionRows(r.Rows, r.SessionID)
	for _, want := range []struct{ tool, outcome string }{{allowedTool, audit.AuditOutcomeOK}, {deniedTool, audit.AuditOutcomeDenied}} {
		var match []audit.AuditEvent
		for _, row := range rows {
			if row.Tool == want.tool {
				match = append(match, row)
			}
		}
		if len(match) != 1 {
			return fail(fmt.Sprintf("%d call_tool rows for %s in the session, want 1", len(match), want.tool))
		}
		row := match[0]
		switch {
		case row.Phase != "":
			return fail(fmt.Sprintf("%s row has phase %q, want none for a local call", want.tool, row.Phase))
		case row.Actor.Kind != audit.AuditActorProjectSession:
			return fail(fmt.Sprintf("%s row actor %q", want.tool, row.Actor.Kind))
		case row.Actor.ProjectID != r.AcmeID:
			return fail(want.tool + " row is not for " + r.AcmeName)
		case row.McpID != acmeMcp:
			return fail(fmt.Sprintf("%s row mcp %q, want %s", want.tool, row.McpID, acmeMcp))
		case row.Outcome != want.outcome:
			return fail(fmt.Sprintf("%s row outcome %q, want %s", want.tool, row.Outcome, want.outcome))
		}
	}
	if len(rows) != 2 {
		return fail(fmt.Sprintf("%d call_tool rows in the session, want 2", len(rows)))
	}
	return result{auditedID, statePass, "one row per call: " + allowedTool + " ok, " + deniedTool + " denied"}
}
