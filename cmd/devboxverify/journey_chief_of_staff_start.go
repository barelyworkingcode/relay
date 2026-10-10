package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	cosStartID   = "cos-start"
	cosOutsideID = "cos-start-outside-root"
	cosHostID    = "cos-start-host"
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
	revoke = func() string {
		bg := context.WithoutCancel(ctx)
		c, _ := gatedCLI(bg, e, fmt.Sprintf("%q", id), "credential", "revoke", "--id", id)
		if c.Exit != 0 {
			return fmt.Sprintf("revoking credential %s failed (exit %d: %s); it expires within the hour", id, c.Exit, lastLine(c.Stderr))
		}
		return ""
	}
	// A mint that printed an id and then failed a check is still revoked.
	refuse := func(r result) (string, func() string, result, bool) {
		if id != "" {
			if d := revoke(); d != "" {
				r.Detail += "; " + d
			}
		}
		return "", nil, r, false
	}
	if r, bad := positiveRefusal(cosStartID, cli.Stderr, d); bad {
		return refuse(r)
	}
	switch {
	case cli.Exit != 0:
		return refuse(cliExitFail(cosStartID, "credential mint", cli))
	case id == "" || !isToken(token):
		return refuse(result{cosStartID, stateFail, "credential mint printed no id or no 64-hex token"})
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
		return sessionLimit(r.Frames, sid).turnFail(id, "no turn_done carrying the marker, then idle, after the headless start")
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

// cosHostRun is what the hosted-start journey observed, in order.
type cosHostRun struct {
	HostID, Host, Project string
	Marker, Marker2       string
	Setup                 string // host or project could not be made (setup P11)

	DialStatus int
	DialErr    string

	Create      frontendResponse
	SessionID   string
	JoinSeen    bool
	TurnWaitErr bool
	List        frontendResponse
	PS          string
	PSErr       string

	Send     frontendResponse
	Turn2Err bool
	RowsErr  string
	Limit    providerLimit      // set when a turn wait fails
	Launch   []audit.AuditEvent // session_launch rows naming the session
	Msgs     []audit.AuditEvent // session_message rows naming the session

	Delete   frontendResponse
	Teardown string
}

func runCosStartHost(ctx context.Context, e env) (out result) {
	_, run, res, ok := screenCreds(e, cosHostID)
	if !ok {
		return res
	}
	cosTok, revoke, res, ok := mintCosStartCred(ctx, e)
	if !ok {
		res.ID = cosHostID
		return res
	}
	// Registered first so it runs last: the deletes below use the credential.
	defer func() {
		if d := revoke(); d != "" && out.State == statePass {
			out = result{cosHostID, stateFail, d}
		}
	}()
	r := cosHostRun{Host: dropInHostPrefix + e.Nonce + "-cos", Project: dropInProjectName + "cos " + e.Nonce,
		Marker: "verify-" + e.Nonce + "-host", Marker2: "verify-" + e.Nonce + "-host2"}
	dir := filepath.Join(gateStateDir(), dropInStateDirName+e.Nonce+"-cos")
	defer func() { _ = os.RemoveAll(dir) }()

	// Host create answers when the probe is done; project create answers when
	// the presence dialog is answered. Both are the response itself.
	hostResp := frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/hosts", jsonBody(map[string]string{
		"name": r.Host, "target": dropInHostTarget, "tmux_path": "/usr/bin/tmux",
	}), hostCreateTimeout)
	var errText string
	if r.HostID, errText = createdID("POST /api/hosts", hostResp); r.HostID == "" {
		return classifyCosStartHost(cosHostRun{Setup: errText})
	}
	var projectID string
	defer func() {
		// Runs after the session delete below, so the project is empty by then.
		r.Teardown = teardownSlowRoute(context.WithoutCancel(ctx), e, run, projectID, r.HostID)
		out = classifyCosStartHost(r)
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Setup = "cannot create the project folder: " + err.Error()
		return
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	projResp, d := gatedFrontend(ctx, e, run, http.MethodPost, "/api/projects", jsonBody(map[string]any{
		"name": r.Project, "path": dir, "host_id": r.HostID, "allowed_templates": []string{"claude-code"},
	}), fmt.Sprintf("%q", r.Project))
	if projectID, errText = createdID("POST /api/projects", projResp); projectID == "" {
		r.Setup = fmt.Sprintf("%s (prompt: %s %s)", errText, d.Outcome, d.Detail)
		return
	}

	obs, status, err := dialCos(ctx, e, run, false)
	r.DialStatus = status
	if err != nil {
		r.DialErr = err.Error()
		return
	}
	defer func() { _ = obs.conn.Close() }()
	defer func() {
		if r.SessionID != "" {
			r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
		}
	}()

	r.Create = cosRequest(ctx, e, cosTok, true, http.MethodPost, cosStartPath,
		cosStartBody(projectID, "", cosSendText(r.Marker), ""), 60*time.Second)
	if r.SessionID = parseCosStart(r.Create).SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return
	}
	id := r.SessionID
	_ = obs.send(map[string]string{"type": "join_session", "sessionId": id})
	if r.JoinSeen = obs.waitFor(ctx, cosWait, cosJoined(id, 1)); !r.JoinSeen {
		return
	}
	// The wait on model output: the turn_done frame carrying the marker.
	if r.TurnWaitErr = !obs.waitFor(ctx, cosTurnWait, cosTurnFinished(id, r.Marker)); r.TurnWaitErr {
		r.Limit = sessionLimit(obs.snapshot(), id)
		return
	}
	r.List = cosRequest(ctx, e, cosTok, true, http.MethodGet, "/api/sessions", nil, frontendRequestTimeout)
	// The session child is alive between turns, so the process list is read once now.
	psOut, err := exec.CommandContext(ctx, "ps", "-axwwo", "command").Output()
	r.PS = string(psOut)
	if err != nil {
		r.PSErr = err.Error()
	}

	r.Send = cosRequest(ctx, e, cosTok, true, http.MethodPost, "/api/chief-of-staff/messages",
		jsonBody(map[string]string{"sessionId": id, "text": cosSendText(r.Marker2)}), frontendRequestTimeout)
	if r.Send.Status != http.StatusAccepted {
		return
	}
	if r.Turn2Err = !obs.waitFor(ctx, cosTurnWait, cosTurnFinished(id, r.Marker2)); r.Turn2Err {
		r.Limit = sessionLimit(obs.snapshot(), id)
		return
	}
	// Both rows are written before the 202, so one read follows the second turn.
	var rowsErr error
	if r.Msgs, rowsErr = auditJSONRows(ctx, e, "--event", string(audit.AuditEventSessionMessage), "--grep", id, "--json", "--tail", "50"); rowsErr == nil {
		r.Launch, rowsErr = auditJSONRows(ctx, e, "--event", string(audit.AuditEventSessionLaunch), "--grep", id, "--json", "--tail", "20")
	}
	if rowsErr != nil {
		r.RowsErr = rowsErr.Error()
	}
	return
}

// scriptB64 finds the base64 script inside the remote command ssh was given:
// sh -c 'eval "$(printf %s <BASE64> | base64 -d)"'.
var scriptB64 = regexp.MustCompile(`printf %s ([A-Za-z0-9+/]+=*) \| base64 -d`)

// sshChildSetsSession is true when one process-list line is an ssh child run
// with -T and the end-of-options marker whose decoded script sets
// RELAY_SESSION_ID to the session id.
func sshChildSetsSession(psOut, sessionID string) bool {
	for _, line := range strings.Split(psOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || filepath.Base(fields[0]) != "ssh" || !strings.Contains(line, " -T -- ") {
			continue
		}
		for _, m := range scriptB64.FindAllStringSubmatch(line, -1) {
			script, err := base64.StdEncoding.DecodeString(m[1])
			if err != nil {
				continue
			}
			for _, q := range []string{"", "'"} {
				if strings.Contains(string(script), q+"RELAY_SESSION_ID"+q+"="+q+sessionID+q) {
					return true
				}
			}
		}
	}
	return false
}

func classifyCosStartHost(r cosHostRun) result {
	const id = cosHostID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	switch {
	case r.Setup != "":
		return blocked(id, "loopback host or its project not set up (setup P11): "+r.Setup+r.Teardown)
	case r.DialStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.DialErr != "" && r.DialStatus == 0:
		return blocked(id, "frontend socket unreachable: "+r.DialErr)
	case r.DialErr != "":
		return fail("GET /ws status %d: %s", r.DialStatus, r.DialErr)
	case r.Create.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the start route")
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return fail("headless start in the host project status %d, want 201: %s", r.Create.Status, r.Create.Error)
	}
	if s := parseCosStart(r.Create); s.Origin != cosOrigin || s.Mode != "headless" {
		return fail("hosted start answered origin %q mode %q, want %q headless", s.Origin, s.Mode, cosOrigin)
	}
	sid := r.SessionID
	switch {
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case r.TurnWaitErr:
		return r.Limit.turnFail(id, "no turn_done carrying the marker, then idle, after the hosted start (host claude cannot sign in over ssh? setup P11)")
	case r.List.Status != http.StatusOK:
		return fail("scoped GET /api/sessions status %d, want 200", r.List.Status)
	case r.PSErr != "":
		return fail("process list unreadable: %s", r.PSErr)
	}
	if o, found := listedOrigin(r.List.Body, "sessions", sid); !found || o != cosOrigin {
		return fail("GET /api/sessions row found=%v origin %q, want %q", found, o, cosOrigin)
	}
	if !sshChildSetsSession(r.PS, sid) {
		return fail("no ssh ... -T -- process whose script sets RELAY_SESSION_ID to %s", sid)
	}
	switch {
	case r.Send.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the send route")
	case r.Send.Status != http.StatusAccepted:
		return fail("scoped POST /api/chief-of-staff/messages status %d, want 202: %s", r.Send.Status, r.Send.Error)
	case r.Turn2Err:
		return r.Limit.turnFail(id, "no turn_done carrying the second marker, then idle, after the scoped message")
	case r.RowsErr != "":
		return blocked(id, r.RowsErr)
	}
	if d := cosHostAudit(r); d != "" {
		return fail("%s", d)
	}
	switch {
	case r.Delete.Status/100 != 2:
		return fail("DELETE of the hosted session status %d: it may still be running", r.Delete.Status)
	case r.Teardown != "":
		return fail("hosted fixtures not removed%s", r.Teardown)
	}
	return result{id, statePass, fmt.Sprintf("Chief of Staff started a headless Haiku agent in a project on SSH host %s and sent it a second message: row marked %s, launch row ok with host_id, session child is ssh -T -- setting RELAY_SESSION_ID, both turns carried their markers, intent and completion rows carry the origin",
		r.Host, cosOrigin)}
}

// cosHostAudit checks the launch row names the host and the message rows of
// both turns (the start prompt and the send) carry the origin.
func cosHostAudit(r cosHostRun) string {
	if r.HostID == "" {
		return "host id unknown"
	}
	launched := false
	for _, row := range r.Launch {
		var a struct {
			SessionID string `json:"session_id"`
			Origin    string `json:"origin"`
			HostID    string `json:"host_id"`
		}
		_ = json.Unmarshal(row.Args, &a)
		if row.Event != string(audit.AuditEventSessionLaunch) || a.SessionID != r.SessionID {
			continue
		}
		launched = true
		if row.Outcome != audit.AuditOutcomeOK || a.Origin != cosOrigin || a.HostID != r.HostID {
			return fmt.Sprintf("session_launch row outcome %q origin %q host_id %q, want ok, %q and %q", row.Outcome, a.Origin, a.HostID, cosOrigin, r.HostID)
		}
	}
	if !launched {
		return "no session_launch row for the hosted session"
	}
	for _, phase := range []string{audit.AuditPhaseIntent, audit.AuditPhaseCompletion} {
		rows := messageRowsFor(r.Msgs, r.SessionID, phase)
		if len(rows) != 2 {
			return fmt.Sprintf("%d %s session_message rows for the hosted session, want 2 (start prompt and send)", len(rows), phase)
		}
		for _, row := range rows {
			if o := rowArgs(row).Origin; o != cosOrigin {
				return fmt.Sprintf("session_message %s origin %q, want %q", phase, o, cosOrigin)
			}
		}
	}
	return ""
}
