package features

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

const b296Refusal = "cannot be run from inside a relay session or a sandbox"

// b296Script runs each verb the way a process inside the sandboxed terminal
// would. Each verb writes its stdout and stderr to files and its exit code
// last, so the session's exit follows every write.
const b296Script = `relay=$1; cfg=$2; out=$3; other=$4
run() {
  name=$1; shift
  env -u RELAY_SESSION_ID "$relay" --config-dir "$cfg" "$@" >"$out/$name.out" 2>"$out/$name.err"
  echo $? >"$out/$name.code"
}
run list service list
run grant grant --project "$other" --json
run update project update --id "$other" --files-read-only=false
run refuse enrol refuse --id acme-nosuch
run mcp mcp unregister --id acme-stdio
run restart service restart --id acme-svc
run unregister service unregister --id acme-svc`

func b296Read(t *testing.T, dir, name string) (code int, stdout, stderr string) {
	t.Helper()
	read := func(ext string) string {
		b, err := os.ReadFile(filepath.Join(dir, name+"."+ext))
		if err != nil {
			t.Fatalf("reading %s.%s: %v", name, ext, err)
		}
		return string(b)
	}
	c := strings.TrimSpace(read("code"))
	if _, err := fmt.Sscan(c, &code); err != nil {
		t.Fatalf("%s.code reads %q", name, c)
	}
	return code, read("out"), read("err")
}

func TestBug296SandboxedSessionAdminOpsRefused(t *testing.T) {
	t.Parallel()
	tmpl, err := json.Marshal([]map[string]any{{
		"id": "acme-probe", "name": "Acme probe", "command": "/bin/sh",
		"args": []string{"-c", b296Script, "acme"}, "sandbox": true,
	}})
	if err != nil {
		t.Fatalf("encoding the template: %v", err)
	}
	settings := g5Settings(t, g5Sleeper("acme-svc", true))
	settings["terminal_templates"] = tmpl
	i := harness.Start(t, harness.Options{
		Credentials: []harness.CredentialSpec{
			{Name: "reader", Classes: []string{"read"}},
			{Name: "op", Classes: []string{"read", "configure", "execute"}},
		},
		Presence: map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
		Settings: settings,
		FakeMCPs: acmeStdioMCP(),
	})
	i.WaitSessionHost(g5Up)
	g5WaitRunning(i, "acme-svc")
	g2WaitMCPUp(i, "acme-stdio")

	a := g2Create(t, i, "acme-probe", map[string]any{"allowed_templates": []string{"acme-probe"}})
	b := g2Create(t, i, "acme-granted", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	i.MustCLI("project", "update", "--id", b.ID, "--files-read-only=true")
	if !g2Get(t, i, b.ID).FilesReadOnly {
		t.Fatalf("the operator's project update left files_read_only unset")
	}
	pidBefore := g5Stat(t, i).Runtime["acme-svc"].PID
	if pidBefore == 0 {
		t.Fatalf("status lists no running process for acme-svc")
	}

	dir := filepath.Join(i.Home, "work", "acme-probe")
	start := i.MustCLI("terminal", "start", "--project", a.ID, "--template", "acme-probe",
		"--extra-arg", harness.BundlePaths().Relay, "--extra-arg", i.ConfigDir,
		"--extra-arg", dir, "--extra-arg", b.ID, "--json")
	launch := requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: start.Trace, Fields: map[string]any{"status": "ok"}})
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": launch.Str("session_id")}}, g9Deadline)

	// AC1: the bug line.
	code, _, _ := b296Read(t, dir, "unregister")
	if code != 1 {
		t.Fatalf("service unregister inside a sandboxed session exited %d, want 1", code)
	}
	list, _ := g5List(t, i)
	listed := false
	for _, id := range g5Ids(list) {
		listed = listed || id == "acme-svc"
	}
	if !listed {
		t.Fatalf("acme-svc is gone from the service list after a sandboxed session unregistered it")
	}

	// AC2.
	_, stdout, stderr := b296Read(t, dir, "unregister")
	if stdout != "" || !strings.Contains(stderr, b296Refusal) {
		t.Fatalf("service unregister printed stdout %q, stderr %q, want empty stdout and the session-caller refusal", stdout, stderr)
	}

	// AC3.
	row := map[string]any{"method": "admin_op", "path": "service.unregister", "class": "operator",
		"transport": "bridge", "outcome": "denied", "error": "session_caller"}
	if rows := g2RowsWait(t, i, "control_decision", "denied", row); len(rows) != 1 {
		t.Fatalf("%d denied control_decision rows for service.unregister, want 1", len(rows))
	}

	// AC4.
	for _, verb := range []string{"restart", "mcp", "update", "refuse"} {
		code, _, stderr := b296Read(t, dir, verb)
		if code != 1 || !strings.Contains(stderr, b296Refusal) {
			t.Fatalf("%s inside a sandboxed session exited %d with stderr %q, want 1 and the session-caller refusal", verb, code, stderr)
		}
	}
	if pid := g5Stat(t, i).Runtime["acme-svc"].PID; pid != pidBefore {
		t.Fatalf("acme-svc runs as pid %d after a refused restart, was %d", pid, pidBefore)
	}
	mcps := g3ListedMCPs(t, i)
	if len(mcps) != 1 || mcps[0] != "acme-stdio" {
		t.Fatalf("GET /api/mcps lists %v after a refused unregister, want acme-stdio", mcps)
	}
	if !g2Get(t, i, b.ID).FilesReadOnly {
		t.Fatalf("a refused project update cleared files_read_only on the granted project")
	}
	refuse := map[string]any{"method": "admin_op", "path": "enrolment.request.refuse", "class": "operator",
		"transport": "bridge", "outcome": "denied", "error": "session_caller"}
	if rows := g2RowsWait(t, i, "control_decision", "denied", refuse); len(rows) != 1 {
		t.Fatalf("%d denied control_decision rows for enrolment.request.refuse, want 1", len(rows))
	}

	// AC5.
	code, stdout, stderr = b296Read(t, dir, "grant")
	if code != 1 || !strings.Contains(stderr, b296Refusal) {
		t.Fatalf("grant inside a sandboxed session exited %d with stderr %q, want 1 and the session-caller refusal", code, stderr)
	}
	if strings.Contains(stdout, b.ID) || strings.Contains(stdout, "acme-stdio") {
		t.Fatalf("grant inside a sandboxed session printed another project's grant: %q", stdout)
	}

	// AC6.
	if code, _, _ := b296Read(t, dir, "list"); code != 0 {
		t.Fatalf("service list inside a sandboxed session exited %d, want 0", code)
	}
}
