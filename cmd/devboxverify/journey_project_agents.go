package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/cmdlookup"
)

const (
	agentsSettingsID = "project-agents-settings"
	agentsHostID     = "project-agents-host"

	agentsUIWait = 10 * time.Second
	agentsGapMS  = 200 * time.Millisecond

	agentWrapperPrefix = "dbv-agent-"
	agentNoneText      = "No agents yet."
)

// agentEntry is one element of a project's agent list as the HTTP door
// carries it.
type agentEntry struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Model   string   `json:"model,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

func agentsPath(projectID string) string { return "/api/projects/" + projectID + "/agents" }

func listAgentEntries(ctx context.Context, e env, token, projectID string) ([]agentEntry, error) {
	resp := frontendDo(ctx, e, token, http.MethodGet, agentsPath(projectID), nil)
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d: %s", agentsPath("{id}"), resp.Status, resp.Error)
	}
	var v struct {
		Entries []agentEntry `json:"entries"`
	}
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		return nil, errors.New("GET " + agentsPath("{id}") + " answered unreadable JSON")
	}
	return v.Entries, nil
}

func hasAgentEntry(entries []agentEntry, name string) bool {
	return slices.ContainsFunc(entries, func(a agentEntry) bool { return a.Name == name })
}

// agentConfigRow is the newest agent_entry config_change row for one entry
// write. The row is written before the door answers, so no wait is needed.
func agentConfigRow(ctx context.Context, e env, projectID, action, name string) (*audit.AuditEvent, error) {
	rows, err := issuanceRows(ctx, e, audit.AuditEventConfigChange, name)
	if err != nil {
		return nil, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Credential == "agent_entry" && r.Subject == projectID &&
			slices.Contains(r.Grants, "action="+action) && slices.Contains(r.Grants, "entry="+name) {
			return &r, nil
		}
	}
	return nil, nil
}

// agentWrapperDir is where the host journey puts its wrapper: a folder the
// login shell already has on PATH, so the entry can name the wrapper by name.
func agentWrapperDir() string { return filepath.Join(home, ".local", "bin") }

func agentWrapperGlob() string { return filepath.Join(agentWrapperDir(), agentWrapperPrefix+"*") }

// ---- project-agents-settings ----

// agentsSettingsRun is what the journey saw. A step that could not be driven
// stops the run and is named.
type agentsSettingsRun struct {
	Untrusted bool
	Setup     result

	Step    string
	StepErr error

	Entry, Command string
	RowText        string

	AddDialog, RemoveDialog dialogResult
	ListedAfterAdd          bool
	StillListed             bool
	AddRow, RemoveRow       *audit.AuditEvent
	RowsErr                 error
	CleanupNote             string
	CloseErr                error
}

// liveAgentsSteps drives the Projects page through Accessibility and reads
// the result back over the frontend API with the run credential.
type liveAgentsSteps struct{ liveCosSteps }

func runProjectAgentsSettings(ctx context.Context, e env) result {
	s := &liveAgentsSteps{liveCosSteps{liveSettingsSteps: liveSettingsSteps{e: e}}}
	return classifyAgentsSettings(s.run(ctx))
}

// agentRowLine is the text the Agents list shows for an entry with no model
// and no arguments: name, kind, model, command, joined by a middle dot.
func agentRowLine(name, kind, command string) string {
	return name + " · " + kind + " · (default) · " + command
}

func normText(s string) string { return strings.Join(strings.Fields(s), " ") }

func pageHasText(page []*axNode, want string) bool {
	return slices.ContainsFunc(page, func(n *axNode) bool { return n.Role == "AXStaticText" && normText(n.Label) == want })
}

func buttonLabeled(label string) func([]*axNode) *axNode {
	return func(page []*axNode) *axNode {
		for _, n := range page {
			if n.Role == "AXButton" && n.Label == label {
				return n
			}
		}
		return nil
	}
}

// lastButtonLabeled picks the page's last button of a label: the project
// form's own Cancel closes the page, and it sits below the agent form's.
func lastButtonLabeled(label string) func([]*axNode) *axNode {
	return func(page []*axNode) *axNode {
		for i := len(page) - 1; i >= 0; i-- {
			if page[i].Role == "AXButton" && page[i].Label == label {
				return page[i]
			}
		}
		return nil
	}
}

// buttonAfterText finds the first button of a label after the first static
// text equal to text. A page lists cards as siblings with no container, so
// the card is the span from its text to its first button of that label.
func buttonAfterText(text, label string) func([]*axNode) *axNode {
	return func(page []*axNode) *axNode {
		seen := false
		for _, n := range page {
			switch {
			case !seen:
				seen = n.Role == "AXStaticText" && normText(n.Label) == text
			case n.Role == "AXButton" && n.Label == label:
				return n
			}
		}
		return nil
	}
}

// formFieldsAfter lists the text fields after the agent form's title. The
// form's fields are Name, then Command; the kind and model are pop-ups.
func formFieldsAfter(page []*axNode) []*axNode {
	var out []*axNode
	seen := false
	for _, n := range page {
		switch {
		case !seen:
			seen = n.Role == "AXStaticText" && n.Label == "Add agent"
		case n.Role == "AXTextField":
			out = append(out, n)
		}
	}
	return out
}

func formField(idx int) func([]*axNode) *axNode {
	return func(page []*axNode) *axNode {
		if f := formFieldsAfter(page); idx < len(f) {
			return f[idx]
		}
		return nil
	}
}

// actOn finds a control from a fresh snapshot, retrying while the page
// re-renders, and runs do on it before the snapshot is released.
func (s *liveAgentsSteps) actOn(ctx context.Context, what string, find func([]*axNode) *axNode, do func(*axNode) error) error {
	return retryInvalidElement(ctx, func() error {
		n, release, err := pollNode(ctx, agentsUIWait, func() (*axNode, func(), error) {
			win, release, err := s.settingsWindow()
			if err != nil || win == nil {
				return nil, release, err
			}
			if n := find(flatten(win)); n != nil {
				return n, release, nil
			}
			release()
			return nil, func() {}, nil
		})
		defer release()
		if err != nil {
			return err
		}
		if n == nil {
			return fmt.Errorf("no %s within %s", what, agentsUIWait)
		}
		return do(n)
	})
}

func (s *liveAgentsSteps) press(ctx context.Context, what string, find func([]*axNode) *axNode) error {
	return s.actOn(ctx, what, find, func(n *axNode) error { return n.press() })
}

func (s *liveAgentsSteps) typeIn(ctx context.Context, what string, find func([]*axNode) *axNode, text string) error {
	return s.actOn(ctx, what, find, func(n *axNode) error {
		if err := axFocus(n); err != nil {
			return err
		}
		return axTypeText(s.e.RelayPID, text)
	})
}

// waitPage reads the page until done holds or within passes. Waits: none
// possible: the WebView exposes no event to an out-of-process Accessibility
// reader, and the Agents list is repainted by script after relay answers.
// Poll every 200 ms for up to within; this is the only wait the journey has
// besides the presence dialog and the HTTP responses.
func (s *liveAgentsSteps) waitPage(ctx context.Context, within time.Duration, done func([]*axNode) bool) (bool, error) {
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		win, release, err := s.settingsWindow()
		ok := false
		switch {
		case err != nil:
			lastErr = err
		case win == nil:
			lastErr = fmt.Errorf("no %q window", settingsWindowTitle)
		default:
			lastErr = nil
			ok = done(flatten(win))
		}
		release()
		if ok {
			return true, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, lastErr
		}
		time.Sleep(agentsGapMS)
	}
}

// answering starts the helper that answers relay's prompt for name, runs
// trigger, and returns the helper's result. trigger must fire the write.
func (s *liveAgentsSteps) answering(ctx context.Context, name string, trigger func() error) (dialogResult, error) {
	wait, err := startDialog(ctx, s.e, dialogAnswer, fmt.Sprintf("%q", name), dialogTimeout)
	if err != nil {
		return notStarted(wait, err), nil
	}
	if terr := trigger(); terr != nil {
		_ = wait()
		return dialogResult{}, terr
	}
	return wait(), nil
}

func (s *liveAgentsSteps) run(ctx context.Context) (r agentsSettingsRun) {
	if !s.Trusted() {
		r.Untrusted = true
		return r
	}
	token, res, ok := runCredential(s.e, agentsSettingsID)
	if !ok {
		r.Setup = res
		return r
	}
	pid := s.e.Run.GrantProjectID
	if pid == "" || s.e.Run.GrantProjectName == "" {
		r.Setup = blocked(agentsSettingsID, "no Verify Grant project: "+grantPosID+" did not pass")
		return r
	}
	r.Entry = "verify-" + s.e.Nonce + "-ui"
	r.Command = filepath.Join(s.e.BinDir, "testmcp")
	r.RowText = agentRowLine(r.Entry, "claude", r.Command)
	if entries, err := listAgentEntries(ctx, s.e, token, pid); err != nil {
		r.Setup = blocked(agentsSettingsID, "fixture: "+err.Error())
		return r
	} else if hasAgentEntry(entries, r.Entry) {
		r.Setup = blocked(agentsSettingsID, "fixture: the entry "+r.Entry+" is already listed")
		return r
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		r.CleanupNote = s.cleanup(bg, token, pid, r.Entry)
		r.CloseErr = s.Close(bg)
	}()

	fail := func(step string, err error) agentsSettingsRun { r.Step, r.StepErr = step, err; return r }
	if err := s.OpenProjects(ctx); err != nil {
		return fail("open the Projects page", err)
	}
	if err := s.press(ctx, "Edit button of "+s.e.Run.GrantProjectName, buttonAfterText(s.e.Run.GrantProjectName, "Edit")); err != nil {
		return fail("open the project", err)
	}
	if err := s.press(ctx, `"Add agent" button`, buttonLabeled("Add agent")); err != nil {
		return fail("open the agent form", err)
	}
	if err := s.typeIn(ctx, "Name field", formField(0), r.Entry); err != nil {
		return fail("type the name", err)
	}
	if err := s.typeIn(ctx, "Command field", formField(1), r.Command); err != nil {
		return fail("type the command", err)
	}
	typed, err := s.waitPage(ctx, agentsUIWait, func(page []*axNode) bool {
		f := formFieldsAfter(page)
		return len(f) >= 2 && f[0].Value == r.Entry && f[1].Value == r.Command
	})
	if !typed {
		return fail("type the fields", cmpErr(err, "the form fields do not show what was typed"))
	}

	// The Save click raises relay's presence prompt through the Settings IPC;
	// the helper answers it, and the list is repainted after relay's reply.
	r.AddDialog, err = s.answering(ctx, r.Entry, func() error {
		return s.press(ctx, `"Save agent" button`, buttonLabeled("Save agent"))
	})
	if err != nil {
		return fail("save the agent", err)
	}
	if res, refused := dialogRefusal(agentsSettingsID, r.AddDialog, dialogAnswer); refused {
		r.Setup = res
		return r
	}
	shown, err := s.waitPage(ctx, agentsUIWait, func(page []*axNode) bool { return pageHasText(page, r.RowText) })
	if !shown {
		return fail("read the new row", cmpErr(err, fmt.Sprintf("no row %q after the prompt was answered", r.RowText)))
	}
	entries, lerr := listAgentEntries(ctx, s.e, token, pid)
	r.ListedAfterAdd = lerr == nil && hasAgentEntry(entries, r.Entry)
	r.AddRow, r.RowsErr = agentConfigRow(ctx, s.e, pid, "add", r.Entry)

	r.RemoveDialog, err = s.answering(ctx, r.Entry, func() error {
		return s.press(ctx, "Remove button of the new row", buttonAfterText(r.RowText, "Remove"))
	})
	if err != nil {
		return fail("remove the agent", err)
	}
	if res, refused := dialogRefusal(agentsSettingsID, r.RemoveDialog, dialogAnswer); refused {
		r.Setup = res
		return r
	}
	// A page that shows nothing passes a text-absence check vacuously, so the
	// removal also needs the Add agent button back.
	gone, err := s.waitPage(ctx, agentsUIWait, func(page []*axNode) bool {
		return !pageHasText(page, r.RowText) && buttonLabeled("Add agent")(page) != nil
	})
	if !gone {
		return fail("read the removal", cmpErr(err, "the row is still shown, or the Agents list is unreadable, after the removal prompt was answered"))
	}
	entries, lerr = listAgentEntries(ctx, s.e, token, pid)
	r.StillListed = lerr != nil || hasAgentEntry(entries, r.Entry)
	if r.RemoveRow, lerr = agentConfigRow(ctx, s.e, pid, "remove", r.Entry); lerr != nil {
		r.RowsErr = lerr
	}
	return r
}

func cmpErr(err error, text string) error {
	if err != nil {
		return err
	}
	return errors.New(text)
}

// cleanup leaves the project as found: it removes the entry through the same
// gate if the run left it, then closes the project form so a later journey
// meets the project list. It reports what it could not undo.
func (s *liveAgentsSteps) cleanup(ctx context.Context, token, pid, name string) string {
	var notes []string
	if entries, err := listAgentEntries(ctx, s.e, token, pid); err == nil && hasAgentEntry(entries, name) {
		resp, d := gatedFrontend(ctx, s.e, token, http.MethodDelete, agentsPath(pid)+"/"+name, nil, fmt.Sprintf("%q", name))
		if resp.Status != http.StatusOK {
			notes = append(notes, fmt.Sprintf("entry %s left in the project (remove status %d, prompt %s)", name, resp.Status, d.Outcome))
		}
	}
	if err := s.press(ctx, "project form Cancel button", lastButtonLabeled("Cancel")); err != nil {
		notes = append(notes, "project form left open: "+err.Error())
	}
	return strings.Join(notes, "; ")
}

func classifyAgentsSettings(r agentsSettingsRun) result {
	const id = agentsSettingsID
	fail := func(d string) result { return result{id, stateFail, d + noteSuffix(r)} }
	switch {
	case r.Untrusted:
		return blocked(id, "no Accessibility trust for this process; run devboxverify from a desktop Terminal")
	case r.Setup.State != "":
		return r.Setup
	case errors.Is(r.StepErr, errAXDriver):
		return blocked(id, r.Step+": "+r.StepErr.Error())
	case r.StepErr != nil:
		return fail(r.Step + ": " + r.StepErr.Error())
	case !r.ListedAfterAdd:
		return fail("the saved entry is not in GET " + agentsPath("{id}"))
	case r.RowsErr != nil:
		return blocked(id, "relay audit: "+r.RowsErr.Error())
	}
	if res, bad := agentRowFailure(id, "add", r.AddRow, nil, r.Command); bad {
		return withNote(res, r)
	}
	if res, bad := agentRowFailure(id, "remove", r.RemoveRow, nil, r.Command); bad {
		return withNote(res, r)
	}
	if r.StillListed {
		return fail("the entry is still listed after Remove")
	}
	if r.CleanupNote != "" || r.CloseErr != nil {
		return fail("restore failed")
	}
	return result{id, statePass, "Add agent form saved after the prompt was answered; the row read name, kind, model and the resolved command; Remove took it away after its prompt; both writes recorded with presence"}
}

func noteSuffix(r agentsSettingsRun) string {
	var parts []string
	if r.CleanupNote != "" {
		parts = append(parts, "cleanup: "+r.CleanupNote)
	}
	if r.CloseErr != nil {
		parts = append(parts, "close: "+r.CloseErr.Error())
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

func withNote(res result, r agentsSettingsRun) result {
	res.Detail += noteSuffix(r)
	return res
}

// ---- project-agents-host ----

// agentsHostRun is what the host journey saw.
type agentsHostRun struct {
	Setup string // wrapper, host or project could not be made

	WrapperPath string
	Entry       string
	AddDialog   dialogResult
	Add         frontendResponse
	Stored      agentEntry
	Machine     string
	HostName    string

	DialStatus int
	DialErr    string
	Create     frontendResponse
	SessionID  string
	JoinSeen   bool
	Message    frontendResponse
	TurnDone   bool
	Limit      providerLimit

	MarkerPath string
	MarkerText string
	MarkerErr  error

	Delete   frontendResponse
	Teardown string
}

// wrapperScript records that it ran, and over ssh, then hands over to the
// real claude with the arguments relay built. SSH_CONNECTION is set by sshd
// for the remote command only.
func wrapperScript(markerPath, claudePath string) string {
	return "#!/bin/sh\nprintf 'ran ssh=%s\\n' \"${SSH_CONNECTION:-none}\" >> " + shellQuote(markerPath) +
		"\nexec " + shellQuote(claudePath) + " \"$@\"\n"
}

func runProjectAgentsHost(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, agentsHostID)
	if !ok {
		return res
	}
	claude, err := cmdlookup.Local(ctx, "claude")
	if err != nil {
		return blocked(agentsHostID, "fixture: no claude on this machine's login-shell PATH to wrap: "+err.Error())
	}
	r := agentsHostRun{Entry: "verify-" + e.Nonce + "-wrap"}
	wrapper := agentWrapperPrefix + e.Nonce
	r.WrapperPath = filepath.Join(agentWrapperDir(), wrapper)
	r.MarkerPath = filepath.Join(gateStateDir(), "grant-agenthost-"+e.Nonce+".ran")
	if err := os.MkdirAll(gateStateDir(), 0o755); err != nil {
		return blocked(agentsHostID, "cannot create the state folder: "+err.Error())
	}
	if err := os.MkdirAll(agentWrapperDir(), 0o755); err != nil {
		return blocked(agentsHostID, "cannot create the wrapper folder: "+err.Error())
	}
	if err := os.WriteFile(r.WrapperPath, []byte(wrapperScript(r.MarkerPath, claude)), 0o755); err != nil {
		return blocked(agentsHostID, "cannot write the wrapper: "+err.Error())
	}
	defer func() { _ = os.Remove(r.WrapperPath); _ = os.Remove(r.MarkerPath) }()

	r.Setup, r.Teardown = withLoopbackHostProject(ctx, e, run, func(projectID, hostName, _ string) {
		r.HostName = hostName
		driveAgentsHost(ctx, e, launch, run, projectID, wrapper, &r)
	})
	return classifyAgentsHost(r)
}

// driveAgentsHost adds the entry, starts a session by it, waits for the
// turn, and reads the wrapper's marker. It stops at the first step that
// failed and leaves the verdict to the classifier.
func driveAgentsHost(ctx context.Context, e env, launch, run, projectID, wrapper string, r *agentsHostRun) {
	body := jsonBody(agentEntry{Name: r.Entry, Kind: "claude", Model: agentModel, Command: wrapper})
	r.Add, r.AddDialog = gatedFrontend(ctx, e, run, http.MethodPost, agentsPath(projectID), body, fmt.Sprintf("%q", r.Entry))
	if r.Add.Status != http.StatusCreated {
		return
	}
	var added struct {
		Entry   agentEntry `json:"entry"`
		Machine string     `json:"machine"`
	}
	_ = json.Unmarshal(r.Add.Body, &added)
	r.Stored, r.Machine = added.Entry, added.Machine

	obs, status, err := dialCos(ctx, e, run, false)
	r.DialStatus = status
	if err != nil {
		r.DialErr = err.Error()
		return
	}
	defer func() { _ = obs.conn.Close() }()

	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", jsonBody(map[string]any{
		"projectId": projectID, "name": "verify-" + e.Nonce + "-entry", "entry": r.Entry,
		"settings": map[string]bool{"headless": true, "agent": true},
	}), 60*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return
	}
	id := r.SessionID
	defer func() {
		r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+id, nil)
	}()

	_ = obs.send(map[string]string{"type": "join_session", "sessionId": id})
	if r.JoinSeen = obs.waitFor(ctx, cosWait, cosJoined(id, 1)); !r.JoinSeen {
		return
	}
	mark := len(obs.snapshot())
	r.Message, _ = chatTurn(ctx, e, run, id, "Reply with exactly: verify-"+e.Nonce+"-entry")
	if r.Message.Status != http.StatusOK {
		return
	}
	// The wait on the host's model output: a turn_done frame, then idle.
	// The reply text is never judged; the wrapper's marker is the proof.
	if r.TurnDone = obs.waitFor(ctx, cosTurnWait, cosTurnFinishedAfter(id, mark)); !r.TurnDone {
		r.Limit = sessionLimit(obs.snapshot(), id)
		return
	}
	// The wrapper wrote the marker when the host process started, before any
	// turn could finish, so one read is enough.
	b, err := os.ReadFile(r.MarkerPath)
	r.MarkerText, r.MarkerErr = string(b), err
}

func classifyAgentsHost(r agentsHostRun) result {
	const id = agentsHostID
	fail := func(d string) result { return result{id, stateFail, d + r.Teardown} }
	if r.Setup != "" {
		return blocked(id, r.Setup+r.Teardown)
	}
	if res, refused := positiveRefusal(id, r.Add.Error, r.AddDialog); refused {
		res.Detail += r.Teardown
		return res
	}
	switch {
	case r.Add.Status == http.StatusUnprocessableEntity && strings.Contains(string(r.Add.Body), "command_not_found"):
		return blocked(id, "fixture: the wrapper folder "+agentWrapperDir()+" is not on the host login shell's PATH: "+r.Add.Error+r.Teardown)
	case r.Add.Status != http.StatusCreated:
		return fail(fmt.Sprintf("POST %s on the host project status %d: %s", agentsPath("{id}"), r.Add.Status, r.Add.Error))
	case r.Stored.Command != r.WrapperPath:
		return fail(fmt.Sprintf("the entry stored command %q, want the host's path %q", r.Stored.Command, r.WrapperPath))
	case r.Machine != r.HostName:
		return fail(fmt.Sprintf("the add answered machine %q, want the host %q", r.Machine, r.HostName))
	case r.DialErr != "":
		return blocked(id, fmt.Sprintf("/ws dial status %d: %s%s", r.DialStatus, r.DialErr, r.Teardown))
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return fail(fmt.Sprintf("POST /api/sessions with entry status %d: %s", r.Create.Status, r.Create.Error))
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case r.Message.Status != http.StatusOK:
		return fail(fmt.Sprintf("message status %d: %s", r.Message.Status, r.Message.Error))
	case !r.TurnDone:
		return r.Limit.turnFail(id, "no turn_done, then idle, after the entry's session was sent a message (host claude cannot sign in over ssh? setup P11)"+r.Teardown)
	case r.MarkerErr != nil:
		return fail("the turn finished but the wrapper left no marker: the session did not run the entry's command (" + path.Base(r.WrapperPath) + ")")
	case !strings.Contains(r.MarkerText, "ran ssh=") || strings.Contains(r.MarkerText, "ssh=none"):
		return fail("the wrapper ran, but not under ssh: marker " + lastLine(r.MarkerText))
	case r.Delete.Status != http.StatusNoContent:
		return fail(fmt.Sprintf("DELETE session status %d", r.Delete.Status))
	}
	return result{id, statePass, fmt.Sprintf("entry %s named a wrapper by name; the add resolved it on host %s and stored its path; a session started by that entry finished a turn and the wrapper's marker shows it ran under ssh%s",
		r.Entry, r.HostName, r.Teardown)}
}
