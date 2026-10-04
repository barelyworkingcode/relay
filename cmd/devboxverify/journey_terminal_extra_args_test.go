package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestClassifyTerminalExtraArgs(t *testing.T) {
	printed := []byte("extra-arg:" + extraArgMarker + "\r\n")
	base := func() extraArgsRun {
		return extraArgsRun{PreDelete: frontendResponse{Status: http.StatusNotFound}, Template: frontendResponse{Status: http.StatusCreated},
			Create: frontendResponse{Status: http.StatusCreated}, TermID: "t1", Project: "Verify Grant",
			List: frontendResponse{Status: http.StatusOK}, Stopped: true, Log: frontendResponse{Status: http.StatusOK, Body: printed}}
	}
	checkMuts(t, base, classifyTerminalExtraArgs, []mutCase[extraArgsRun]{
		{"marker printed, exit 0", func(*extraArgsRun) {}, statePass},
		{"leftover template deleted", func(r *extraArgsRun) { r.PreDelete.Status = http.StatusNoContent }, statePass},
		{"run credential refused", func(r *extraArgsRun) { r.List.Status = http.StatusUnauthorized }, stateBlocked},
		{"leftover delete refused", func(r *extraArgsRun) { r.PreDelete.Status = http.StatusInternalServerError }, stateFail},
		{"template not created", func(r *extraArgsRun) { r.Template.Status = http.StatusConflict }, stateFail},
		{"launch forbidden", func(r *extraArgsRun) { r.Create, r.TermID = frontendResponse{Status: http.StatusForbidden}, "" }, stateBlocked},
		{"launch failed", func(r *extraArgsRun) {
			r.Create, r.TermID = frontendResponse{Status: http.StatusInternalServerError}, ""
		}, stateFail},
		{"list refused", func(r *extraArgsRun) { r.List.Status = http.StatusInternalServerError }, stateFail},
		{"still running after 10 s", func(r *extraArgsRun) { r.Stopped = false }, stateFail},
		{"log unreadable", func(r *extraArgsRun) { r.Log = frontendResponse{Status: http.StatusNotFound} }, stateFail},
		{"log lacks the marker", func(r *extraArgsRun) { r.Log.Body = []byte("extra-arg:\r\n") }, stateFail},
		{"marker without the printed prefix", func(r *extraArgsRun) { r.Log.Body = []byte(extraArgMarker) }, stateFail},
		{"exit code not 0", func(r *extraArgsRun) { r.ExitCode = 3 }, stateFail},
		{"no marker and exit not 0", func(r *extraArgsRun) { r.Log.Body, r.ExitCode = []byte("extra-arg:\r\n"), 3 }, stateFail},
	}, map[string]string{
		"template not created": "POST /api/terminal/templates status 409", "log lacks the marker": "marker",
		"marker without the printed prefix": "marker", "exit code not 0": "exit code 3", "no marker and exit not 0": "marker",
	})
}

// extraArgsFake is the slice of relay's frontend API the journey drives; it
// records the template and launch bodies.
type extraArgsFake struct {
	mu               sync.Mutex
	template, launch []byte
}

func (f *extraArgsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch r.Method + " " + r.URL.Path {
	case "DELETE /api/terminal/templates/" + extraArgsTemplateID:
		w.WriteHeader(http.StatusNotFound)
	case "POST /api/terminal/templates":
		f.template = body
		w.WriteHeader(http.StatusCreated)
	case "POST /api/terminals":
		f.launch = body
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"terminalId":"t1"}`))
	case "GET /api/terminals":
		_, _ = w.Write([]byte(`{"terminals":[{"id":"t1","state":"stopped"}]}`))
	case "GET /api/terminals/t1/log":
		_, _ = w.Write([]byte("extra-arg:" + extraArgMarker + "\r\n"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestRunTerminalExtraArgs(t *testing.T) {
	cases := []struct {
		name  string
		grant string
		want  state
	}{
		{"launches in the Verify Grant project", "g1", statePass},
		{"no Verify Grant project", "", stateBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := blockedEnv(t)
			e.FrontendSocket = filepath.Join(resolvedTempDir(t), "frontend.sock")
			if err := os.WriteFile(e.CredentialFile, []byte("tok-launch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e.Run = &runState{RunCredID: "c1", RunToken: "tok-p1", GrantProjectID: c.grant}
			f := &extraArgsFake{}
			ln, err := net.Listen("unix", e.FrontendSocket)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: f}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			got := runJourney(t, extraArgsID, e)
			checkState(t, got, c.want)
			f.mu.Lock()
			defer f.mu.Unlock()
			if c.grant == "" {
				checkDetail(t, got, grantPosID)
				if f.template != nil || f.launch != nil {
					t.Errorf("sent a template or launch without a Verify Grant project")
				}
				return
			}
			var launch struct {
				ProjectID string   `json:"projectId"`
				ExtraArgs []string `json:"extraArgs"`
			}
			var tmpl struct {
				Args []string `json:"args"`
			}
			if json.Unmarshal(f.launch, &launch) != nil || json.Unmarshal(f.template, &tmpl) != nil {
				t.Fatalf("launch %q or template %q is not JSON", f.launch, f.template)
			}
			if !slices.Equal(launch.ExtraArgs, []string{extraArgMarker}) || launch.ProjectID != "g1" {
				t.Errorf("launch extraArgs %v, projectId %q; want [%s] in g1", launch.ExtraArgs, launch.ProjectID, extraArgMarker)
			}
			if strings.Contains(string(f.template), extraArgMarker) {
				t.Errorf("template %s contains the marker", f.template)
			}
		})
	}
}
