package features

import (
	"encoding/json"
	"strings"
	"testing"

	"relaye2e/harness"
)

const (
	cepHeader = "CLAUDE_CODE_ATTRIBUTION_HEADER"
	cepTokens = "CLAUDE_CODE_MAX_OUTPUT_TOKENS"
	cepOther  = "CLAUDE_CODE_ACME_UNLISTED"
	cepSecret = "acme-fake-token-not-real"
)

// cepStart boots an instance whose relay environment holds env and whose
// settings list the given names (nil leaves the setting absent). The project
// grants claude-code and pi.
func cepStart(t *testing.T, list []string, env []string, mut ...func(*harness.Options)) (*harness.Instance, string) {
	t.Helper()
	o := harness.Options{Credentials: agCreds, Presence: agApprove, Env: env}
	if list != nil {
		raw, err := json.Marshal(list)
		if err != nil {
			t.Fatalf("encoding the list: %v", err)
		}
		o.Settings = map[string]json.RawMessage{"claude_env_passthrough": raw}
	}
	for _, m := range mut {
		m(&o)
	}
	i := harness.Start(t, o)
	i.WaitSessionHost(agHostDeadline)
	return i, agProject(t, i, "acme-cep", nil)
}

func cepRelayEnv() []string {
	return []string{cepHeader + "=0", cepTokens + "=4096", cepOther + "=1"}
}

// cepClaudeStart launches a claude session, runs one turn (the start line is
// written before the first reply) and returns the whole child environment.
func cepClaudeStart(t *testing.T, i *harness.Instance, pid string) []string {
	t.Helper()
	sid := g1sLaunchClaude(t, i, pid)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && c.Env["RELAY_SESSION_ID"] == sid {
			return c.Environ
		}
	}
	t.Fatalf("no claude start line carries RELAY_SESSION_ID %s", sid)
	return nil
}

func cepValue(environ []string, name string) (string, bool) {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// A listed name that relay's environment sets reaches the claude session at
// relay's value; an unlisted name stays dropped.
func TestClaudeEnvPassthroughListedReachesSession(t *testing.T) {
	t.Parallel()
	i, pid := cepStart(t, []string{cepHeader, cepTokens}, cepRelayEnv())
	environ := cepClaudeStart(t, i, pid)
	for name, want := range map[string]string{cepHeader: "0", cepTokens: "4096"} {
		if got, ok := cepValue(environ, name); !ok || got != want {
			t.Fatalf("the claude session holds %s=%q (set %v), want %q", name, got, ok, want)
		}
	}
	if got, ok := cepValue(environ, cepOther); ok {
		t.Fatalf("the unlisted %s reached the claude session as %q", cepOther, got)
	}
}

// With the setting absent, no CLAUDE_CODE_* name from relay's environment
// reaches the session.
func TestClaudeEnvPassthroughEmptyByDefault(t *testing.T) {
	t.Parallel()
	i, pid := cepStart(t, nil, cepRelayEnv())
	environ := cepClaudeStart(t, i, pid)
	for _, name := range []string{cepHeader, cepTokens, cepOther} {
		if got, ok := cepValue(environ, name); ok {
			t.Fatalf("with the setting absent, %s reached the claude session as %q", name, got)
		}
	}
}

// One bad entry refuses the launch with 400 invalid_claude_env. The body names
// the entry, never a value, and no claude starts.
func TestClaudeEnvPassthroughRefusedNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
		"CLAUDE_CODE_SANDBOXED",
		"ANTHROPIC_API_KEY",
		"claude_code_lower",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := []string{name + "=" + cepSecret}
			i, pid := cepStart(t, []string{name}, env)
			resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
				map[string]any{"projectId": pid, "model": "sonnet"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
			if resp.Trace == "" {
				t.Fatalf("the answer carries no trace id")
			}
			if resp.Status != 400 {
				t.Fatalf("POST /api/sessions with %s listed answered %d, want 400: %s", name, resp.Status, resp.Body)
			}
			i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
				Fields: map[string]any{"reason": "invalid_claude_env"}}, agHostDeadline)
			if !strings.Contains(string(resp.Body), name) {
				t.Fatalf("the 400 body does not name the entry %s: %s", name, resp.Body)
			}
			if strings.Contains(string(resp.Body), cepSecret) {
				t.Fatalf("the 400 body holds the value of %s", name)
			}
			for _, c := range i.FakeAgentCalls("claude") {
				if c.Event == "start" && (len(c.Argv) == 0 || c.Argv[0] != "debug") {
					t.Fatalf("a refused launch still started the fake claude: argv %v", c.Argv)
				}
			}
		})
	}
}

// With a name listed and set, a claude-code terminal and a pi session start
// without it.
func TestClaudeEnvPassthroughOtherKindsUntouched(t *testing.T) {
	t.Parallel()
	models := &harness.FakeModelHostSpec{ID: "acme-models", Models: []json.RawMessage{
		json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
		json.RawMessage(`{"id":"local/coder","object":"model","owned_by":"fake","context_length":8192}`),
	}}
	i, pid := cepStart(t, []string{cepHeader, cepTokens}, cepRelayEnv(), func(o *harness.Options) { o.FakeModelHost = models })
	i.WaitModelHost(agHostDeadline)

	term := ctTerminalStart(t, i, pid)
	for _, name := range []string{cepHeader, cepTokens} {
		if got, ok := cepValue(term.Environ, name); ok {
			t.Fatalf("the claude-code terminal holds %s=%q, want it dropped", name, got)
		}
	}

	agAdd(t, i, pid, agEntry{Name: "piLocal", Kind: "pi", Model: "local/coder"})
	res := agSessionStart(i, pid, "piLocal")
	sid := agRunTurn(t, i, res)
	found := false
	for _, c := range i.FakeAgentCalls("pi") {
		if c.Event != "start" || len(c.Argv) == 0 || c.Argv[0] == "--list-models" {
			continue
		}
		found = true
		for _, name := range []string{cepHeader, cepTokens} {
			if got, ok := cepValue(c.Environ, name); ok {
				t.Fatalf("pi session %s holds %s=%q, want it dropped", sid, name, got)
			}
		}
	}
	if !found {
		t.Fatalf("no pi start line for session %s", sid)
	}
}
