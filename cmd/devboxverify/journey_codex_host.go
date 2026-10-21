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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/cmdlookup"
)

const (
	codexHostID         = "session-codex-host-runner"
	codexHostAnswerWait = 20 * time.Second
	// codexKeyEnv is the variable codex's custom provider reads its bearer from.
	codexKeyEnv = "RELAY_VERIFY_LOCAL_KEY"
)

// codexHostLauncher is the entry's and the host codex template's command. A
// host session does not inherit relay's environment, so the script builds a
// scratch codex home whose config names one provider, the stack's local model
// endpoint, and no login. The bearer is read from its file at exec into the
// environment variable codex's env_key names; it goes into no file, log or
// detail.
func codexHostLauncher(codexPath, baseURL, model, tokenFile, home string) string {
	cfg := "model = " + tomlString(model) + "\n" +
		"model_provider = \"local\"\n" +
		"[model_providers.local]\n" +
		"name = \"local\"\n" +
		"base_url = " + tomlString(baseURL+"/v1") + "\n" +
		"wire_api = \"responses\"\n" +
		"env_key = " + tomlString(codexKeyEnv) + "\n"
	return "#!/bin/sh\n" +
		"CODEX_HOME=" + shellQuote(home) + "\n" +
		"mkdir -p \"$CODEX_HOME\" || exit 1\n" +
		"printf '%s' " + shellQuote(cfg) + " > \"$CODEX_HOME/config.toml\" || exit 1\n" +
		codexKeyEnv + "=$(cat " + shellQuote(tokenFile) + ") || exit 1\n" +
		"export CODEX_HOME " + codexKeyEnv + "\n" +
		"exec " + shellQuote(codexPath) + " \"$@\"\n"
}

// tomlString quotes s as a TOML basic string; JSON string syntax is a subset.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runCodexHostRunner drives a codex session on a loopback host with no
// detach_sessions key: it runs under the runner, so a dropped host link leaves
// the agent, its run directory and its generation as they were, a message sent
// after the link returns reaches it, and a stop ends its process group and
// removes the run directory. It judges no model output: the stack's endpoint
// may not serve the Responses API, and the detail says whether the turn
// answered.
func runCodexHostRunner(ctx context.Context, e env) result {
	const id = codexHostID
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, "fixture host or project: "+err.Error())
	}
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	// Deliberate: a signal cancels the journey's context so the sweep and the
	// teardown run, then is delivered again so the process ends as the sender
	// meant; same as session-detached-host.
	sigs := make(chan os.Signal, 2)
	watched := []os.Signal{syscall.SIGTERM}
	if !signal.Ignored(os.Interrupt) {
		watched = append(watched, os.Interrupt)
	}
	signal.Notify(sigs, watched...)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var caught os.Signal // written only before done closes
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case caught = <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	var res result
	fx, err := setUpCodexHostFixture(ctx, e, baseURL, model, tokenFile)
	if err != nil {
		res = blocked(id, "fixture host or project: "+err.Error())
	} else {
		res = driveCodexHost(ctx, e, fx)
		res.Detail += fx.tearDown()
	}

	signal.Stop(sigs)
	cancel()
	<-done
	if caught == nil {
		select {
		case caught = <-sigs:
		default:
		}
	}
	if caught != nil {
		res.Detail += "; " + caught.String() + " received during the journey; cleanup ran"
		if sig, ok := caught.(syscall.Signal); ok {
			_ = syscall.Kill(os.Getpid(), sig)
		}
	}
	return res
}

type codexHostFixture struct {
	*detachedFixture
	codexHome string
}

func (fx *codexHostFixture) tearDown() string {
	problem := fx.detachedFixture.tearDown()
	if err := os.RemoveAll(fx.codexHome); err != nil {
		problem += "; teardown: cannot remove the scratch codex home"
	}
	return problem
}

func setUpCodexHostFixture(ctx context.Context, e env, baseURL, model, tokenFile string) (*codexHostFixture, error) {
	codex, err := cmdlookup.Local(ctx, "codex")
	if err != nil {
		return nil, fmt.Errorf("no codex on this machine's login-shell PATH to launch: %w", err)
	}
	base := filepath.Join(gateStateDir(), "grant-codexhost-"+e.Nonce)
	fx := &codexHostFixture{detachedFixture: &detachedFixture{
		configDir: e.ConfigDir, hostID: uuid.NewString(), projectID: uuid.NewString(), entry: "verify-" + e.Nonce + "-codexhost",
		projectDir: base, launcher: base + ".sh",
	}, codexHome: base + ".codexhome"}
	if err := os.MkdirAll(fx.projectDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the project folder: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(fx.projectDir); err == nil {
		fx.projectDir = resolved
	}
	if err := os.WriteFile(fx.launcher, []byte(codexHostLauncher(codex, baseURL, model, tokenFile, fx.codexHome)), 0o700); err != nil {
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
	now := time.Now().UTC().Format(time.RFC3339)
	host := map[string]any{
		"id": fx.hostID, "name": dropInHostPrefix + e.Nonce + "-codexhost", "target": dropInHostTarget,
		"created_at": now,
		"terminal_templates": []map[string]string{
			{"id": "shell", "name": "Shell"},
			{"id": "codex", "name": "Codex", "command": fx.launcher},
		},
	}
	// Deliberate: the entry's model is a codex model relay offers; codex
	// reaches the stack's endpoint through the scratch home's one provider.
	project := map[string]any{
		"id": fx.projectID, "name": dropInProjectName + e.Nonce + " codexhost", "path": fx.projectDir, "host_id": fx.hostID,
		"allowed_mcp_ids": []string{}, "allowed_models": []string{"*"}, "allowed_templates": []string{"codex"},
		"token": projectToken, "token_hash": hex.EncodeToString(tokenHash[:]), "created_at": now,
		"agents": []map[string]string{{"name": fx.entry, "kind": "codex", "model": codexSlug, "command": fx.launcher}},
	}
	if err := editSettings(e.ConfigDir, func(top map[string]json.RawMessage) error {
		return appendRecords(top, map[string]any{"hosts": host, "projects": project})
	}); err != nil {
		fx.tearDown()
		return nil, err
	}
	return fx, nil
}

// readRunEvents returns the parsed lines of a generation's events.jsonl.
func readRunEvents(root, sid, gen string) []struct{ Type, Generation string } {
	raw, err := os.ReadFile(filepath.Join(root, sid, gen, "events.jsonl"))
	if err != nil {
		return nil
	}
	var out []struct{ Type, Generation string }
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var ev struct {
			Type       string `json:"type"`
			Generation string `json:"generation"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type != "" {
			out = append(out, struct{ Type, Generation string }{ev.Type, ev.Generation})
		}
	}
	return out
}

func countEvents(evs []struct{ Type, Generation string }, typ, gen string) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == typ && ev.Generation == gen {
			n++
		}
	}
	return n
}

func driveCodexHost(ctx context.Context, e env, fx *codexHostFixture) (res result) {
	const id = codexHostID
	agentsRoot, rootErr := hostAgentsRoot()
	sweep := &detachedSweep{agentsRoot: agentsRoot}
	defer func() { res = sweep.finish(ctx, e, res) }()
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	if rootErr != nil {
		return blocked(id, rootErr.Error())
	}

	// Waits: none possible for the settings import; relay's file watch emits
	// no event. A start that left no run directory is stopped and started
	// again, bounded, until runner.json exists.
	var err error
	var sid string
	var first detachedRunFile
	started := pollUntil(ctx, detachedStartTries, func() bool {
		var s string
		s, err = startHostSession(ctx, e, fx.projectID, fx.entry)
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
		return fail("a codex session on a host with no detach_sessions key left no runner.json under the agents directory")
	}
	if first.PID <= 1 || first.PGID <= 1 || first.Started == "" || first.Generation == "" || !pidAlive(first.PID) {
		return fail("runner.json after start reads %+v, or its pid is not alive", first)
	}

	dis := cliWithTimeout(ctx, e, detachedCLIWait, "host", "disconnect", "--id", fx.hostID, "--json")
	if dis.Exit != 0 {
		return fail("relay host disconnect exited %d: %s", dis.Exit, firstLine(dis.Stderr))
	}
	// Waits: none possible; a stack run reads session.host_link from the
	// relay-sessions log, which has no wake-up. Polled, bounded.
	if !pollUntil(ctx, detachedLinkWait, func() bool { return hostLinkSeen(e, sid, "lost") }) {
		return fail("no session.host_link link=lost event within %s of relay host disconnect", detachedLinkWait)
	}
	if !pollUntil(ctx, detachedLinkWait, func() bool { return hostLinkSeen(e, sid, "restored") }) {
		return fail("no session.host_link link=restored event within %s", detachedLinkWait)
	}

	before := countEvents(readRunEvents(agentsRoot, sid, first.Generation), "turn_started", first.Generation)
	send := cliWithTimeout(ctx, e, detachedCLIWait, "session", "send", "--id", sid, "--json", "--text", "Reply with exactly: verify-"+e.Nonce+"-codexhost")
	if send.Exit != 0 {
		return fail("relay session send after the link returned exited %d: %s", send.Exit, firstLine(send.Stderr))
	}
	// session send returns once relay holds the runner's receipt, and the
	// runner writes turn_started before the receipt, so one read is enough.
	if got := countEvents(readRunEvents(agentsRoot, sid, first.Generation), "turn_started", first.Generation); got <= before {
		return fail("events.jsonl holds %d turn_started lines in generation %s after the send, had %d before", got, first.Generation, before)
	}

	after, err := readRunFile(agentsRoot, sid, first.Generation)
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

	// Not judged: whether the endpoint answered the turn. Bounded look.
	answered := pollUntil(ctx, codexHostAnswerWait, func() bool {
		return countEvents(readRunEvents(agentsRoot, sid, first.Generation), "turn_ended", first.Generation) > 0
	})

	stop := cliWithTimeout(ctx, e, detachedCLIWait, "session", "stop", "--id", sid, "--json")
	if stop.Exit != 0 {
		return fail("relay session stop exited %d: %s", stop.Exit, firstLine(stop.Stderr))
	}
	if err := syscall.Kill(-first.PGID, 0); !errors.Is(err, syscall.ESRCH) {
		return fail("process group %d is still alive after relay session stop (kill(2): %v)", first.PGID, err)
	}
	if _, err := os.Stat(filepath.Join(agentsRoot, sid)); !errors.Is(err, os.ErrNotExist) {
		return fail("the run directory for the stopped session still exists")
	}
	turn := "the endpoint did not answer the turn within " + codexHostAnswerWait.String() + " (not judged)"
	if answered {
		turn = "the endpoint answered the turn (not judged)"
	}
	return result{id, statePass, fmt.Sprintf("codex agent pid %d lived through relay host disconnect (link lost, then restored); runner.json kept its identity in one generation, a message sent after the link returned wrote turn_started, and stop ended group %d and removed the run directory; %s", first.PID, first.PGID, turn)}
}
