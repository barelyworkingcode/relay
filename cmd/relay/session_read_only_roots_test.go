package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

func TestReadOnlyProjectRoots_LocalProjectsOnly(t *testing.T) {
	local1, local2 := t.TempDir(), t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	settings := &config.Settings{Projects: []config.Project{
		{ID: "a", Path: local1},
		{ID: "remote", Kind: config.ProjectKindRemote, Path: t.TempDir()},
		{ID: "hosted", HostID: "h1", Path: t.TempDir()},
		{ID: "empty", Path: ""},
		{ID: "missing", Path: filepath.Join(t.TempDir(), "gone")},
		{ID: "b", Path: local2},
		{ID: "linked", Path: link},
	}}

	got := readOnlyProjectRoots(settings)

	want := []string{sandboxRealPath(t, local1), sandboxRealPath(t, local2), sandboxRealPath(t, real)}
	if !slices.Equal(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
}

// readOnlySweepFixture holds two live claude sessions on one project, one
// launched read-only and one not, behind a fake host that reports each
// /terminate call.
type readOnlySweepFixture struct {
	f          *sessionRoutesFixture
	ops        *ProjectOps
	readOnly   string
	plain      string
	terminated chan string
	host       *FakeService
}

func newReadOnlySweepFixture(t *testing.T) *readOnlySweepFixture {
	t.Helper()
	noDeveloperTools(t)
	x := &readOnlySweepFixture{f: newSessionRoutesFixture(t), terminated: make(chan string, 16)}
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/launch":
				fakeLaunchHandler(&fs, func(id string) string { return `{"sessionId":"` + id + `"}` })(w, r)
			case "/terminate":
				var body struct {
					SessionID string `json:"session_id"`
				}
				_ = json.Unmarshal(fs.LastRequest().Body, &body)
				x.terminated <- body.SessionID
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	})
	x.host = fs
	x.f.registerFakeSessionsHost(t, fs, selfPeerToken(t).Process())
	x.ops = &ProjectOps{Store: x.f.store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t), SessionCleanup: x.f.deps}
	x.readOnly = x.launch(t, `{"readOnlyProjects":true}`)
	x.plain = x.launch(t, `{}`)
	return x
}

func (x *readOnlySweepFixture) launch(t *testing.T, settings string) string {
	t.Helper()
	before := len(x.f.deps.sessions.All())
	rec := x.f.post(t, "/api/sessions", `{"projectId":"`+x.f.proj.ID+`","name":"","model":"haiku","settings":`+settings+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("launch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	all := x.f.deps.sessions.All()
	if len(all) != before+1 {
		t.Fatalf("ledger holds %d sessions after a launch, want %d", len(all), before+1)
	}
	for _, r := range all {
		if !slices.ContainsFunc(x.known(), func(id string) bool { return id == r.SessionID }) {
			return r.SessionID
		}
	}
	t.Fatal("no new session in the ledger")
	return ""
}

func (x *readOnlySweepFixture) known() []string { return []string{x.readOnly, x.plain} }

func (x *readOnlySweepFixture) terminateCalls() int {
	n := 0
	for _, r := range x.host.Requests() {
		if r.Path == "/terminate" {
			n++
		}
	}
	return n
}

// expectTerminated waits for the fake host's /terminate call and checks it
// named the read-only session and nothing else.
func (x *readOnlySweepFixture) expectTerminated(t *testing.T) {
	t.Helper()
	select {
	case id := <-x.terminated:
		if id != x.readOnly {
			t.Fatalf("terminated session %q, want the read-only session %q (plain is %q)", id, x.readOnly, x.plain)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no /terminate call for the read-only session")
	}
	if n := x.terminateCalls(); n != 1 {
		t.Fatalf("%d /terminate calls, want exactly 1", n)
	}
}

func TestReadOnlySweep_ProjectChangesEndOnlyReadOnlySessions(t *testing.T) {
	ctx := context.Background()
	surfaces := func() project.McpSurfaces { return nil }
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, x *readOnlySweepFixture)
	}{
		{"create", func(t *testing.T, x *readOnlySweepFixture) {
			_, err := x.ops.Create(ctx, project.CreateFields{Name: "Beta", Path: t.TempDir()}, nil, auditViaCLI, "")
			assertNoErr(t, err, "Create")
		}},
		{"delete", func(t *testing.T, x *readOnlySweepFixture) {
			other := addLaunchTestProject(t, x.f.store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
			_, found, err := x.ops.Remove(other.ID)
			assertNoErr(t, err, "Remove")
			if !found {
				t.Fatal("Remove: project not found")
			}
		}},
		{"path change", func(t *testing.T, x *readOnlySweepFixture) {
			other := addLaunchTestProject(t, x.f.store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
			moved := t.TempDir()
			_, found, err := x.ops.Update(ctx, other.ID, project.UpdateFields{Path: &moved}, surfaces, auditViaCLI, "")
			assertNoErr(t, err, "Update")
			if !found {
				t.Fatal("Update: project not found")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newReadOnlySweepFixture(t)
			tc.run(t, x)
			x.expectTerminated(t)
		})
	}
}

func TestReadOnlySweep_RenameOnlyUpdateEndsNothing(t *testing.T) {
	x := newReadOnlySweepFixture(t)
	ctx := context.Background()
	other := addLaunchTestProject(t, x.f.store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta" })
	name := "Beta Renamed"
	_, found, err := x.ops.Update(ctx, other.ID, project.UpdateFields{Name: &name}, func() project.McpSurfaces { return nil }, auditViaCLI, "")
	assertNoErr(t, err, "rename")
	if !found {
		t.Fatal("rename: project not found")
	}

	// A path change afterwards is the positive marker: its terminate call is
	// the one a rename must not have preceded.
	moved := t.TempDir()
	_, _, err = x.ops.Update(ctx, other.ID, project.UpdateFields{Path: &moved}, func() project.McpSurfaces { return nil }, auditViaCLI, "")
	assertNoErr(t, err, "path change")
	x.expectTerminated(t)
}
