package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

const (
	chatID     = "session-chat-lifecycle"
	terminalID = "terminal-lifecycle"
	modelID    = "model-list-and-completion"
	disabledID = "disabled-tool-refused"
	narrowID   = "grant-narrowing-live"
	svcStartID = "service-start-stop"
	svcCrashID = "service-restart-on-crash"
	hostRstID  = "session-host-restart"

	liveEditTimeout = 10 * time.Second
	messageTimeout  = 60 * time.Second
	disabledRefusal = "is disabled for this token"
	narrowRefusal   = "no tool named '" + probeTool + "' is available to this grant"
)

var screenJourneys = []journey{
	{staleID, []string{"projects", "grants"}, nil, phaseScreen, 30 * time.Second, runStaleDerivedEdit},
	{"context-number-resave", []string{"projects", "grants", "audit"}, nil, phaseScreen, 30 * time.Second, runContextNumberResave},
	{chatID, []string{"sessions", "audit"}, []string{"project:acme"}, phaseScreen, 90 * time.Second, runChatLifecycle},
	{terminalID, []string{"sessions", "templates", "audit"}, []string{"project:acme"}, phaseScreen, 30 * time.Second, runTerminalLifecycle},
	{extraArgsID, []string{"sessions", "templates"}, nil, phaseScreen, 60 * time.Second, runTerminalExtraArgs},
	{modelID, []string{"models", "sessions", "audit"}, []string{"project:acme"}, phaseScreen, 90 * time.Second, runModelCompletion},
	{chatResumeID, []string{"sessions", "audit"}, []string{"project:acme"}, phaseScreen, 180 * time.Second, runChatResume},
	{chatToolSearchID, []string{"sessions", "audit"}, nil, phaseScreen, 240 * time.Second, runChatToolSearch},
	{agentStateID, []string{"sessions"}, []string{"project:acme"}, phaseScreen, 180 * time.Second, runAgentState},
	{codexID, []string{"sessions"}, []string{"project:acme"}, phaseScreen, 180 * time.Second, runCodex},
	{dropInID, []string{"sessions"}, []string{"project:acme"}, phaseScreen, 300 * time.Second, runDropIn},
	{dropInHostID, []string{"sessions"}, nil, phaseScreen, 240 * time.Second, runDropInHost},
	{dropInRefusedID, []string{"sessions"}, []string{"project:acme"}, phaseScreen, 180 * time.Second, runDropInToolRefused},
	{cosID, []string{"sessions", "audit", "credentials"}, []string{"project:acme"}, phaseScreen, 240 * time.Second, runChiefOfStaffSend},
	{cosStartID, []string{"sessions", "audit", "credentials"}, []string{"project:acme"}, phaseScreen, 240 * time.Second, runCosStart},
	{disabledID, []string{"mcps", "grants"}, nil, phaseScreen, 45 * time.Second, runDisabledTool},
	{narrowID, []string{"grants", "projects", "sandbox"}, nil, phaseScreen, 45 * time.Second, runGrantNarrowing},
	{svcStartID, []string{"services"}, nil, phaseScreen, 30 * time.Second, runServiceStartStop},
	{svcCrashID, []string{"services"}, nil, phaseScreen, 30 * time.Second, runServiceRestart},
	{settingsWindowID, []string{"services", "tray", "settings-ui"}, nil, phaseScreen, 60 * time.Second, runSettingsWindow},
	{slowRouteID, []string{"credentials", "sessions", "hosts"}, nil, phaseScreen, gateTimeout, runSlowRouteKeepalive},
	// Last: it restarts the session host under every live session.
	{hostRstID, []string{"services", "sessions"}, nil, phaseScreen, 30 * time.Second, runSessionHostRestart},
}

// screenCreds answers P4 for launches, which are class execute, and the run
// credential for everything else.
func screenCreds(e env, id string) (launch, run string, res result, ok bool) {
	if run, res, ok = runCredential(e, id); !ok {
		return "", "", res, false
	}
	launch, err := readCredential(e.CredentialFile)
	if err != nil {
		return "", "", blocked(id, "P4 execute credential: "+err.Error()), false
	}
	return launch, run, result{}, true
}

func jsonBody(v any) []byte { b, _ := json.Marshal(v); return b }

func verifyModel() string { return envOr("RELAY_VERIFY_MODEL", "Chat") }

// pickModel prefers an exact value over one that only ends in /want.
func pickModel(body []byte, want string) string {
	var v struct {
		Models []struct {
			Value string `json:"value"`
		} `json:"models"`
	}
	_ = json.Unmarshal(body, &v)
	suffix := ""
	for _, m := range v.Models {
		switch {
		case m.Value == want:
			return m.Value
		case suffix == "" && strings.HasSuffix(m.Value, "/"+want):
			suffix = m.Value
		}
	}
	return suffix
}

func modelMatches(rowModel, picked, want string) bool {
	return rowModel != "" && (rowModel == picked || rowModel == want || strings.HasSuffix(rowModel, "/"+want))
}

// listedIDs reads {"<key>":[{"id":…}]}, the shape of relay-sessions' lists.
func listedIDs(body []byte, key string) []string {
	var v map[string][]struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &v)
	ids := make([]string, 0, len(v[key]))
	for _, x := range v[key] {
		ids = append(ids, x.ID)
	}
	return ids
}

// modelCallRows answers the model_call rows for project since the given
// time, oldest first. A time bound stands in for a baseline row id, which
// other services' calls could push out of the window.
func modelCallRows(ctx context.Context, e env, since time.Time, project string) ([]audit.AuditEvent, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", audit.AuditEventModelCall, "--json", "--tail", "200").Output()
	if err != nil {
		return nil, errors.New("relay audit --event model_call failed")
	}
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		var row audit.AuditEvent
		if len(line) == 0 {
			continue
		}
		if json.Unmarshal(line, &row) != nil {
			return nil, errors.New("relay audit printed unreadable JSON")
		}
		if !row.TS.Before(since) && row.Actor.ProjectID == project {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type chatRun struct {
	Models           frontendResponse
	Want, Model      string
	Project          string // the world project's name, for details
	Create           frontendResponse
	SessionID        string
	Message          frontendResponse
	Reply            string
	Delete, List     frontendResponse
	LaunchRow        *audit.AuditEvent
	ModelRows        []audit.AuditEvent
	RowsErr          error
	StillListedAfter bool
}

// runChat launches a chat session in Acme with P4, sends one message,
// deletes the session and reads the audit back. Everything after the launch
// uses the run credential, whose proxy class reaches relay-sessions.
func runChat(ctx context.Context, e env, id string) (chatRun, result, bool) {
	launch, run, res, ok := screenCreds(e, id)
	if !ok {
		return chatRun{}, res, false
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return chatRun{}, blocked(id, err.Error()), false
	}
	start := time.Now().Add(-time.Second)
	r := chatRun{Want: verifyModel(), Project: acme.Name}
	r.Models = frontendDo(ctx, e, run, http.MethodGet, "/api/models", nil)
	if r.Models.Status == http.StatusOK {
		r.Model = pickModel(r.Models.Body, r.Want)
	}
	if r.Model == "" {
		return r, result{}, true
	}
	body := jsonBody(map[string]string{"projectId": acmeID, "name": "verify-" + e.Nonce + "-chat", "model": r.Model})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return r, result{}, true
	}
	r.Message = frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/sessions/"+r.SessionID+"/message",
		jsonBody(map[string]string{"text": "Reply with the single word: ready"}), messageTimeout)
	var reply struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(r.Message.Body, &reply)
	r.Reply = reply.Text
	cleanup := context.WithoutCancel(ctx)
	r.Delete = frontendDo(cleanup, e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
	r.List = frontendDo(cleanup, e, run, http.MethodGet, "/api/sessions", nil)
	r.StillListedAfter = slices.Contains(listedIDs(r.List.Body, "sessions"), r.SessionID)
	r.LaunchRow = auditRow(ctx, e, r.SessionID)
	// Relay records a model call after it returns, off the caller's path.
	for deadline := time.Now().Add(5 * time.Second); ; {
		r.ModelRows, r.RowsErr = modelCallRows(ctx, e, start, acmeID)
		if r.RowsErr != nil || okModelRow(r) != nil || r.Message.Status != http.StatusOK || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return r, result{}, true
}

func okModelRow(r chatRun) *audit.AuditEvent {
	for i := len(r.ModelRows) - 1; i >= 0; i-- {
		row := r.ModelRows[i]
		if row.Outcome == audit.AuditOutcomeOK && (modelMatches(row.Model, r.Model, r.Want) || modelMatches(row.ModelCanonical, r.Model, r.Want)) {
			return &r.ModelRows[i]
		}
	}
	return nil
}

func hostUnavailable(rows []audit.AuditEvent) bool {
	return slices.ContainsFunc(rows, func(row audit.AuditEvent) bool {
		return row.Outcome == "host_unavailable" || row.Status == http.StatusServiceUnavailable
	})
}

func classifyChat(id string, r chatRun) (result, bool) {
	fail := func(d string) (result, bool) { return result{id, stateFail, d}, true }
	switch {
	case r.Models.Status == 0:
		return blocked(id, "frontend socket unreachable"), true
	case r.Models.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)"), true
	case r.Models.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/models status %d: %s", r.Models.Status, r.Models.Error))
	case r.Model == "":
		return blocked(id, fmt.Sprintf("model %q not in GET /api/models; set RELAY_VERIFY_MODEL", r.Want)), true
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return launchRefusal(id, "/api/sessions", r.Project, r.Create), true
	case hostUnavailable(r.ModelRows):
		return blocked(id, "the model host answered 503"), true
	case r.Message.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the message route"), true
	case r.Message.TimedOut:
		return fail("no reply within 60 s")
	case r.Message.Status != http.StatusOK:
		return fail(fmt.Sprintf("message status %d: %s", r.Message.Status, r.Message.Error))
	case strings.TrimSpace(r.Reply) == "":
		return fail("200 with an empty reply")
	case r.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: session %s may still be running", r.Delete.Status, r.SessionID))
	case r.List.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/sessions status %d", r.List.Status))
	case r.StillListedAfter:
		return fail("session still listed after DELETE")
	}
	return result{}, false
}

func runChatLifecycle(ctx context.Context, e env) result {
	r, res, ok := runChat(ctx, e, chatID)
	if !ok {
		return res
	}
	return classifyChatLifecycle(r)
}

func classifyChatLifecycle(r chatRun) result {
	if res, bad := classifyChat(chatID, r); bad {
		return res
	}
	switch {
	case r.LaunchRow == nil:
		return result{chatID, stateFail, "no session_launch row"}
	case r.LaunchRow.Outcome != audit.AuditOutcomeOK:
		return result{chatID, stateFail, fmt.Sprintf("session_launch outcome %q", r.LaunchRow.Outcome)}
	}
	return result{chatID, statePass, "chat launched in " + r.Project + " with " + r.Model + ", answered, deleted and gone from the list; launch audited"}
}

func runModelCompletion(ctx context.Context, e env) result {
	r, res, ok := runChat(ctx, e, modelID)
	if !ok {
		return res
	}
	return classifyModelCompletion(r)
}

func classifyModelCompletion(r chatRun) result {
	if res, bad := classifyChat(modelID, r); bad {
		return res
	}
	switch {
	case r.RowsErr != nil:
		return blocked(modelID, r.RowsErr.Error())
	case okModelRow(r) == nil:
		return result{modelID, stateFail, fmt.Sprintf("no ok model_call row for %s in %s", r.Model, r.Project)}
	}
	return result{modelID, statePass, r.Model + " listed; one turn answered through the model endpoint; model_call recorded ok; session stopped"}
}

func launchRefusal(id, path, project string, r frontendResponse) result {
	switch r.Status {
	case 0:
		return blocked(id, "frontend socket unreachable")
	case http.StatusUnauthorized:
		return blocked(id, "P4 execute credential refused (401)")
	case http.StatusForbidden:
		return blocked(id, "launch refused for "+project+": "+r.Error)
	case http.StatusCreated:
		return result{id, stateFail, "201 from POST " + path + " without an id"}
	}
	return result{id, stateFail, fmt.Sprintf("POST %s status %d: %s", path, r.Status, r.Error)}
}

type terminalRun struct {
	Project                   string
	Create                    frontendResponse
	TermID                    string
	ListBefore, Log           frontendResponse
	Delete, ListAfter         frontendResponse
	ListedBefore, ListedAfter bool
	Row                       *audit.AuditEvent
}

func runTerminalLifecycle(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, terminalID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(terminalID, err.Error())
	}
	r := terminalRun{Project: acme.Name}
	body := jsonBody(map[string]any{"templateId": "world-probe", "projectId": acmeID, "name": "verify-" + e.Nonce + "-term", "cols": 120, "rows": 40})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/terminals", body, 30*time.Second)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.TermID = created.TerminalID; r.Create.Status != http.StatusCreated || r.TermID == "" {
		return classifyTerminalLifecycle(r)
	}
	r.ListBefore = frontendDo(ctx, e, run, http.MethodGet, "/api/terminals", nil)
	r.ListedBefore = slices.Contains(listedIDs(r.ListBefore.Body, "terminals"), r.TermID)
	for deadline := time.Now().Add(5 * time.Second); ; {
		r.Log = frontendDo(ctx, e, run, http.MethodGet, "/api/terminals/"+r.TermID+"/log", nil)
		if r.Log.Status == http.StatusOK && len(r.Log.Body) > 0 || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cleanup := context.WithoutCancel(ctx)
	r.Delete = frontendDo(cleanup, e, run, http.MethodDelete, "/api/terminals/"+r.TermID, nil)
	r.ListAfter = frontendDo(cleanup, e, run, http.MethodGet, "/api/terminals", nil)
	r.ListedAfter = slices.Contains(listedIDs(r.ListAfter.Body, "terminals"), r.TermID)
	r.Row = auditRow(ctx, e, r.TermID)
	return classifyTerminalLifecycle(r)
}

func classifyTerminalLifecycle(r terminalRun) result {
	const id = terminalID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Create.Status != http.StatusCreated || r.TermID == "":
		return launchRefusal(id, "/api/terminals", r.Project, r.Create)
	case r.ListBefore.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)")
	case !r.ListedBefore:
		return fail("terminal not listed after launch")
	case r.Log.Status != http.StatusOK:
		return fail(fmt.Sprintf("log status %d within 5 s", r.Log.Status))
	case len(r.Log.Body) == 0:
		return fail("log empty after 5 s")
	case r.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: terminal %s may still be running", r.Delete.Status, r.TermID))
	case r.ListAfter.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/terminals status %d", r.ListAfter.Status))
	case r.ListedAfter:
		return fail("terminal still listed after DELETE")
	case r.Row == nil:
		return fail("no session_launch row")
	case r.Row.Outcome != audit.AuditOutcomeOK:
		return fail(fmt.Sprintf("session_launch outcome %q", r.Row.Outcome))
	}
	return result{id, statePass, "world-probe terminal launched in " + r.Project + ", listed, logged, deleted and gone; launch audited"}
}

// probeView is what a Verify Grant session sees of the probe MCP.
type probeView struct{ List, Call execOut }

func (p probeView) listed() bool { return slices.Contains(parseToolList(p.List.Out), probeTool) }

func viewProbe(ctx context.Context, e env, s *liveSession) probeView {
	return probeView{
		List: sessionExec(ctx, s, shellQuote(e.RelayBin)+" mcp call --list"),
		Call: sessionExec(ctx, s, toolCallLine(e.RelayBin, probeTool)),
	}
}

func openGrantSession(ctx context.Context, e env, id string) (*liveSession, string, result, bool) {
	token, res, ok := runCredential(e, id)
	switch {
	case !ok:
		return nil, "", res, false
	case e.Run.ProbeMCP == "":
		return nil, "", blocked(id, "no probe MCP: "+mcpPosID+" did not pass"), false
	case e.Run.GrantProjectID == "" || e.Run.GrantProjectPath == "":
		return nil, "", blocked(id, "no Verify Grant project: "+grantPosID+" did not pass"), false
	}
	s, reason, err := openSession(ctx, e, "world-probe", e.Run.GrantProjectPath)
	if err != nil {
		return nil, "", blocked(id, err.Error()), false
	}
	if res, refused := attachRefusal(id, reason); refused {
		return nil, "", res, false
	}
	return s, token, result{}, true
}

func putProject(ctx context.Context, e env, token, projectID string, body any) frontendResponse {
	return frontendDoTimeout(ctx, e, token, http.MethodPut, "/api/projects/"+projectID, jsonBody(body), liveEditTimeout)
}

// probeBaseline judges the view before the change: without the tool
// answering there, the change under test has nothing to take away.
func probeBaseline(id string, before probeView, others ...execOut) (result, bool) {
	for _, x := range append([]execOut{before.List, before.Call}, others...) {
		if x.Err != "" {
			return blocked(id, "session: "+x.Err), true
		}
	}
	switch {
	case !before.listed():
		return blocked(id, "fixture: "+probeTool+" not listed in Verify Grant before the change: "+lastLine(before.List.Out)), true
	case before.Call.Exit != 0:
		return blocked(id, "fixture: "+probeTool+" refused in Verify Grant before the change: "+lastLine(before.Call.Out)), true
	}
	return result{}, false
}

// callRefusal judges an ungated call's answer. One held past its bound is
// waiting on a presence prompt it should never have raised.
func callRefusal(id, what string, r frontendResponse) (result, bool) {
	switch {
	case r.TimedOut:
		return result{id, stateFail, what + " held past 10 s; a presence prompt?"}, true
	case r.Status == 0:
		return blocked(id, "frontend socket unreachable"), true
	case r.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)"), true
	case r.Status != http.StatusOK:
		return result{id, stateFail, fmt.Sprintf("%s status %d: %s", what, r.Status, r.Error)}, true
	}
	return result{}, false
}

type disabledRun struct {
	Before, After    probeView
	Disable, Restore frontendResponse
	Restored         execOut
}

func runDisabledTool(ctx context.Context, e env) result {
	s, token, res, ok := openGrantSession(ctx, e, disabledID)
	if !ok {
		return res
	}
	defer s.Close(context.WithoutCancel(ctx))
	var r disabledRun
	if r.Before = viewProbe(ctx, e, s); !r.Before.listed() || r.Before.Call.Exit != 0 {
		return classifyDisabledTool(r)
	}
	r.Disable = putProject(ctx, e, token, e.Run.GrantProjectID, map[string]any{"disabled_tools": map[string][]string{e.Run.ProbeMCP: {probeTool}}})
	if r.Disable.Status == http.StatusOK {
		r.After = viewProbe(ctx, e, s)
	}
	r.Restore = putProject(context.WithoutCancel(ctx), e, token, e.Run.GrantProjectID, map[string]any{"disabled_tools": map[string][]string{}})
	if r.Restore.Status == http.StatusOK {
		r.Restored = sessionExec(ctx, s, shellQuote(e.RelayBin)+" mcp call --list")
	}
	return classifyDisabledTool(r)
}

func classifyDisabledTool(r disabledRun) result {
	const id = disabledID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, bad := probeBaseline(id, r.Before, r.After.List, r.After.Call, r.Restored); bad {
		return res
	}
	if res, bad := callRefusal(id, "disabling "+probeTool, r.Disable); bad {
		return res
	}
	switch {
	case r.After.listed():
		return fail(probeTool + " still listed after it was disabled")
	case r.After.Call.Exit == 0:
		return fail(probeTool + " answered after it was disabled")
	case !strings.Contains(r.After.Call.Out, disabledRefusal):
		return fail("refused, but not as disabled: " + lastLine(r.After.Call.Out))
	}
	if res, bad := callRefusal(id, "restoring disabled_tools", r.Restore); bad {
		return res
	}
	if !slices.Contains(parseToolList(r.Restored.Out), probeTool) {
		return fail(probeTool + " not listed again after disabled_tools was cleared")
	}
	return result{id, statePass, probeTool + " disabled without a prompt, dropped from the live session's list and refused; restored"}
}

type narrowRun struct {
	Before, After probeView
	Narrow        frontendResponse
}

func runGrantNarrowing(ctx context.Context, e env) result {
	s, token, res, ok := openGrantSession(ctx, e, narrowID)
	if !ok {
		return res
	}
	defer s.Close(context.WithoutCancel(ctx))
	var r narrowRun
	if r.Before = viewProbe(ctx, e, s); r.Before.listed() && r.Before.Call.Exit == 0 {
		r.Narrow = putProject(ctx, e, token, e.Run.GrantProjectID, map[string]any{"allowed_mcp_ids": []string{}})
		if r.Narrow.Status == http.StatusOK {
			r.After = viewProbe(ctx, e, s)
		}
	}
	return classifyGrantNarrowing(r)
}

func classifyGrantNarrowing(r narrowRun) result {
	const id = narrowID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, bad := probeBaseline(id, r.Before, r.After.List, r.After.Call); bad {
		return res
	}
	if res, bad := callRefusal(id, "narrowing allowed_mcp_ids", r.Narrow); bad {
		return res
	}
	switch tools := parseToolList(r.After.List.Out); {
	case r.After.List.Exit != 0:
		return fail(fmt.Sprintf("mcp call --list exit %d after narrowing", r.After.List.Exit))
	case len(tools) > 0:
		return fail(fmt.Sprintf("%d tools still listed after narrowing to no MCPs", len(tools)))
	case r.After.Call.Exit == 0:
		return fail(probeTool + " answered after its MCP was removed from the grant")
	case !strings.Contains(r.After.Call.Out, narrowRefusal):
		return fail("refused, but not as outside the grant: " + lastLine(r.After.Call.Out))
	}
	return result{id, statePass, "narrowed without a prompt; the live session lists no tools and " + probeTool + " is refused: " + lastLine(r.After.Call.Out)}
}

// svcView is one poll of a service: the STATE relay service list renders
// from the same statuses, and the processes whose argv matches the service's
// pgrep pattern.
type svcView struct {
	State   string
	Attempt int
	PIDs    []int
	Err     error
}

func serviceView(ctx context.Context, e env, svcID, pidPattern string) svcView {
	type status struct {
		Phase        string `json:"phase"`
		Attempt      int    `json:"attempt"`
		LastExitCode int    `json:"last_exit_code"`
	}
	r, err := adminRead[struct {
		Statuses map[string]status `json:"statuses"`
	}](ctx, e, "service.list")
	if err != nil {
		return svcView{Err: fmt.Errorf("service.list: %w", err)}
	}
	v := svcView{State: "-"}
	switch st, ok := r.Statuses[svcID]; {
	case !ok:
	case st.Phase == "running":
		v.State = "running"
	case st.Phase == "restarting":
		v.State, v.Attempt = fmt.Sprintf("restarting (attempt %d)", st.Attempt), st.Attempt
	case st.Phase == "failed":
		v.State = fmt.Sprintf("failed (exit %d)", st.LastExitCode)
	}
	out, err := exec.CommandContext(ctx, "pgrep", "-f", pidPattern).Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		v.Err = errors.New("pgrep failed")
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			v.PIDs = append(v.PIDs, pid)
		}
	}
	return v
}

// Each pgrep pattern opens with a bracket so it cannot match a shell that
// quotes it.
func crashPIDPattern(svcID string) string {
	return "[c]rash-" + strings.TrimPrefix(svcID, crashPrefix) + `\.env`
}

const sessionHostPIDPattern = "[r]elay-sessions service"

func pollService(ctx context.Context, e env, svcID, pidPattern string, within time.Duration, done func(svcView) bool, seen func(svcView)) (svcView, bool) {
	for deadline := time.Now().Add(within); ; {
		v := serviceView(ctx, e, svcID, pidPattern)
		if seen != nil {
			seen(v)
		}
		if v.Err != nil || done(v) {
			return v, v.Err == nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return v, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func isUp(v svcView) bool   { return v.State == "running" && len(v.PIDs) > 0 }
func isDown(v svcView) bool { return v.State == "-" && len(v.PIDs) == 0 }

func serviceAction(ctx context.Context, e env, token, svcID, action string) frontendResponse {
	return frontendDo(ctx, e, token, http.MethodPost, "/api/services/"+svcID+"/"+action, nil)
}

func serviceFixture(e env, id string) (token, svcID string, res result, ok bool) {
	if token, res, ok = runCredential(e, id); !ok {
		return "", "", res, false
	}
	if e.Run.CrashService == "" {
		return "", "", blocked(id, "no crash service: "+servicePosID+" did not pass"), false
	}
	return token, e.Run.CrashService, result{}, true
}

type startStopRun struct {
	Start, Stop frontendResponse
	Started     svcView
	Up          bool
	Stopped     svcView
	Down        bool
}

func runServiceStartStop(ctx context.Context, e env) result {
	token, svcID, res, ok := serviceFixture(e, svcStartID)
	if !ok {
		return res
	}
	var r startStopRun
	if r.Start = serviceAction(ctx, e, token, svcID, "start"); r.Start.Status == http.StatusOK {
		r.Started, r.Up = pollService(ctx, e, svcID, crashPIDPattern(svcID), 5*time.Second, isUp, nil)
		r.Stop = serviceAction(context.WithoutCancel(ctx), e, token, svcID, "stop")
		if r.Stop.Status == http.StatusOK {
			r.Stopped, r.Down = pollService(ctx, e, svcID, crashPIDPattern(svcID), 5*time.Second, isDown, nil)
		}
	}
	return classifyServiceStartStop(r)
}

func classifyServiceStartStop(r startStopRun) result {
	const id = svcStartID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, bad := callRefusal(id, "start", r.Start); bad {
		return res
	}
	switch {
	case r.Started.Err != nil:
		return blocked(id, r.Started.Err.Error())
	case !r.Up:
		return fail(fmt.Sprintf("not running within 5 s: STATE %s, %d processes", r.Started.State, len(r.Started.PIDs)))
	}
	if res, bad := callRefusal(id, "stop", r.Stop); bad {
		return res
	}
	switch {
	case r.Stopped.Err != nil:
		return blocked(id, r.Stopped.Err.Error())
	case !r.Down:
		return fail(fmt.Sprintf("still up 5 s after stop: STATE %s, %d processes", r.Stopped.State, len(r.Stopped.PIDs)))
	}
	return result{id, statePass, "started: STATE running with its process; stopped: STATE - and no process"}
}

type crashRun struct {
	Start, Stop frontendResponse
	Up          svcView
	UpOK        bool
	Killed      int
	KillErr     error
	After       svcView
	Restarted   bool
	SawAttempt1 bool
}

func runServiceRestart(ctx context.Context, e env) result {
	token, svcID, res, ok := serviceFixture(e, svcCrashID)
	if !ok {
		return res
	}
	var r crashRun
	if r.Start = serviceAction(ctx, e, token, svcID, "start"); r.Start.Status == http.StatusOK {
		crashOnce(ctx, e, svcID, &r)
		r.Stop = serviceAction(context.WithoutCancel(ctx), e, token, svcID, "stop")
	}
	return classifyServiceRestart(r)
}

func crashOnce(ctx context.Context, e env, svcID string, r *crashRun) {
	if r.Up, r.UpOK = pollService(ctx, e, svcID, crashPIDPattern(svcID), 5*time.Second, isUp, nil); !r.UpOK {
		return
	}
	r.Killed = r.Up.PIDs[0]
	if r.KillErr = syscall.Kill(r.Killed, syscall.SIGKILL); r.KillErr != nil {
		return
	}
	r.After, r.Restarted = pollService(ctx, e, svcID, crashPIDPattern(svcID), 15*time.Second,
		func(v svcView) bool { return isUp(v) && !slices.Contains(v.PIDs, r.Killed) },
		func(v svcView) { r.SawAttempt1 = r.SawAttempt1 || v.Attempt == 1 })
}

func classifyServiceRestart(r crashRun) result {
	const id = svcCrashID
	fail := func(d string) result { return result{id, stateFail, d} }
	if res, bad := callRefusal(id, "start", r.Start); bad {
		return res
	}
	switch {
	case r.Up.Err != nil:
		return blocked(id, r.Up.Err.Error())
	case !r.UpOK:
		return fail(fmt.Sprintf("not running within 5 s: STATE %s", r.Up.State))
	case r.KillErr != nil:
		return blocked(id, fmt.Sprintf("could not SIGKILL pid %d: %v", r.Killed, r.KillErr))
	case r.After.Err != nil:
		return blocked(id, r.After.Err.Error())
	case !r.Restarted:
		return fail(fmt.Sprintf("not running with a new pid 15 s after SIGKILL: STATE %s", r.After.State))
	}
	seen := "not seen"
	if r.SawAttempt1 {
		seen = "seen"
	}
	detail := "running with a new pid after SIGKILL; \"restarting (attempt 1\" " + seen
	if r.Stop.Status != http.StatusOK {
		detail += fmt.Sprintf("; stop afterwards answered %d", r.Stop.Status)
	}
	return result{id, statePass, detail}
}

type hostRestartRun struct {
	Before     svcView
	RestartErr error
	After      svcView
	Back       bool
}

func runSessionHostRestart(ctx context.Context, e env) result {
	const svcID = config.RelaySessionsServiceID
	r := hostRestartRun{Before: serviceView(ctx, e, svcID, sessionHostPIDPattern)}
	if r.Before.Err == nil && isUp(r.Before) {
		_, r.RestartErr = adminOp[json.RawMessage](ctx, e, "service.restart", jsonBody(map[string]string{"id": svcID}))
		if r.RestartErr == nil {
			r.After, r.Back = pollService(ctx, e, svcID, sessionHostPIDPattern, 15*time.Second,
				func(v svcView) bool {
					return isUp(v) && !slices.ContainsFunc(v.PIDs, func(p int) bool { return slices.Contains(r.Before.PIDs, p) })
				}, nil)
		}
	}
	return classifySessionHostRestart(r)
}

func classifySessionHostRestart(r hostRestartRun) result {
	const id = hostRstID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Before.Err != nil:
		return blocked(id, r.Before.Err.Error())
	case !isUp(r.Before):
		return blocked(id, fmt.Sprintf("session host not running before the restart: STATE %s, %d processes", r.Before.State, len(r.Before.PIDs)))
	case r.RestartErr != nil:
		return fail("service.restart failed: " + r.RestartErr.Error())
	case r.After.Err != nil:
		return blocked(id, r.After.Err.Error())
	case !r.Back:
		return fail(fmt.Sprintf("not running with a new pid 15 s after service.restart: STATE %s, %d processes", r.After.State, len(r.After.PIDs)))
	}
	return result{id, statePass, fmt.Sprintf("service.restart answered; running with pid %v, was %v", r.After.PIDs, r.Before.PIDs)}
}
