package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/sandbox"
)

func TestClassifyReadOnlyProfile(t *testing.T) {
	eperm := profileProbe{Started: true, Eperm: true, Out: "Operation not permitted"}
	base := func() readOnlyRun {
		return readOnlyRun{
			Create: frontendResponse{Status: http.StatusCreated}, SessionID: "s1", JoinSeen: true, InitSeen: true,
			Tools: []string{"Glob", "Grep", "Read"}, Servers: []string{"relay"},
			ReadOther: profileProbe{Started: true, OK: true}, WriteOwn: eperm, ReadOutside: eperm,
			AddProject: frontendResponse{Status: http.StatusCreated}, ExitSeen: true,
		}
	}
	checkMuts(t, base, classifyReadOnlyProfile, []mutCase[readOnlyRun]{
		{"all four observations hold", func(*readOnlyRun) {}, statePass},
		{"setup failed", func(r *readOnlyRun) { r.Setup = "fixture project: status 500" }, stateFail},
		{"launch refused", func(r *readOnlyRun) { r.Create.Status, r.SessionID = http.StatusBadRequest, "" }, stateFail},
		{"no init", func(r *readOnlyRun) { r.InitSeen = false }, stateFail},
		{"a write tool is offered", func(r *readOnlyRun) { r.Tools = []string{"Edit", "Glob", "Grep", "Read"} }, stateFail},
		{"a tool is missing", func(r *readOnlyRun) { r.Tools = []string{"Grep", "Read"} }, stateFail},
		{"another MCP server loaded", func(r *readOnlyRun) { r.Servers = []string{"relay", "devtools"} }, stateFail},
		{"no MCP server", func(r *readOnlyRun) { r.Servers = nil }, stateFail},
		{"profile file missing", func(r *readOnlyRun) { r.ProfileErr = "no such file" }, stateFail},
		{"other root unreadable", func(r *readOnlyRun) { r.ReadOther = eperm }, stateFail},
		{"own root writable", func(r *readOnlyRun) { r.WriteOwn = profileProbe{Started: true, OK: true} }, stateFail},
		{"write refused for another reason", func(r *readOnlyRun) {
			r.WriteOwn = profileProbe{Started: true, Out: "No such file or directory"}
		}, stateFail},
		{"outside readable", func(r *readOnlyRun) { r.ReadOutside = profileProbe{Started: true, OK: true} }, stateFail},
		{"probe never ran", func(r *readOnlyRun) { r.ReadOutside = profileProbe{Out: "exec: not found"} }, stateFail},
		{"project add refused", func(r *readOnlyRun) { r.AddProject.Status = http.StatusForbidden }, stateFail},
		{"session survives the add", func(r *readOnlyRun) { r.ExitSeen = false }, stateFail},
	}, map[string]string{
		"a write tool is offered": "Edit", "another MCP server loaded": "devtools", "own root writable": "own project root",
		"write refused for another reason": "not with EPERM", "session survives the add": "process_exited",
	})
}

func TestParseReadOnlyFrame_InitToolsAndServers(t *testing.T) {
	raw := []byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"system","subtype":"init",` +
		`"tools":["Read","mcp__relay__ping","Glob","Grep"],"mcp_servers":[{"name":"relay","status":"connected"}]}}`)
	f := parseReadOnlyFrame(raw)
	if f.SessionID != "s1" || f.Subtype != "init" || !slices.Equal(f.Servers, []string{"relay"}) {
		t.Fatalf("frame = %+v", f)
	}
	if got := builtinTools(f.Tools); !slices.Equal(got, []string{"Glob", "Grep", "Read"}) {
		t.Fatalf("built-in tools = %v, want the three without the MCP tool", got)
	}
	if other := parseReadOnlyFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"assistant"}}`)); other.Subtype != "" {
		t.Fatalf("a non-system event parsed as subtype %q", other.Subtype)
	}
}

// The probes only mean something if a refusal by the profile reads differently
// from a command that failed or never ran. The profile grants no temp directory,
// so the fixtures under the stand-in home are refused for want of a grant.
func TestRunProfileProbe_TellsAProfileRefusalFromOtherFailures(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	old := home
	home = resolvedTempDir(t)
	t.Cleanup(func() { home = old })
	base := home
	granted, ungranted := filepath.Join(base, "granted"), filepath.Join(base, "ungranted")
	for _, d := range []string{granted, ungranted} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	readable, secret := filepath.Join(granted, "a.txt"), filepath.Join(ungranted, "b.txt")
	for _, f := range []string{readable, secret} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := sandbox.Write(filepath.Join(t.TempDir(), "profiles"), "probe-test", sandbox.Spec{
		Read: []string{granted}, ReadWrite: []string{"/dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if p := runProfileProbe(ctx, profile, "/bin/cat", readable); !p.Started || !p.OK {
		t.Errorf("a granted read = %+v", p)
	}
	if p := runProfileProbe(ctx, profile, "/bin/cat", secret); !p.Started || p.OK || !p.Eperm {
		t.Errorf("an ungranted read = %+v, want a refusal with EPERM", p)
	}
	if p := runProfileProbe(ctx, profile, "/bin/sh", "-c", `echo x > "$1"`, "sh", filepath.Join(granted, "new.txt")); !p.Started || p.OK || !p.Eperm {
		t.Errorf("a write under a read grant = %+v, want a refusal with EPERM", p)
	}
	if p := runProfileProbe(ctx, profile, "/bin/cat", filepath.Join(granted, "missing.txt")); !p.Started || p.OK || p.Eperm {
		t.Errorf("a missing file in a granted root = %+v, want a failure that is not EPERM", p)
	}
	if p := runProfileProbe(ctx, profile, filepath.Join(base, "no-such-binary")); p.Started {
		t.Errorf("a command that never ran = %+v, want Started false", p)
	}
}
