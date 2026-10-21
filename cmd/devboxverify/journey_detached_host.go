package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/cmdlookup"
)

const (
	detachedHostID     = "session-detached-host"
	detachedStartTries = 30 * time.Second
	detachedLinkWait   = 45 * time.Second
	detachedTurnWait   = 120 * time.Second
	detachedCLIWait    = 30 * time.Second
)

// detachedRunFile is the identity part of runner.json, written by the runner
// on the host once the agent leads its own process group. The file also
// carries "delivered", which moves with every message and is not identity.
type detachedRunFile struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	Started    string `json:"started"`
	Generation string `json:"generation"`
}

// runDetachedHost drives a Claude session on a loopback host with no
// detach_sessions key, so it runs under the runner: a turn is in flight when
// the host link is dropped, the turn still completes, a later message is
// answered with no resume, and a stop ends the agent's process group and
// removes its run directory. The loopback host is this machine and its login
// user is this user, so the agent's run directory is read as a local path.
//
// It runs in the api phase, so it has no run credential and no prompt: the
// host, its project and the agent entry are written into settings.json (the
// power door), which relay's file watch imports.
func runDetachedHost(ctx context.Context, e env) result {
	const id = detachedHostID
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, "fixture host or project: "+err.Error())
	}
	// Deliberate: off a stack the launcher would run claude against the
	// machine's own login, a hosted model.
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	// Deliberate: the journey borrows SIGINT and SIGTERM only to clean up. A
	// signal cancels the journey's context, the sweep and the teardown run
	// (both ignore the cancellation), and the signal is then delivered again so
	// the process still ends as the sender meant.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	caught := make(chan os.Signal, 1)
	go func() {
		select {
		case sig := <-sigs:
			caught <- sig
			cancel()
		case <-ctx.Done():
		}
	}()

	var res result
	fx, err := setUpDetachedFixture(ctx, e, baseURL, model, tokenFile)
	if err != nil {
		res = blocked(id, "fixture host or project: "+err.Error())
	} else {
		res = driveDetachedHost(ctx, e, fx)
		res.Detail += fx.tearDown()
	}

	signal.Stop(sigs)
	select {
	case sig := <-sigs: // arrived after the goroutine stopped listening
		caught <- sig
	default:
	}
	select {
	case sig := <-caught:
		res.Detail += "; " + sig.String() + " received during the journey; cleanup ran"
		if s, ok := sig.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), s)
		}
	default:
	}
	return res
}

// detachedFixture is what the journey seeds into settings.json and the
// machine: a loopback host, a project on it, one agent entry and the
// launcher script that entry runs.
type detachedFixture struct {
	configDir                string
	wsToken                  string // the /ws bearer, a seeded credential of class proxy (the execute credential is refused there); never printed
	credentialID             string
	hostID, projectID, entry string
	projectDir, launcher     string
}

// detachedLauncher is the entry's command. A host session does not inherit
// relay's environment, so the script points claude at the stack's model
// endpoint itself. The token is read from its file at exec and goes into no
// file, log or detail. Without a stack baseURL is empty and claude uses the
// machine's own login.
func detachedLauncher(claudePath, baseURL, model, tokenFile string) string {
	script := "#!/bin/sh\n"
	if baseURL != "" {
		script += "ANTHROPIC_BASE_URL=" + shellQuote(baseURL) + "\n" +
			"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + shellQuote(model) + "\n" +
			"ANTHROPIC_AUTH_TOKEN=$(cat " + shellQuote(tokenFile) + ") || exit 1\n" +
			"export ANTHROPIC_BASE_URL ANTHROPIC_DEFAULT_HAIKU_MODEL ANTHROPIC_AUTH_TOKEN\n"
	}
	return script + "exec " + shellQuote(claudePath) + " \"$@\"\n"
}

// stackClaude reads claude_base_url, claude_model and claude_token_file from
// the stack's stack.json, which sits beside the stack's config dir. All three
// are empty outside a stack and on a stack that runs real Claude.
func stackClaude(e env) (baseURL, model, tokenFile string, err error) {
	if !e.Stack {
		return "", "", "", nil
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(e.ConfigDir), "stack.json"))
	if err != nil {
		return "", "", "", errors.New("stack.json is not readable beside the stack's config dir")
	}
	var s struct {
		BaseURL   string `json:"claude_base_url"`
		Model     string `json:"claude_model"`
		TokenFile string `json:"claude_token_file"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return "", "", "", errors.New("stack.json is not valid JSON")
	}
	if s.BaseURL != "" && (s.Model == "" || !filepath.IsAbs(s.TokenFile)) {
		return "", "", "", errors.New("stack.json names claude_base_url without claude_model and an absolute claude_token_file")
	}
	return s.BaseURL, s.Model, s.TokenFile, nil
}

func setUpDetachedFixture(ctx context.Context, e env, baseURL, model, tokenFile string) (*detachedFixture, error) {
	claude, err := cmdlookup.Local(ctx, "claude")
	if err != nil {
		return nil, fmt.Errorf("no claude on this machine's login-shell PATH to launch: %w", err)
	}
	base := filepath.Join(gateStateDir(), "grant-detached-"+e.Nonce)
	fx := &detachedFixture{
		configDir: e.ConfigDir, hostID: uuid.NewString(), projectID: uuid.NewString(), entry: "verify-" + e.Nonce + "-detached",
		projectDir: base, launcher: base + ".sh",
	}
	if err := os.MkdirAll(fx.projectDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the project folder: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(fx.projectDir); err == nil {
		fx.projectDir = resolved
	}
	if err := os.WriteFile(fx.launcher, []byte(detachedLauncher(claude, baseURL, model, tokenFile)), 0o700); err != nil {
		fx.tearDown()
		return nil, fmt.Errorf("cannot write the launcher: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		fx.tearDown()
		return nil, err
	}
	projectToken := hex.EncodeToString(secret)
	tokenHash := sha256.Sum256([]byte(projectToken))
	wsSecret := make([]byte, 32)
	if _, err := rand.Read(wsSecret); err != nil {
		fx.tearDown()
		return nil, err
	}
	fx.wsToken = hex.EncodeToString(wsSecret)
	wsHash := sha256.Sum256([]byte(fx.wsToken))
	fx.credentialID = uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	host := map[string]any{
		"id": fx.hostID, "name": dropInHostPrefix + e.Nonce + "-detach", "target": dropInHostTarget,
		"created_at": now,
		// A hosted project launches the host's own templates; the probe
		// seeds them on a host added through the API, and this host is not.
		"terminal_templates": []map[string]string{
			{"id": "shell", "name": "Shell"},
			{"id": "claude-code", "name": "Claude Code", "command": claude},
		},
	}
	project := map[string]any{
		"id": fx.projectID, "name": dropInProjectName + e.Nonce + " detach", "path": fx.projectDir, "host_id": fx.hostID,
		"allowed_mcp_ids": []string{}, "allowed_models": []string{"*"}, "allowed_templates": []string{"claude-code"},
		"token": projectToken, "token_hash": hex.EncodeToString(tokenHash[:]), "created_at": now,
		"agents": []map[string]string{{"name": fx.entry, "kind": "claude", "model": agentModel, "command": fx.launcher}},
	}
	credential := map[string]any{
		"id": fx.credentialID, "name": "verify-" + e.Nonce + "-detached-ws", "hash": hex.EncodeToString(wsHash[:]),
		"classes": []string{"proxy"}, "created": now, "expires": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	if err := editSettings(e.ConfigDir, func(top map[string]json.RawMessage) error {
		return appendRecords(top, map[string]any{"hosts": host, "projects": project, "api_credentials": credential})
	}); err != nil {
		fx.tearDown()
		return nil, err
	}
	return fx, nil
}

// tearDown removes the seeded records and files, whatever the journey did.
// It returns text for the result detail, empty when all went well.
func (fx *detachedFixture) tearDown() (problem string) {
	if fx.hostID != "" {
		if err := editSettings(fx.configDir, func(top map[string]json.RawMessage) error {
			dropRecords(top, "hosts", fx.hostID)
			dropRecords(top, "projects", fx.projectID)
			dropRecords(top, "api_credentials", fx.credentialID)
			return nil
		}); err != nil {
			problem += "; teardown: " + err.Error()
		}
	}
	if err := os.RemoveAll(fx.projectDir); err != nil {
		problem += "; teardown: cannot remove the project folder"
	}
	if err := os.Remove(fx.launcher); err != nil && !errors.Is(err, os.ErrNotExist) {
		problem += "; teardown: cannot remove the launcher"
	}
	return problem
}

// detachedSweep ends everything this run started: every session id (including
// those the start retry loop abandoned), the agent's process group and the
// runner processes. It touches only the run directories of its own sessions.
type detachedSweep struct {
	agentsRoot string
	sids       []string
	pgid       int // the first runner.json's group, 0 until known
}

func (w *detachedSweep) runDir(sid string) string { return filepath.Join(w.agentsRoot, sid) }

// holders returns the pids whose command line names one of the swept run
// directories. Deliberate: the runner leads its own process group and is
// reparented to init, so its run-directory path is the only handle on it. The
// match is the full directory path with a trailing slash, never a bare id.
func (w *detachedSweep) holders(ctx context.Context) []int {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pidText, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, perr := strconv.Atoi(pidText)
		if !ok || perr != nil || pid == os.Getpid() {
			continue
		}
		for _, sid := range w.sids {
			if strings.Contains(cmd, w.runDir(sid)+string(filepath.Separator)) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func (w *detachedSweep) run(ctx context.Context, e env) {
	ctx = context.WithoutCancel(ctx)
	for _, sid := range w.sids {
		cliStopSession(ctx, e, sid)
	}
	if w.pgid > 1 && groupAlive(w.pgid) { // only our own agent's group, read from its runner.json
		_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	}
	signalAll := func(sig syscall.Signal) {
		for _, pid := range w.holders(ctx) {
			_ = syscall.Kill(pid, sig)
		}
	}
	signalAll(syscall.SIGTERM)
	// Waits: process exit after a signal has no event; polled, bounded.
	if !pollUntil(ctx, 5*time.Second, func() bool { return len(w.holders(ctx)) == 0 }) {
		signalAll(syscall.SIGKILL)
		pollUntil(ctx, 2*time.Second, func() bool { return len(w.holders(ctx)) == 0 })
	}
	for _, sid := range w.sids {
		_ = os.RemoveAll(w.runDir(sid))
	}
}

// finish runs the sweep, then turns res into FAIL when a process still names a
// swept run directory. An existing FAIL or BLOCKED keeps its state.
func (w *detachedSweep) finish(ctx context.Context, e env, res result) result {
	if len(w.sids) == 0 {
		return res
	}
	w.run(ctx, e)
	left := w.holders(context.WithoutCancel(ctx))
	if len(left) == 0 {
		return res
	}
	note := fmt.Sprintf("cleanup: pids %v still name a started session's run directory", left)
	if res.State == statePass {
		return result{res.ID, stateFail, note}
	}
	res.Detail += "; " + note
	return res
}

func driveDetachedHost(ctx context.Context, e env, fx *detachedFixture) (res result) {
	const id = detachedHostID
	agentsRoot, rootErr := hostAgentsRoot()
	sweep := &detachedSweep{agentsRoot: agentsRoot}
	defer func() { res = sweep.finish(ctx, e, res) }()
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	blockedf := func(f string, a ...any) result { return blocked(id, fmt.Sprintf(f, a...)) }

	hostID, projectID := fx.hostID, fx.projectID
	if rootErr != nil {
		return blockedf("%v", rootErr)
	}
	var err error

	// Waits: none possible for the settings import; relay's file watch emits
	// no event. The seeded credential opens /ws only once the import has run,
	// so a refused dial is retried, bounded.
	var conn *websocket.Conn
	var status int
	if !pollUntil(ctx, detachedStartTries, func() bool {
		conn, status, err = dialFrontendWS(ctx, e, fx.wsToken)
		return err == nil
	}) {
		return blockedf("GET /ws status %d: %v", status, err)
	}
	defer func() { _ = conn.Close() }()
	log := &dropLog{}
	collectFrames(conn, log)

	// A detached start returns only once runner.json exists, so a start that
	// left no run directory is stopped and started again, bounded.
	var sid string
	var first detachedRunFile
	started := pollUntil(ctx, detachedStartTries, func() bool {
		var s string
		s, err = startHostSession(ctx, e, projectID, fx.entry)
		if err != nil {
			return false
		}
		sweep.sids = append(sweep.sids, s)
		if f, rerr := readRunFile(agentsRoot, s, "g1"); rerr == nil {
			sid, first = s, f
			return true
		}
		cliStopSession(ctx, e, s)
		return false
	})
	sweep.pgid = first.PGID
	if !started {
		if err != nil {
			return fail("relay session start: %v", err)
		}
		return fail("a session on a host with no detach_sessions key left no runner.json under the agents directory")
	}
	if first.PID <= 1 || first.PGID <= 1 || first.Started == "" || !pidAlive(first.PID) {
		return fail("runner.json after start reads %+v, or its pid is not alive", first)
	}

	_ = wsSend(conn, map[string]string{"type": "join_session", "sessionId": sid})
	if !log.waitFor(ctx, agentFrameWait, frameSeen(0, func(f dropFrame) bool { return f.Type == "session_joined" && f.SessionID == sid })) {
		return fail("no session_joined frame after join_session")
	}
	mark := len(log.snapshot())

	// The long answer keeps the turn open across the drop. session send
	// returns once relay holds the runner's receipt, so the drop never lands
	// on a send still in flight (that ends unknown); the answer is read with
	// session wait after the link returns.
	marker1 := "verify-" + e.Nonce + "-turn"
	send := cliWithTimeout(ctx, e, detachedCLIWait, "session", "send", "--id", sid, "--json", "--text",
		"Count from 1 to 150, one number per line, then on a last line write exactly: "+marker1)
	if send.Exit != 0 {
		return fail("relay session send exited %d: %s", send.Exit, firstLine(send.Stderr))
	}
	if !log.waitFor(ctx, agentFrameWait, stateSeq(sid, mark, "running")) {
		return fail("the session never reported running after relay session send")
	}
	if stateSeq(sid, mark, "running", "idle")(log.snapshot()) {
		return blockedf("the turn ended before the link was dropped; nothing was in flight")
	}

	dis := cliWithTimeout(ctx, e, detachedCLIWait, "host", "disconnect", "--id", hostID, "--json")
	if dis.Exit != 0 {
		return fail("relay host disconnect exited %d: %s", dis.Exit, firstLine(dis.Stderr))
	}
	if !pollUntil(ctx, detachedLinkWait, func() bool { return hostLinkSeen(e, sid, "lost") }) {
		return fail("no session.host_link link=lost event within %s of relay host disconnect", detachedLinkWait)
	}
	if !pollUntil(ctx, detachedLinkWait, func() bool { return hostLinkSeen(e, sid, "restored") }) {
		return fail("no session.host_link link=restored event within %s", detachedLinkWait)
	}

	wait := cliWithTimeout(ctx, e, detachedTurnWait, "session", "wait", "--id", sid, "--json")
	var turnReply struct {
		State   string `json:"state"`
		Excerpt string `json:"excerpt"`
	}
	_ = json.Unmarshal([]byte(wait.Stdout), &turnReply)
	if wait.Exit != 0 || !bytes.Contains([]byte(turnReply.Excerpt), []byte(marker1)) {
		return fail("the turn in flight across the drop did not return its answer: exit %d, state %q, stderr %q, reply %q", wait.Exit, turnReply.State, firstLine(wait.Stderr), tail(turnReply.Excerpt, 80))
	}
	if !log.waitFor(ctx, agentFrameWait, stateSeq(sid, mark, "running", "idle")) {
		return fail("the session never returned to idle after the turn")
	}

	marker2 := "verify-" + e.Nonce + "-after"
	r := cliWithTimeout(ctx, e, detachedTurnWait, "session", "message", "--id", sid, "--json", "--text", "Reply with exactly: "+marker2)
	var reply struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(r.Stdout), &reply)
	if r.Exit != 0 || !bytes.Contains([]byte(reply.Text), []byte(marker2)) {
		return fail("relay session message after the link returned: exit %d, stderr %q, reply %q", r.Exit, firstLine(r.Stderr), tail(reply.Text, 80))
	}

	after, err := readRunFile(agentsRoot, sid, "g1")
	if err != nil {
		return fail("runner.json unreadable after the drop: %v", err)
	}
	if after != first {
		return fail("runner.json changed across the drop: before %+v, after %+v", first, after)
	}
	if !pidAlive(after.PID) {
		return fail("agent pid %d is not alive after the drop", after.PID)
	}
	if gens, _ := os.ReadDir(filepath.Join(agentsRoot, sid)); len(gens) != 1 {
		return fail("the session has %d generation directories after the drop, want 1 (a resume started another)", len(gens))
	}

	stop := cliWithTimeout(ctx, e, detachedCLIWait, "session", "stop", "--id", sid, "--json")
	if stop.Exit != 0 {
		return fail("relay session stop exited %d: %s", stop.Exit, firstLine(stop.Stderr))
	}
	if groupAlive(first.PGID) {
		return fail("process group %d is still alive after relay session stop", first.PGID)
	}
	if _, err := os.Stat(filepath.Join(agentsRoot, sid)); !errors.Is(err, os.ErrNotExist) {
		return fail("the run directory for the stopped session still exists")
	}
	return result{id, statePass, fmt.Sprintf("agent pid %d lived through relay host disconnect (link lost, then restored); the turn in flight returned, a later message was answered with no resume, and stop ended group %d and removed the run directory", first.PID, first.PGID)}
}

func hostAgentsRoot() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(h, ".relay", "agents"), nil
}

func readRunFile(root, sid, gen string) (detachedRunFile, error) {
	var f detachedRunFile
	raw, err := os.ReadFile(filepath.Join(root, sid, gen, "runner.json"))
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("runner.json is not JSON: %w", err)
	}
	return f, nil
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func cliWithTimeout(ctx context.Context, e env, d time.Duration, args ...string) cliResult {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return runRelayAt(ctx, e, e.ConfigDir, "", args...)
}

func firstLine(s string) string {
	line, _, _ := bytes.Cut([]byte(s), []byte("\n"))
	return string(line)
}

// startHostSession is relay session start by the project's agent entry.
func startHostSession(ctx context.Context, e env, projectID, entry string) (string, error) {
	r := cliWithTimeout(ctx, e, 60*time.Second, "session", "start", "--project", projectID, "--entry", entry,
		"--settings", `{"headless":true,"agent":true}`, "--json")
	if r.Exit != 0 {
		return "", fmt.Errorf("exit %d: %s", r.Exit, firstLine(r.Stderr))
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(r.Stdout), &s) != nil || s.SessionID == "" {
		return "", errors.New("no sessionId in the start reply")
	}
	return s.SessionID, nil
}

func cliStopSession(ctx context.Context, e env, sid string) {
	cliWithTimeout(ctx, e, detachedCLIWait, "session", "stop", "--id", sid)
}

// editSettings rewrites settings.json by temp file and rename, which relay's
// file watch imports. This is the power-user door: the host's own fields have
// no API route.
func editSettings(dir string, edit func(top map[string]json.RawMessage) error) error {
	path := filepath.Join(dir, "settings.json")
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("settings.json is not readable JSON: %w", err)
	}
	if err := edit(top); err != nil {
		return err
	}
	next, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-detach-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(next); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// appendRecords adds one record to each named top-level array.
func appendRecords(top map[string]json.RawMessage, add map[string]any) error {
	for key, rec := range add {
		var list []json.RawMessage
		if raw := top[key]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("settings.json %s: %w", key, err)
			}
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		top[key], _ = json.Marshal(append(list, b))
	}
	return nil
}

// dropRecords removes the record with this id from a top-level array.
func dropRecords(top map[string]json.RawMessage, key, id string) {
	var list []json.RawMessage
	if json.Unmarshal(top[key], &list) != nil {
		return
	}
	kept := list[:0]
	for _, rec := range list {
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(rec, &r) == nil && r.ID == id {
			continue
		}
		kept = append(kept, rec)
	}
	top[key], _ = json.Marshal(kept)
}

// hostLinkSeen reports whether relay-sessions logged op=session.host_link for
// the session with the given link state.
func hostLinkSeen(e env, sid, link string) bool {
	f, err := os.Open(filepath.Join(e.ConfigDir, "logs", "relaysessions.log"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > toolSearchLogTailSize {
		_, _ = f.Seek(fi.Size()-toolSearchLogTailSize, io.SeekStart)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var l struct {
			Op        string `json:"op"`
			SessionID string `json:"session_id"`
			Link      string `json:"link"`
		}
		if json.Unmarshal(line, &l) == nil && l.Op == "session.host_link" && l.SessionID == sid && l.Link == link {
			return true
		}
	}
	return false
}
