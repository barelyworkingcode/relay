package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	adoptID         = "session-adopt"
	adoptUnsID      = "session-adopt-unsandboxed"
	adoptTrustWait  = 30 * time.Second
	adoptMarkerWait = 120 * time.Second
	adoptIdleWait   = 60 * time.Second
	adoptCLIWait    = 180 * time.Second
	adoptDoorWait   = 60 * time.Second
	adoptDownGap    = 1500 * time.Millisecond
)

var adoptJourneys = []journey{
	{adoptID, []string{"sessions", "audit"}, []string{"project:acme"}, phaseAPI, 240 * time.Second, runAdopt},
	{adoptUnsID, []string{"sessions", "audit"}, nil, phaseAPI, 300 * time.Second, runAdoptUnsandboxed},
	{adoptHostID, []string{"sessions", "audit"}, nil, phaseAPI, 600 * time.Second, runAdoptHost},
	{adoptHostDropID, []string{"sessions", "audit"}, nil, phaseAPI, 600 * time.Second, runAdoptHostDrop},
}

var adoptTrustPrompt = regexp.MustCompile(`(?i)trust (this|the files in this) folder|Do you trust|Yes, I trust`)

// adoptRun is what the journey saw, for classifyAdopt.
type adoptRun struct {
	Status      int
	Body        []byte
	ClaudeID    string
	PIDGone     bool
	Reply       string
	ReplyInFile bool
	Audit       []audit.AuditEvent

	// Unsandboxed journey only.
	Unsandboxed bool
	SendStatus  int
	SendDetail  string
}

// runAdopt drives the simple path of the adopt door: an interactive Claude
// started in a terminal (a detached tmux session) in a scratch folder under
// Acme Corp's folder is adopted by its conversation id, and the same
// conversation answers a message as a headless relay session.
//
// Deliberate: the outside claude runs against the stack's local model
// endpoint only. Off a stack, or on a stack with no claude_base_url, the
// journey is NOTRUN, so it never runs claude against a hosted model. The
// token is read by the shell at exec from its file; it is in no argv, file
// or detail.
func runAdopt(ctx context.Context, e env) result { return adoptFlow(ctx, e, false) }

// runAdoptUnsandboxed is runAdopt for a terminal Claude in a scratch folder
// outside every project: relay adopts it with no project, no sandbox and no
// permission policy, and the Chief of Staff can still send to it.
func runAdoptUnsandboxed(ctx context.Context, e env) result { return adoptFlow(ctx, e, true) }

func adoptFlow(ctx context.Context, e env, unsandboxed bool) result {
	id, suffix := adoptID, "adopt"
	if unsandboxed {
		id, suffix = adoptUnsID, "adopt-uns"
	}
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, err.Error())
	}
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	tag := "verify-" + e.Nonce + "-" + suffix
	var dir string
	if !unsandboxed {
		acme, err := e.World.project("acme")
		if err != nil {
			return blocked(id, err.Error())
		}
		folder, err := filepath.EvalSymlinks(acme.Folder)
		if err != nil {
			return blocked(id, acme.Name+" folder missing")
		}
		dir = filepath.Join(folder, tag)
	}
	token, err := adoptCredential(e.CredentialFile)
	if err != nil {
		return blocked(id, err.Error())
	}
	tmux := adoptTmux()
	claude, cerr := cmdlookup.Local(ctx, "claude")
	switch {
	case tmux == "":
		return blocked(id, "tmux is not installed")
	case cerr != nil:
		return blocked(id, "no claude on this machine's login-shell PATH")
	}

	if unsandboxed {
		// Deliberate: the folder is under the OS temp directory, which no
		// registered project holds, so relay finds the row in no project. It is
		// made after every BLOCKED check above, so none leaves it behind.
		tmp, err := os.MkdirTemp("", tag+"-")
		if err != nil {
			return blocked(id, "scratch folder: "+err.Error())
		}
		if dir, err = filepath.EvalSymlinks(tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return blocked(id, "scratch folder: "+err.Error())
		}
	}
	convID := uuid.NewString()
	sess := &adoptCleanup{tmux: tmux, name: tag, dir: dir, convID: convID, tag: tag, e: e, id: id}
	defer sess.run(context.WithoutCancel(ctx))

	if err := writeAdoptSettings(dir); err != nil {
		return blocked(id, "scratch folder: "+err.Error())
	}
	line := adoptShellPrefix(baseURL, model, tokenFile) + "exec env -u CLAUDE_CODE_ENTRYPOINT -u CLAUDECODE " + shellQuote(claude) +
		" --model haiku --session-id " + convID + " --permission-mode default " + shellQuote("Reply with exactly: "+tag+"-1")
	create := adoptTmuxCmd(ctx, tmux, tag, "new-session", "-d", "-s", tag, "-x", "200", "-y", "50", "-c", dir, line)
	// Deliberate: a person's terminal has neither variable. Inside a Claude
	// agent session both are set, and claude then registers itself as
	// entrypoint "sdk-cli", which the door refuses as not_command_line.
	create.Env = adoptCleanEnv(os.Environ())
	if out, err := create.CombinedOutput(); err != nil {
		return blocked(id, "tmux new-session: "+firstLine(string(out)))
	}
	sess.started = true

	// Waits: none possible: a terminal UI has no signal; pane capture, 30 s.
	answerAdoptTrust(ctx, tmux, tag, convID)

	// Waits: none possible for the outside reply: model output; a bounded read
	// of the transcript for the marker record, 120 s.
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
	if got, err := claudeSessionForPID(pid); err != nil || got != convID {
		return blocked(id, "the registry entry does not carry the conversation id the journey set")
	}

	r := adoptRun{ClaudeID: convID}
	body, _ := json.Marshal(map[string]any{"claudeSessionId": convID, "interrupt": false, "model": "haiku"})
	r.Status, r.Body, err = adoptPost(ctx, e.FrontendSocket, token, body)
	if err != nil {
		return blocked(id, "frontend socket unreachable")
	}
	var ok struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Body, &ok)
	sess.sessionID = ok.SessionID

	r.PIDGone = syscall.Kill(pid, 0) == syscall.ESRCH
	if r.Status == http.StatusCreated && ok.SessionID != "" {
		reply := cliWithTimeout(ctx, e, adoptCLIWait, "session", "message", "--id", ok.SessionID, "--text", "Reply with exactly: "+tag+"-2", "--json")
		var rep struct {
			Text string `json:"text"`
		}
		if reply.Exit == 0 && json.Unmarshal([]byte(reply.Stdout), &rep) == nil {
			r.Reply = rep.Text
		}
		r.ReplyInFile = adoptAssistantHas(convID, tag+"-2")
	}
	if unsandboxed && r.Status == http.StatusCreated && ok.SessionID != "" {
		r.Unsandboxed = true
		r.SendStatus, r.SendDetail = adoptCosSend(ctx, e, ok.SessionID, "Reply with exactly: "+tag+"-3")
	}
	r.Audit = adoptAuditRows(ctx, e, convID)
	if unsandboxed {
		return classifyAdoptUnsandboxed(r, tag)
	}
	return classifyAdopt(r, tag)
}

func classifyAdopt(r adoptRun, tag string) result {
	fail := func(d string) result { return result{adoptID, stateFail, d} }
	if r.Status != http.StatusCreated {
		return fail(fmt.Sprintf("adopt answered %d, want 201: %s", r.Status, firstLine(string(r.Body))))
	}
	var got struct {
		SessionID, ClaudeSessionID string
		Interrupted, RecoverySent  bool
	}
	if json.Unmarshal(r.Body, &got) != nil || got.SessionID == "" {
		return fail("201 body holds no sessionId")
	}
	var intent, done bool
	for _, row := range r.Audit {
		switch {
		case row.Phase == "intent":
			intent = true
		case row.Phase == "completion" && row.Outcome == "ok":
			done = true
		}
	}
	switch {
	case got.ClaudeSessionID != r.ClaudeID:
		return fail("201 names another conversation than the one adopted")
	case got.Interrupted || got.RecoverySent:
		return fail("an idle adopt reported an interrupt or a recovery message")
	case !r.PIDGone:
		return fail("the terminal's claude process was still alive after 201")
	case !strings.Contains(r.Reply, tag+"-2"):
		return fail("the adopted session's reply lacks " + tag + "-2")
	case !r.ReplyInFile:
		return fail("the reply did not land in the conversation's own transcript")
	case !intent:
		return fail("no session_adopt intent row")
	case !done:
		return fail("no session_adopt completion row with outcome ok")
	}
	return result{adoptID, statePass, "idle terminal Claude adopted by conversation id: 201, terminal process gone, the resumed headless session replied in the same transcript, session_adopt intent and ok completion audited"}
}

func classifyAdoptUnsandboxed(r adoptRun, tag string) result {
	fail := func(d string) result { return result{adoptUnsID, stateFail, d} }
	base := classifyAdopt(r, tag)
	if base.State != statePass {
		base.ID = adoptUnsID
		return base
	}
	var got struct {
		Unsandboxed bool `json:"unsandboxed"`
	}
	_ = json.Unmarshal(r.Body, &got)
	var intent, done bool
	for _, row := range r.Audit {
		var args struct {
			NoProject bool `json:"no_project"`
		}
		if json.Unmarshal(row.Args, &args) != nil || !args.NoProject {
			return fail("a session_adopt row lacks no_project: true (" + row.Phase + ")")
		}
		intent = intent || row.Phase == "intent"
		done = done || row.Phase == "completion" && row.Outcome == "ok"
	}
	switch {
	case !got.Unsandboxed:
		return fail("201 body lacks unsandboxed: true")
	case r.SendStatus != http.StatusAccepted:
		return fail(fmt.Sprintf("Chief of Staff send answered %d, want 202: %s", r.SendStatus, r.SendDetail))
	case !intent || !done:
		return fail("session_adopt rows with no_project: true lack the intent or the ok completion")
	}
	return result{adoptUnsID, statePass, "terminal Claude in a folder outside every project adopted unsandboxed: 201 with unsandboxed true, terminal process gone, the headless session replied in the same transcript, a Chief of Staff send answered 202, session_adopt rows audited with no_project true"}
}

// adoptCosSend sends one message to the session as the Chief of Staff, with a
// proxy credential seeded into settings.json for the call and dropped after
// it. Deliberate: a presence-gated mint needs the console, which a stack run
// has not; the seeded record is how session-swap-back and session-detached-host
// get theirs. The scope needs class proxy only. The token goes into no file,
// log or detail beyond the hash settings.json keeps.
func adoptCosSend(ctx context.Context, e env, sessionID, text string) (status int, detail string) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return 0, "credential: " + err.Error()
	}
	token := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	credID := uuid.NewString()
	cred := map[string]any{
		"id": credID, "name": "verify-" + e.Nonce + "-adopt-cos", "hash": hex.EncodeToString(hash[:]),
		"classes": []string{"proxy"}, "created": time.Now().UTC().Format(time.RFC3339),
		"expires": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	if err := editSettings(e.ConfigDir, func(top map[string]json.RawMessage) error {
		return appendRecords(top, map[string]any{"api_credentials": cred})
	}); err != nil {
		return 0, "seeding the credential: " + err.Error()
	}
	defer func() {
		if err := editSettings(e.ConfigDir, func(top map[string]json.RawMessage) error {
			dropRecords(top, "api_credentials", credID)
			return nil
		}); err != nil {
			detail += "; teardown: " + err.Error()
		}
	}()
	// Waits: none possible for the settings import; relay's file watch emits
	// no event. The seeded credential answers 200 once the import has run, so
	// the scoped read is retried, bounded.
	if !pollUntil(ctx, detachedStartTries, func() bool {
		return cosRequest(ctx, e, token, true, http.MethodGet, "/api/sessions", nil, frontendRequestTimeout).Status == http.StatusOK
	}) {
		return 0, "the seeded credential was not imported within " + detachedStartTries.String()
	}
	resp := cosRequest(ctx, e, token, true, http.MethodPost, "/api/chief-of-staff/messages",
		jsonBody(map[string]string{"sessionId": sessionID, "text": text}), frontendRequestTimeout)
	return resp.Status, firstLine(resp.Error)
}

// adoptCredential reads the execute credential the way the other journeys do.
func adoptCredential(file string) (string, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return "", errors.New("credential file missing")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", errors.New("credential file not mode 0600")
	}
	raw, err := os.ReadFile(file)
	if t := strings.TrimSpace(string(raw)); err == nil && t != "" {
		return t, nil
	}
	return "", errors.New("credential file empty or unreadable")
}

func adoptPost(ctx context.Context, socket, token string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, adoptDoorWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay/api/sessions/adopt", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func adoptAuditRows(ctx context.Context, e env, convID string) []audit.AuditEvent {
	out, err := e.relayExec(ctx, "audit", "--event", audit.AuditEventSessionAdopt, "--grep", convID, "--json", "--tail", "10").Output()
	if err != nil {
		return nil
	}
	var rows []audit.AuditEvent
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var row audit.AuditEvent
		if json.Unmarshal(sc.Bytes(), &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func adoptTmux() string {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// writeAdoptSettings makes the scratch folder and the claude settings that
// keep the attribution header (a per-session hash that defeats the endpoint's
// prefix cache) and the output cap off the outside claude.
func writeAdoptSettings(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		return err
	}
	const settings = `{"env":{"CLAUDE_CODE_ATTRIBUTION_HEADER":"0","CLAUDE_CODE_MAX_OUTPUT_TOKENS":"8000"}}` + "\n"
	return os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(settings), 0o644)
}

// adoptShellPrefix points the outside claude at the stack's endpoint. The
// shell reads the token file at exec, so the token is in no tmux argv.
func adoptShellPrefix(baseURL, model, tokenFile string) string {
	return "[ -s " + shellQuote(tokenFile) + " ] || { echo 'the model token file is empty or unreadable' >&2; exit 1; }; " +
		"ANTHROPIC_BASE_URL=" + shellQuote(baseURL) + " ANTHROPIC_DEFAULT_HAIKU_MODEL=" + shellQuote(model) +
		" ANTHROPIC_AUTH_TOKEN=\"$(cat " + shellQuote(tokenFile) + ")\" "
}

// adoptTmuxCmd runs tmux on the journey's own socket, so a server another run
// left behind (and the folders it holds) is never used.
func adoptTmuxCmd(ctx context.Context, tmux, sock string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, tmux, append([]string{"-L", sock}, args...)...)
}

func adoptCleanEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") || strings.HasPrefix(kv, "CLAUDECODE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func adoptPane(tmux, name string) string {
	out, _ := adoptTmuxCmd(context.Background(), tmux, name, "capture-pane", "-p", "-J", "-t", name).Output()
	return string(out)
}

// answerAdoptTrust confirms Claude's folder-trust prompt. Deliberate: the
// terminal ignores keys sent before it accepts input and its redraw lags
// them, so a key goes out only when adoptDownGap has passed since the last key
// of any kind, chosen from where ❯ shows on the latest capture: Down on "No,
// exit", Enter on "Yes, I trust". A queued key that moves ❯ off Yes is
// corrected on a later poll. It ends when the transcript exists, or when the
// prompt, once seen, is gone, or at the bound.
func answerAdoptTrust(ctx context.Context, tmux, name, convID string) {
	var lastKey time.Time
	var seen bool
	pollUntil(ctx, adoptTrustWait, func() bool {
		if len(adoptTranscripts(convID)) > 0 {
			return true
		}
		text := adoptPane(tmux, name)
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
				_ = adoptTmuxCmd(ctx, tmux, name, "send-keys", "-t", name, key).Run()
				lastKey = time.Now()
				break
			}
		}
		return false
	})
}

func adoptTranscripts(convID string) []string {
	h, err := os.UserHomeDir()
	if err != nil || !validUUID(convID) {
		return nil
	}
	m, _ := filepath.Glob(filepath.Join(h, ".claude", "projects", "*", convID+".jsonl"))
	return m
}

// adoptAssistantHas is true when the conversation's transcript holds an
// assistant record that contains text. The user's prompt holds the marker too,
// so only an assistant record counts.
func adoptAssistantHas(convID, text string) bool {
	for _, f := range adoptTranscripts(convID) {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range bytes.Split(raw, []byte("\n")) {
			var rec struct {
				Type string `json:"type"`
			}
			if bytes.Contains(l, []byte(text)) && json.Unmarshal(l, &rec) == nil && rec.Type == "assistant" {
				return true
			}
		}
	}
	return false
}

func adoptPID(ctx context.Context, convID string) int {
	out, err := exec.CommandContext(ctx, "ps", "-axwwo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	return claudePIDForArgv(string(out), convID)
}

func adoptRegistryStatus(pid int) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(h, ".claude", "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return ""
	}
	var rec struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(raw, &rec)
	return rec.Status
}

// adoptCleanup undoes what the journey made, whatever its result: the relay
// session, the tmux session and its claude, the transcript, the folder.
type adoptCleanup struct {
	tmux, name, dir, convID, tag, sessionID string
	started                                 bool
	e                                       env
	id                                      string
}

func (c *adoptCleanup) run(ctx context.Context) {
	if c.sessionID != "" {
		cliWithTimeout(ctx, c.e, 30*time.Second, "session", "stop", "--id", c.sessionID)
	}
	if c.started {
		_ = adoptTmuxCmd(ctx, c.tmux, c.name, "kill-server").Run()
	}
	if pid := adoptPID(ctx, c.convID); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	removeTranscript(c.id, c.convID, c.tag, false, func(ctx context.Context) string {
		if adoptPID(ctx, c.convID) > 0 {
			return "process still running"
		}
		return ""
	})
	if c.dir != "" && strings.HasPrefix(filepath.Base(c.dir), "verify-") {
		_ = os.RemoveAll(c.dir)
	}
}
