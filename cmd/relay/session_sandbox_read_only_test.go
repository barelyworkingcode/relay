package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// blockTerms returns the terms of every profile block whose head line is head.
func blockTerms(body, head string) []string {
	var out []string
	for rest := body; ; {
		at := strings.Index(rest, head+"\n")
		if at < 0 {
			return out
		}
		terms, after := profileBlockTerms(rest[at+len(head)+1:])
		out = append(out, terms...)
		rest = after
	}
}

// withoutBlocks drops every block whose head is one of heads.
func withoutBlocks(body string, heads ...string) string {
	for _, h := range heads {
		for strings.Contains(body, h+"\n") {
			body = dropBlock(body, h+"\n")
		}
	}
	return body
}

const (
	readHead      = "(allow file-read*"
	readWriteHead = "(allow file-read* file-write*"
)

func subpathTerm(p string) string { return `(subpath "` + p + `")` }

func readOnlyLaunchRequest(proj config.Project, settings string) LaunchRequest {
	req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude}
	if settings != "" {
		req.ClientSettings = json.RawMessage(settings)
	}
	return req
}

func TestReadOnlyProfile_ThreeRootsReadableNoProjectWritable(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	own := addLaunchTestProject(t, store, nil)
	p2 := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
	p3 := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p3"; p.Name = "Gamma" })
	t.Setenv("HOME", t.TempDir())

	_, base := launchWithSandbox(t, readOnlyLaunchRequest(own, ""), store)
	_, ro := launchWithSandbox(t, readOnlyLaunchRequest(own, `{"readOnlyProjects":true}`), store)

	roots := []string{sandboxRealPath(t, own.Path), sandboxRealPath(t, p2.Path), sandboxRealPath(t, p3.Path)}
	readRO, readBase := blockTerms(ro, readHead), blockTerms(base, readHead)
	for _, r := range roots {
		if !slices.Contains(readRO, subpathTerm(r)) {
			t.Errorf("read grants %v lack root %s", readRO, r)
		}
	}
	if got, want := len(readRO), len(readBase)+len(roots); got != want {
		t.Errorf("read grants = %d terms, want the default's %d plus %d roots\n%v", got, len(readBase), len(roots), readRO)
	}

	rwRO := blockTerms(ro, readWriteHead)
	for _, r := range roots {
		if slices.Contains(rwRO, subpathTerm(r)) {
			t.Errorf("project root %s is writable: %v", r, rwRO)
		}
	}
	wantRW := slices.DeleteFunc(slices.Clone(blockTerms(base, readWriteHead)), func(s string) bool { return s == subpathTerm(roots[0]) })
	if !slices.Equal(rwRO, wantRW) {
		t.Errorf("read-write grants = %v, want the default's minus the project: %v", rwRO, wantRW)
	}
	if len(wantRW) == len(blockTerms(base, readWriteHead)) {
		t.Fatal("premise broken: the default profile did not grant the project for writing")
	}

	// Everything but the read and read-write grants (deny, sockets, TCP) is as it was.
	if a, b := withoutBlocks(ro, readHead, readWriteHead), withoutBlocks(base, readHead, readWriteHead); a != b {
		t.Errorf("rules outside the file grants changed.\nread-only:\n%s\ndefault:\n%s", a, b)
	}
}

func TestReadOnlyProfile_SymlinkedRootIsResolved(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	own := addLaunchTestProject(t, store, nil)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta"; p.Path = link })
	t.Setenv("HOME", t.TempDir())

	_, body := launchWithSandbox(t, readOnlyLaunchRequest(own, `{"readOnlyProjects":true}`), store)

	reads := blockTerms(body, readHead)
	if !slices.Contains(reads, subpathTerm(sandboxRealPath(t, real))) {
		t.Errorf("read grants lack the link's target %s: %v", real, reads)
	}
	if slices.Contains(reads, subpathTerm(link)) {
		t.Errorf("read grants name the link %s itself: %v", link, reads)
	}
}

func TestReadOnlyProfile_DefaultCreateHoldsNoOtherProjectRoot(t *testing.T) {
	noDeveloperTools(t)
	store := newLaunchTestStore(t)
	own := addLaunchTestProject(t, store, nil)
	other := addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
	t.Setenv("HOME", t.TempDir())

	for _, settings := range []string{"", `{}`, `{"readOnlyProjects":false}`} {
		_, body := launchWithSandbox(t, readOnlyLaunchRequest(own, settings), store)
		if strings.Contains(body, sandboxRealPath(t, other.Path)) {
			t.Errorf("settings %q: profile names another project's root", settings)
		}
		if !slices.Contains(blockTerms(body, readWriteHead), subpathTerm(sandboxRealPath(t, own.Path))) {
			t.Errorf("settings %q: the session's own project is not writable", settings)
		}
	}
}

func TestReadOnlyProjects_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		model    string
		settings string
		hosted   bool
		status   int
		code     string
	}{
		{"string value", "haiku", `{"readOnlyProjects":"true"}`, false, 400, "invalid_settings"},
		{"number value", "haiku", `{"readOnlyProjects":1}`, false, 400, "invalid_settings"},
		{"null value", "haiku", `{"readOnlyProjects":null}`, false, 400, "invalid_settings"},
		{"chat kind", "gpt-5", `{"readOnlyProjects":true}`, false, 400, "read_only_needs_claude"},
		{"hosted project", "haiku", `{"readOnlyProjects":true}`, true, 403, "read_only_local_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fs := newBlankModelFixture(t)
			projectID := f.proj.ID
			if tc.hosted {
				projectID = "hosted"
				assertNoErr(t, f.store.With(func(s *config.Settings) {
					s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "Testbox", TerminalTemplates: []config.TerminalTemplate{{ID: "claude-code", Name: "Claude Code"}}})
					s.Projects = append(s.Projects, config.Project{
						ID: projectID, Name: "Hosted", Path: "/work/acme", HostID: "h1",
						AllowedModels: []string{"*"}, AllowedMcpIDs: []string{"*"}, AllowedTemplates: []string{"*"},
					})
				}), "seed hosted project")
			}
			body := `{"projectId":"` + projectID + `","name":"","model":"` + tc.model + `","settings":` + tc.settings + `}`

			rec := f.post(t, "/api/sessions", body)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if n := len(fs.Requests()); n != 0 {
				t.Fatalf("host received %d request(s) for a refused launch", n)
			}
			_, refusal := AuthorizeLaunch(f.store, f.deps.modelKeys, f.deps.sessions, LaunchRequest{
				Caller: bearerCaller(control.ClassExecute), ProjectID: projectID, Kind: deriveSessionKind(tc.model),
				Model: tc.model, ClientSettings: json.RawMessage(tc.settings),
			})
			if refusal == nil || refusal.Code != tc.code || refusal.Status != tc.status {
				t.Fatalf("refusal = %+v, want %d %s", refusal, tc.status, tc.code)
			}
		})
	}
}

func TestReadOnlyProjects_LaunchAuditCarriesTheOptionAndRootCount(t *testing.T) {
	noDeveloperTools(t)
	f, _ := newBlankModelFixture(t)
	addLaunchTestProject(t, f.store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
	addLaunchTestProject(t, f.store, func(p *config.Project) { p.ID = "p3"; p.Name = "Gamma" })

	rec := f.post(t, "/api/sessions", `{"projectId":"`+f.proj.ID+`","name":"","model":"haiku","settings":{"readOnlyProjects":true}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}

	var launches []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, f.deps.auditor) {
		if ev.Event == audit.AuditEventSessionLaunch && ev.Outcome == audit.AuditOutcomeOK {
			launches = append(launches, ev)
		}
	}
	if len(launches) != 1 {
		t.Fatalf("ok session_launch events = %+v, want 1", launches)
	}
	var args map[string]any
	assertNoErr(t, json.Unmarshal(launches[0].Args, &args), "decode audit args")
	if args["read_only_projects"] != true || args["read_roots"] != float64(3) {
		t.Fatalf("audit args = %v, want read_only_projects true and read_roots 3", args)
	}
}
