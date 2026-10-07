package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	cosStartID   = "cos-start"
	cosOutsideID = "cos-start-outside-root"
	cosStartPath = "/api/chief-of-staff/sessions"
	cosListWait  = 10 * time.Second
)

// cosStartBody is the start request the Chief of Staff grant sends.
func cosStartBody(projectID, folder, prompt, mode string) []byte {
	return jsonBody(map[string]string{"projectId": projectID, "folder": folder, "prompt": prompt, "model": agentModel, "mode": mode})
}

type cosStartResp struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	Origin    string `json:"origin"`
}

func parseCosStart(r frontendResponse) cosStartResp {
	var v cosStartResp
	_ = json.Unmarshal(r.Body, &v)
	return v
}

// listedOrigin finds the row for id under the key ("sessions" or "terminals")
// of a list body and returns its origin.
func listedOrigin(body []byte, key, id string) (origin string, found bool) {
	var v map[string][]struct {
		ID     string `json:"id"`
		Origin string `json:"origin"`
	}
	_ = json.Unmarshal(body, &v)
	for _, row := range v[key] {
		if row.ID == id {
			return row.Origin, true
		}
	}
	return "", false
}

// argvHasStart is true when one process-list line holds the terminal start's
// argv: the alias, the end-of-options marker and the prompt as one argument.
func argvHasStart(psOut, prompt string) bool {
	for _, line := range strings.Split(psOut, "\n") {
		if strings.Contains(line, "--model "+agentModel+" -- "+prompt) {
			return true
		}
	}
	return false
}

type cosStartRun struct {
	Project, Marker, TermPrompt string

	DialStatus int
	DialErr    string

	Create      frontendResponse
	SessionID   string
	JoinSeen    bool
	TurnWaitErr bool
	Frames      []cosFrame
	List        frontendResponse
	LaunchRows  []audit.AuditEvent // session_launch rows naming the session
	MsgRows     []audit.AuditEvent // session_message rows naming the session
	RowsErr     string
	Delete      frontendResponse

	TermCreate frontendResponse
	TermID     string
	TermList   frontendResponse
	TermDelete frontendResponse
	TermRows   []audit.AuditEvent
	PS         string
	PSErr      string
}

func runCosStart(ctx context.Context, e env) (out result) {
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(cosStartID, err.Error())
	}
	run, revoke, res, ok := mintCosStartCred(ctx, e)
	if !ok {
		return res
	}
	// Registered first so it runs last: the session deletes below use the credential.
	defer func() {
		if d := revoke(); d != "" && out.State == statePass {
			out = result{cosStartID, stateFail, d}
		}
	}()
	r := cosStartRun{Project: acme.Name, Marker: "verify-" + e.Nonce + "-start", TermPrompt: "verify-" + e.Nonce + "-term"}
	// Dialled before the start so the connection is ready when the id is known.
	obs, status, err := dialCos(ctx, e, run, false)
	r.DialStatus = status
	if err != nil {
		r.DialErr = err.Error()
		return classifyCosStart(r)
	}
	defer func() { _ = obs.conn.Close() }()

	// Deferred so a failed leg still removes what it started; the deletes ignore cancellation.
	defer func() {
		bg := context.WithoutCancel(ctx)
		if r.SessionID != "" {
			r.Delete = frontendDo(bg, e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
		}
		if r.TermID != "" {
			r.TermDelete = frontendDo(bg, e, run, http.MethodDelete, "/api/terminals/"+r.TermID, nil)
		}
		out = classifyCosStart(r)
	}()

	r.Create = cosRequest(ctx, e, run, true, http.MethodPost, cosStartPath,
		cosStartBody(acmeID, "", cosSendText(r.Marker), ""), 30*time.Second)
	if r.SessionID = parseCosStart(r.Create).SessionID; r.Create.Status == http.StatusCreated && r.SessionID != "" {
		id := r.SessionID
		_ = obs.send(map[string]string{"type": "join_session", "sessionId": id})
		if r.JoinSeen = obs.waitFor(ctx, cosWait, cosJoined(id, 1)); r.JoinSeen {
			// The only wait on model output.
			r.TurnWaitErr = !obs.waitFor(ctx, cosTurnWait, cosTurnFinished(id, r.Marker))
		}
		r.Frames = obs.snapshot()
		r.List = cosRequest(ctx, e, run, true, http.MethodGet, "/api/sessions", nil, frontendRequestTimeout)
		r.MsgRows, r.RowsErr = sessionMessageRows(ctx, e, id)
		if r.RowsErr == "" {
			var err error
			if r.LaunchRows, err = auditJSONRows(ctx, e, "--event", string(audit.AuditEventSessionLaunch), "--grep", id, "--json", "--tail", "20"); err != nil {
				r.RowsErr = err.Error()
			}
		}
	}

	r.TermCreate = cosRequest(ctx, e, run, true, http.MethodPost, cosStartPath,
		cosStartBody(acmeID, "", r.TermPrompt, "terminal"), 30*time.Second)
	if r.TermID = parseCosStart(r.TermCreate).SessionID; r.TermCreate.Status == http.StatusCreated && r.TermID != "" {
		// The row appearing is the signal; the process list is read once after it.
		for deadline := time.Now().Add(cosListWait); ; {
			r.TermList = frontendDo(ctx, e, run, http.MethodGet, "/api/terminals", nil)
			if _, found := listedOrigin(r.TermList.Body, "terminals", r.TermID); found || time.Now().After(deadline) || ctx.Err() != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		psOut, err := exec.CommandContext(ctx, "ps", "-axwwo", "command").Output()
		r.PS = string(psOut)
		if err != nil {
			r.PSErr = err.Error()
		}
	}
	return result{}
}

// mintCosStartCred mints the caller the start needs. Deliberate: the scope
// needs class proxy and the launch core (AuthorizeLaunch) needs class
// execute, and the run credential and P4 each hold only one of them. The
// credential lives 1h, so a run that dies before revoking it leaves nothing
// live past the hour. revoke returns a failure detail, or "" when it worked.
func mintCosStartCred(ctx context.Context, e env) (token string, revoke func() string, res result, ok bool) {
	name := "devbox-verify-cos-" + e.Nonce
	cli, d := gatedCLI(ctx, e, fmt.Sprintf("named %q", name),
		"credential", "mint", "--name", name, "--class", "proxy", "--class", "execute", "--ttl", "1h")
	id, token := parseMintOutput(cli.Stdout)
	if r, bad := positiveRefusal(cosStartID, cli.Stderr, d); bad {
		return "", nil, r, false
	}
	switch {
	case cli.Exit != 0:
		return "", nil, cliExitFail(cosStartID, "credential mint", cli), false
	case id == "" || !isToken(token):
		return "", nil, result{cosStartID, stateFail, "credential mint printed no id or no 64-hex token"}, false
	}
	revoke = func() string {
		bg := context.WithoutCancel(ctx)
		c, _ := gatedCLI(bg, e, fmt.Sprintf("%q", id), "credential", "revoke", "--id", id)
		if c.Exit != 0 {
			return fmt.Sprintf("revoking credential %s failed (exit %d: %s); it expires within the hour", id, c.Exit, lastLine(c.Stderr))
		}
		return ""
	}
	return token, revoke, result{}, true
}

func classifyCosStart(r cosStartRun) result {
	const id = cosStartID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	switch {
	case r.DialStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.DialErr != "" && r.DialStatus == 0:
		return blocked(id, "frontend socket unreachable: "+r.DialErr)
	case r.DialErr != "":
		return fail("GET /ws status %d: %s", r.DialStatus, r.DialErr)
	case r.Create.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the start route")
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return fail("headless start status %d, want 201: %s", r.Create.Status, r.Create.Error)
	}
	if s := parseCosStart(r.Create); s.Origin != cosOrigin || s.Mode != "headless" {
		return fail("headless start answered origin %q mode %q, want %q headless", s.Origin, s.Mode, cosOrigin)
	}
	sid := r.SessionID
	model := ""
	for _, f := range r.Frames {
		if f.Type == "llm_event" && f.SessionID == sid && f.InitModel != "" {
			model = f.InitModel
			break
		}
	}
	switch {
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case r.TurnWaitErr:
		return fail("no turn_done carrying the marker, then idle, after the headless start")
	case model != agentModelID:
		return fail("system/init model %q, want %q", model, agentModelID)
	case r.List.Status != http.StatusOK:
		return fail("scoped GET /api/sessions status %d, want 200", r.List.Status)
	case r.RowsErr != "":
		return blocked(id, r.RowsErr)
	}
	if o, found := listedOrigin(r.List.Body, "sessions", sid); !found || o != cosOrigin {
		return fail("GET /api/sessions row found=%v origin %q, want %q", found, o, cosOrigin)
	}
	if d := cosStartAudit(r); d != "" {
		return fail("%s", d)
	}
	return classifyCosStartTerminal(r)
}

// cosStartAudit checks the launch and message rows carry the origin.
func cosStartAudit(r cosStartRun) string {
	launched := false
	for _, row := range r.LaunchRows {
		if a := rowArgs(row); row.Event == string(audit.AuditEventSessionLaunch) && a.SessionID == r.SessionID {
			launched = true
			if a.Origin != cosOrigin || row.Outcome != audit.AuditOutcomeOK {
				return fmt.Sprintf("session_launch row outcome %q origin %q, want ok and %q", row.Outcome, a.Origin, cosOrigin)
			}
		}
	}
	if !launched {
		return "no session_launch row for the started session"
	}
	for _, phase := range []string{audit.AuditPhaseIntent, audit.AuditPhaseCompletion} {
		rows := messageRowsFor(r.MsgRows, r.SessionID, phase)
		if len(rows) != 1 {
			return fmt.Sprintf("%d %s session_message rows for the started session, want 1", len(rows), phase)
		}
		if o := rowArgs(rows[0]).Origin; o != cosOrigin {
			return fmt.Sprintf("session_message %s origin %q, want %q", phase, o, cosOrigin)
		}
	}
	return ""
}

func classifyCosStartTerminal(r cosStartRun) result {
	const id = cosStartID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	if r.Delete.Status/100 != 2 {
		return fail("DELETE of the headless session status %d: it may still be running", r.Delete.Status)
	}
	switch {
	case r.TermCreate.Status != http.StatusCreated || r.TermID == "":
		return fail("terminal start status %d, want 201: %s", r.TermCreate.Status, r.TermCreate.Error)
	case r.TermList.Status != http.StatusOK:
		return fail("GET /api/terminals status %d, want 200", r.TermList.Status)
	case r.TermDelete.Status/100 != 2:
		return fail("DELETE of the terminal status %d: it may still be running", r.TermDelete.Status)
	case r.PSErr != "":
		return fail("process list unreadable: %s", r.PSErr)
	}
	if s := parseCosStart(r.TermCreate); s.Origin != cosOrigin || s.Mode != "terminal" {
		return fail("terminal start answered origin %q mode %q, want %q terminal", s.Origin, s.Mode, cosOrigin)
	}
	if o, found := listedOrigin(r.TermList.Body, "terminals", r.TermID); !found || o != cosOrigin {
		return fail("GET /api/terminals row found=%v origin %q, want %q", found, o, cosOrigin)
	}
	if !argvHasStart(r.PS, r.TermPrompt) {
		return fail("no process with argv containing --model %s -- %s", agentModel, r.TermPrompt)
	}
	return result{id, statePass, fmt.Sprintf("Chief of Staff started a headless Haiku agent and a terminal in %s: both rows marked %s, launch and message audit rows carry it, init model %s, terminal argv holds --model %s -- <prompt>",
		r.Project, cosOrigin, agentModelID, agentModel)}
}

type cosOutsideRun struct {
	Project         string
	SymlinkName     string
	Dotdot, Symlink frontendResponse
	Since           time.Time
	Rows            []audit.AuditEvent
	RowsErr         string
}

func runCosStartOutsideRoot(ctx context.Context, e env) result {
	_, run, res, ok := screenCreds(e, cosOutsideID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(cosOutsideID, err.Error())
	}
	r := cosOutsideRun{Project: acme.Name, SymlinkName: "devboxverify-outside-" + e.Nonce}
	outside, err := os.MkdirTemp("", "devboxverify-outside-")
	if err != nil {
		return blocked(cosOutsideID, "cannot make a directory outside "+acme.Name+": "+err.Error())
	}
	defer func() { _ = os.RemoveAll(outside) }()
	link := filepath.Join(acme.Folder, r.SymlinkName)
	if err := os.Symlink(outside, link); err != nil {
		return blocked(cosOutsideID, "cannot place a symlink in "+acme.Name+": "+err.Error())
	}
	defer func() { _ = os.Remove(link) }()

	r.Since = time.Now().Add(-time.Second)
	r.Dotdot = cosRequest(ctx, e, run, true, http.MethodPost, cosStartPath, cosStartBody(acmeID, "../x", "must not start", ""), frontendRequestTimeout)
	r.Symlink = cosRequest(ctx, e, run, true, http.MethodPost, cosStartPath, cosStartBody(acmeID, r.SymlinkName, "must not start", ""), frontendRequestTimeout)
	// The refusal writes its row before it answers, so one read follows.
	var rowsErr error
	if r.Rows, rowsErr = auditJSONRows(ctx, e, "--event", string(audit.AuditEventSessionLaunch), "--json", "--tail", "50"); rowsErr != nil {
		r.RowsErr = rowsErr.Error()
	}
	return classifyCosOutside(r, acmeID)
}

func classifyCosOutside(r cosOutsideRun, projectID string) result {
	const id = cosOutsideID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	errCode := func(x frontendResponse) string {
		var b struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(x.Body, &b)
		return b.Error
	}
	switch {
	case r.Dotdot.Status == http.StatusUnauthorized || r.Symlink.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the start route")
	case r.Dotdot.Status != http.StatusBadRequest || errCode(r.Dotdot) != "folder_invalid":
		return fail("folder ../x answered %d %q, want 400 folder_invalid", r.Dotdot.Status, errCode(r.Dotdot))
	case r.Symlink.Status != http.StatusForbidden || errCode(r.Symlink) != "directory_outside_project":
		return fail("symlink out of %s answered %d %q, want 403 directory_outside_project", r.Project, r.Symlink.Status, errCode(r.Symlink))
	case r.RowsErr != "":
		return blocked(id, r.RowsErr)
	}
	for _, row := range r.Rows {
		if row.Outcome == audit.AuditOutcomeDenied && !row.TS.Before(r.Since) && row.Actor.ProjectID == projectID && rowArgs(row).Origin == cosOrigin {
			return result{id, statePass, "folder ../x refused 400 folder_invalid; a symlink out of " + r.Project + " refused 403 directory_outside_project with a denied session_launch row"}
		}
	}
	return fail("no denied session_launch row with origin %s for the symlink refusal", cosOrigin)
}
