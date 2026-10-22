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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/cmdlookup"
)

const (
	adoptHostID      = "session-adopt-host"
	adoptHostDropID  = "session-adopt-host-drop"
	adoptHostRowWait = 90 * time.Second
	adoptHostWait    = 180 * time.Second
	adoptHostStep    = 90 * time.Second
)

// adoptAnswer is the adopt door's HTTP answer, read from a goroutine.
type adoptAnswer struct {
	status int
	body   []byte
}

// runAdoptHost adopts an interactive Claude that runs outside relay on the
// loopback SSH host, then drives the adopted session as the Chief of Staff and
// the owner do: a send, a drop in, a send refused while dropped in, the hand
// back and a second send.
//
// Deliberate: the adopt sends model "haiku". On a stack the transcript records
// the stack's model id, which names no Claude family, and the stack maps only
// the haiku alias to its endpoint. The no-model rule is an e2e test.
func runAdoptHost(ctx context.Context, e env) result { return adoptHostFlow(ctx, e, adoptHostID) }

// runAdoptHostDrop stops the outside Claude with SIGSTOP, so the script's
// SIGTERM wait runs its full bound, and kills the adopt's ssh client when the
// script logs the SIGTERM. The script outlives the client: it ends the process
// and starts the agent, and Resume continues the dormant session.
func runAdoptHostDrop(ctx context.Context, e env) result {
	return adoptHostFlow(ctx, e, adoptHostDropID)
}

// adoptHostRow is the part of an outside row the journeys read.
type adoptHostRow struct {
	ID              string  `json:"id"`
	HostID          *string `json:"hostId"`
	ClaudeSessionID string  `json:"claudeSessionId"`
	Adoptable       bool    `json:"adoptable"`
	AdoptReason     string  `json:"adoptReason"`
}

func adoptHostFlow(ctx context.Context, e env, id string) result {
	drop := id == adoptHostDropID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, err.Error())
	}
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	tmux := adoptTmux()
	claude, cerr := cmdlookup.Local(ctx, "claude")
	switch {
	case tmux == "":
		return blocked(id, "tmux is not installed")
	case cerr != nil:
		return blocked(id, "no claude on this machine's login-shell PATH")
	}
	hostTmux, _, err := findHostTmux(ctx)
	if err != nil {
		return blocked(id, "the loopback host has no tmux: "+err.Error())
	}
	agentsRoot, err := hostAgentsRoot()
	if err != nil {
		return blocked(id, err.Error())
	}
	fx, err := setUpDetachedFixture(ctx, e, baseURL, model, tokenFile)
	if err != nil {
		return blocked(id, "fixture host or project: "+err.Error())
	}
	token, credID, err := seedSwapCredential(e, fx, hostTmux)
	if err != nil {
		return blocked(id, "fixture credential: "+err.Error()+fx.tearDown())
	}

	suffix := "ahost"
	if drop {
		suffix = "ahdrop"
	}
	tag := "verify-" + e.Nonce + "-" + suffix
	convID := uuid.NewString()
	var sids []string
	started := false
	defer func() {
		bg := context.WithoutCancel(ctx)
		for _, sid := range sids {
			cliStopSession(bg, e, sid)
		}
		if pid := adoptPID(bg, convID); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGCONT)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if started {
			// Deliberate: the journey's tmux session lives on the default
			// server, where the host's relay-agent sessions live too, so only
			// its own session is ended, never the server.
			_ = exec.CommandContext(bg, tmux, "kill-session", "-t", "="+tag).Run()
		}
		removeTranscript(id, convID, tag, false, func(ctx context.Context) string {
			if adoptPID(ctx, convID) > 0 {
				return "process still running"
			}
			return ""
		})
		_ = dropSwapCredential(fx, credID)
		_ = fx.tearDown()
	}()

	dir := fx.projectDir
	if err := writeAdoptSettings(dir); err != nil {
		return blocked(id, "project folder: "+err.Error())
	}
	line := adoptShellPrefix(baseURL, model, tokenFile) + "exec env -u CLAUDE_CODE_ENTRYPOINT -u CLAUDECODE " + shellQuote(claude) +
		" --model haiku --session-id " + convID + " --permission-mode default " + shellQuote("Reply with exactly: "+tag+"-1")
	create := exec.CommandContext(ctx, tmux, "new-session", "-d", "-s", tag, "-x", "200", "-y", "50", "-c", dir, line)
	// Deliberate: a person's terminal has neither variable; set, claude
	// registers entrypoint "sdk-cli" and the door refuses not_command_line.
	create.Env = adoptCleanEnv(os.Environ())
	if out, err := create.CombinedOutput(); err != nil {
		return blocked(id, "tmux new-session: "+firstLine(string(out)))
	}
	started = true

	// Waits: none possible: a terminal UI has no signal; pane capture, 30 s.
	adoptHostTrust(ctx, tmux, tag, convID)
	// Waits: none possible for the outside reply (model output); a bounded
	// read of the transcript, 120 s.
	if !pollUntil(ctx, adoptMarkerWait, func() bool { return adoptAssistantHas(convID, tag+"-1") }) {
		return blocked(id, "the outside claude did not reply "+tag+"-1 within 120s (model output)")
	}
	// Waits: the registry status change to idle, re-read, 60 s.
	var pid int
	if !pollUntil(ctx, adoptIdleWait, func() bool {
		pid = adoptPID(ctx, convID)
		return pid > 0 && adoptRegistryStatus(pid) == "idle"
	}) {
		return blocked(id, "the outside claude's registry entry did not read idle within 60s")
	}

	// Waits: none possible for the settings import; the seeded credential
	// answers 200 once it has run, so the read is retried, bounded.
	if !pollUntil(ctx, detachedStartTries, func() bool {
		return frontendDo(ctx, e, token, http.MethodGet, "/api/terminals", nil).Status == http.StatusOK
	}) {
		return blocked(id, "the seeded credential was not imported within "+detachedStartTries.String())
	}
	// Waits: the probe interval; the outside list is polled, bounded. The row
	// is the one with the host's hostId, never its console twin.
	var row adoptHostRow
	if !pollUntil(ctx, adoptHostRowWait, func() bool {
		resp := cosRequest(ctx, e, token, true, http.MethodGet, "/api/chief-of-staff/outside-sessions", nil, frontendRequestTimeout)
		var snap struct {
			Sessions []adoptHostRow `json:"sessions"`
		}
		if resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &snap) != nil {
			return false
		}
		for _, r := range snap.Sessions {
			if r.ClaudeSessionID == convID && r.HostID != nil && *r.HostID == fx.hostID {
				row = r
				return true
			}
		}
		return false
	}) {
		return fail("no outside row with the host's hostId for the conversation within %s", adoptHostRowWait)
	}
	if !row.Adoptable {
		return fail("the host row is not adoptable: %q", row.AdoptReason)
	}

	before := map[string]bool{}
	if ents, _ := os.ReadDir(agentsRoot); ents != nil {
		for _, ent := range ents {
			before[ent.Name()] = true
		}
	}
	if drop {
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			return blocked(id, "SIGSTOP of the outside claude: "+err.Error())
		}
	}
	answered := make(chan adoptAnswer, 1)
	go func() {
		r := frontendDoTimeout(ctx, e, token, http.MethodPost, "/api/sessions/adopt", jsonBody(map[string]any{"outsideId": row.ID, "model": "haiku"}), adoptHostWait)
		answered <- adoptAnswer{r.Status, r.Body}
	}()

	if drop {
		return adoptHostDropDrive(ctx, e, id, agentsRoot, before, answered, pid, convID, tag, &sids)
	}
	// Waits: the adopt's HTTP answer.
	got := <-answered
	var ok struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(got.body, &ok)
	if ok.SessionID != "" {
		sids = append(sids, ok.SessionID)
	}
	if got.status != http.StatusCreated || ok.SessionID == "" {
		return fail("adopt answered %d, want 201: %s", got.status, firstLine(string(got.body)))
	}
	sid := ok.SessionID

	if d := adoptHostListProblem(ctx, e, sid, fx.hostID); d != "" {
		return fail("after the adopt: %s", d)
	}
	if _, d := paneState(ctx, hostTmux, "relay-agent-"+sid); d != "" {
		return fail("after the adopt: %s", d)
	}
	if d := adoptHostTurn(ctx, e, token, sid, convID, tag+"-2"); d != "" {
		return fail("first Chief of Staff send: %s", d)
	}

	since := time.Now()
	dropIn := frontendDoTimeout(ctx, e, token, http.MethodPost, "/api/sessions/"+sid+"/drop-in", []byte(`{}`), swapDropInWait)
	var term swapBody
	_ = json.Unmarshal(dropIn.Body, &term)
	if dropIn.Status != http.StatusCreated || term.Terminal.ID == "" {
		return fail("drop-in answered %d, want 201 with a terminal: %s", dropIn.Status, firstLine(dropIn.Error))
	}
	termID := term.Terminal.ID
	defer func() {
		if termID != "" {
			frontendDo(context.WithoutCancel(ctx), e, token, http.MethodDelete, "/api/terminals/"+termID, nil)
		}
	}()
	// Waits: none possible; tmux gives relay no event when a pane's program
	// starts. The host's process table is polled, bounded, as session-swap-back does.
	if !pollUntil(ctx, 20*time.Second, func() bool {
		table, _ := hostProcessTable(ctx)
		return resumeLaunched(string(table), term.ClaudeSessionID)
	}) {
		return fail("no claude --resume %s in the host's process table within 20 s of the drop-in", term.ClaudeSessionID)
	}
	if st, body := adoptHostSend(ctx, e, token, sid, "Reply with exactly: "+tag+"-held"); st != http.StatusConflict || !strings.Contains(body, "dropped_in") {
		return fail("a send while dropped in answered %d %q, want 409 dropped_in", st, firstLine(body))
	}

	if del := frontendDo(ctx, e, token, http.MethodDelete, "/api/terminals/"+termID, nil); del.Status/100 != 2 {
		return fail("DELETE /api/terminals/{id} answered %d", del.Status)
	}
	termID = ""
	ev, err := waitEvent(ctx, e, e.ConfigDir, "session.hand_back", since, swapHandBackWait, func(m map[string]any) bool { return m["session_id"] == sid })
	if err != nil {
		return fail("after the terminal closed: %v", err)
	}
	if ev["status"] != "ok" {
		return fail("session.hand_back status %v reason %v, want ok", ev["status"], ev["reason"])
	}
	if d := adoptHostListProblem(ctx, e, sid, fx.hostID); d != "" {
		return fail("after the hand back: %s", d)
	}
	if d := adoptHostTurn(ctx, e, token, sid, convID, tag+"-3"); d != "" {
		return fail("second Chief of Staff send after the hand back: %s", d)
	}

	var intent, done bool
	for _, r := range adoptAuditRows(ctx, e, convID) {
		var args struct {
			HostID string `json:"host_id"`
		}
		if json.Unmarshal(r.Args, &args) != nil || args.HostID != fx.hostID {
			return fail("a session_adopt row lacks host_id %s (%s)", fx.hostID, r.Phase)
		}
		intent = intent || r.Phase == "intent"
		done = done || r.Phase == "completion" && r.Outcome == audit.AuditOutcomeOK
	}
	if !intent || !done {
		return fail("session_adopt rows lack the intent or the ok completion")
	}
	return result{id, statePass, "terminal Claude on the loopback host adopted by outsideId: 201, relay-agent-<id> in the host's tmux, listed with its host and headless, a Chief of Staff send answered 202 and replied in the same transcript, drop-in opened Claude and a send answered dropped_in, the hand back made it headless again, a second send answered 202 and replied, session_adopt rows carry host_id"}
}

var adoptHostTerminal = regexp.MustCompile(`^(ok|refused|failed)\b`)

// adoptHostDropDrive drops the adopt's ssh client at the script's SIGTERM line
// and checks that the script finished the adopt alone.
func adoptHostDropDrive(ctx context.Context, e env, id, agentsRoot string, before map[string]bool, answered chan adoptAnswer, pid int, convID, tag string, sids *[]string) result {
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	var sid string
	// Waits: the script's adopt.state line "signal TERM", read from the file
	// the script writes on the host; bounded.
	if !pollUntil(ctx, adoptHostStep, func() bool {
		ents, _ := os.ReadDir(agentsRoot)
		for _, ent := range ents {
			if before[ent.Name()] {
				continue
			}
			for _, l := range adoptStateLines(agentsRoot, ent.Name()) {
				if strings.HasPrefix(l, "signal TERM") {
					sid = ent.Name()
					return true
				}
			}
		}
		return false
	}) {
		return fail("no adopt.state line \"signal TERM\" within %s", adoptHostStep)
	}
	*sids = append(*sids, sid)
	client := adoptSSHClientPID(ctx, sid)
	if client == 0 {
		return fail("no ssh client of relay-sessions whose launcher script holds relay-adopt and the session id")
	}
	if err := syscall.Kill(client, syscall.SIGKILL); err != nil {
		return fail("killing the adopt's ssh client %d: %v", client, err)
	}
	got := <-answered
	var refusal struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(got.body, &refusal)
	if got.status != http.StatusBadGateway || refusal.Error != "host_link_lost" {
		return fail("the dropped adopt answered %d %q, want 502 host_link_lost", got.status, firstLine(string(got.body)))
	}
	// Waits: the adopt.state terminal line; the script's SIGTERM wait runs its
	// full bound first, so the read is bounded by the wait plus the start.
	var terminal string
	if !pollUntil(ctx, adoptHostWait, func() bool {
		for _, l := range adoptStateLines(agentsRoot, sid) {
			if adoptHostTerminal.MatchString(l) {
				terminal = l
				return true
			}
		}
		return false
	}) {
		return fail("no adopt.state terminal line within %s after the link dropped; the outside process alive: %v", adoptHostWait, pidAlive(pid))
	}
	if !strings.HasPrefix(terminal, "ok ") {
		return fail("the script ended with %q, want ok <generation>", terminal)
	}
	if r := cliWithTimeout(ctx, e, adoptCLIWait, "session", "resume", "--id", sid); r.Exit != 0 {
		return fail("relay session resume exited %d: %s; the outside process alive: %v", r.Exit, firstLine(r.Stderr), pidAlive(pid))
	}
	marker := tag + "-2"
	r := cliWithTimeout(ctx, e, adoptCLIWait, "session", "message", "--id", sid, "--text", "Reply with exactly: "+marker, "--json")
	var rep struct {
		Text string `json:"text"`
	}
	if r.Exit != 0 || json.Unmarshal([]byte(r.Stdout), &rep) != nil || !strings.Contains(rep.Text, marker) {
		return fail("a send after the resume: exit %d, reply %q", r.Exit, tail(rep.Text, 80))
	}
	if !adoptAssistantHas(convID, marker) {
		return fail("the reply did not land in the conversation's own transcript")
	}
	return result{id, statePass, "the adopt's ssh client was killed at the script's SIGTERM line: the adopt answered 502 host_link_lost, the script finished alone (" + terminal + "), relay session resume answered 0 and a send replied in the same transcript"}
}

func adoptStateLines(root, sid string) []string {
	raw, err := os.ReadFile(filepath.Join(root, sid, "adopt.state"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// adoptSSHClientPID is the adopt's ssh client: a process named ssh, a child
// of relay-sessions, whose remote command is the adopt's. Deliberate: relay
// wraps the whole remote script as base64 inside an sh -c launcher, so the
// words relay-adopt and the session id appear only in the decoded payload,
// which is matched. The detached script (relay-adopt-run) is a shell, never a
// process named ssh, so the name test keeps it out.
func adoptSSHClientPID(ctx context.Context, sid string) int {
	out, err := exec.CommandContext(ctx, "ps", "-axwwo", "pid=,ppid=,comm=,command=").Output()
	if err != nil {
		return 0
	}
	type proc struct {
		ppid          int
		comm, cmdline string
	}
	procs := map[int]proc{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 == nil && e2 == nil {
			procs[pid] = proc{ppid, filepath.Base(f[2]), strings.Join(f[3:], " ")}
		}
	}
	for pid, p := range procs {
		parent := procs[p.ppid].cmdline
		if p.comm != "ssh" || !strings.Contains(parent, "relaysessions") && !strings.Contains(parent, "relay-sessions") {
			continue
		}
		if script := adoptLauncherScript(p.cmdline); strings.Contains(script, "relay-adopt") && strings.Contains(script, sid) {
			return pid
		}
	}
	return 0
}

var adoptLauncherPayload = regexp.MustCompile(`printf %s ([A-Za-z0-9+/=]+) \| base64 -d`)

// adoptLauncherScript is the script inside the base64 launcher of an ssh
// command line, or the line itself when it holds no launcher.
func adoptLauncherScript(cmdline string) string {
	m := adoptLauncherPayload.FindStringSubmatch(cmdline)
	if m == nil {
		return cmdline
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return cmdline
	}
	return string(b)
}

// adoptHostSend is one Chief of Staff send.
func adoptHostSend(ctx context.Context, e env, token, sid, text string) (int, string) {
	r := cosRequest(ctx, e, token, true, http.MethodPost, "/api/chief-of-staff/messages",
		jsonBody(map[string]string{"sessionId": sid, "text": text}), frontendRequestTimeout)
	return r.Status, r.Error + string(r.Body)
}

// adoptHostTurn sends one message, reads its reply from the conversation's
// transcript and waits for the session to settle. It returns a problem text.
func adoptHostTurn(ctx context.Context, e env, token, sid, convID, marker string) string {
	if st, body := adoptHostSend(ctx, e, token, sid, "Reply with exactly: "+marker); st != http.StatusAccepted {
		return fmt.Sprintf("answered %d, want 202: %s", st, firstLine(body))
	}
	// Waits: none possible for the reply (model output); a bounded read of the
	// transcript, 120 s.
	if !pollUntil(ctx, adoptMarkerWait, func() bool { return adoptAssistantHas(convID, marker) }) {
		return "no reply holding " + marker + " in the conversation's transcript within 120s"
	}
	if w := cliWithTimeout(ctx, e, adoptCLIWait, "session", "wait", "--id", sid, "--json"); w.Exit != 0 {
		return fmt.Sprintf("relay session wait exited %d: %s", w.Exit, firstLine(w.Stderr))
	}
	return ""
}

// adoptHostListProblem checks the session as relay session list shows it:
// its host, headless true and no unsandboxed field.
func adoptHostListProblem(ctx context.Context, e env, sid, hostID string) string {
	r := cliWithTimeout(ctx, e, adoptCLIWait, "session", "list", "--json")
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if r.Exit != 0 || json.Unmarshal([]byte(r.Stdout), &list) != nil {
		return fmt.Sprintf("relay session list exited %d", r.Exit)
	}
	for _, s := range list.Sessions {
		if s["id"] != sid {
			continue
		}
		h, _ := s["host"].(map[string]any)
		if _, has := s["unsandboxed"]; has || s["headless"] != true || h["id"] != hostID {
			return fmt.Sprintf("session lists host %v headless %v unsandboxed present %v, want host %s, headless true and no unsandboxed", h["id"], s["headless"], has, hostID)
		}
		return ""
	}
	return "the session is not listed"
}

// adoptHostTrust answers Claude's folder-trust prompt in the journey's tmux
// session, as answerAdoptTrust does on its own socket: a key goes out only
// when adoptDownGap has passed since the last one. It ends when the
// transcript exists, or when the prompt, once seen, is gone, or at the bound.
func adoptHostTrust(ctx context.Context, tmux, name, convID string) {
	var lastKey time.Time
	var seen bool
	pollUntil(ctx, adoptTrustWait, func() bool {
		if len(adoptTranscripts(convID)) > 0 {
			return true
		}
		pane, _ := exec.CommandContext(ctx, tmux, "capture-pane", "-p", "-J", "-t", "="+name+":").Output()
		text := string(pane)
		if !adoptTrustPrompt.MatchString(text) {
			return seen
		}
		seen = true
		if time.Since(lastKey) < adoptDownGap {
			return false
		}
		for _, l := range strings.Split(text, "\n") {
			if !strings.Contains(l, "❯") {
				continue
			}
			key := ""
			switch {
			case strings.Contains(l, "Yes, I trust"):
				key = "Enter"
			case strings.Contains(l, "No, exit"):
				key = "Down"
			}
			if key != "" {
				_ = exec.CommandContext(ctx, tmux, "send-keys", "-t", "="+name+":", key).Run()
				lastKey = time.Now()
				break
			}
		}
		return false
	})
}
