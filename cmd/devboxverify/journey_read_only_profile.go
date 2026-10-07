package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	readOnlyProfileID = "cos-read-only-profile"
	readOnlyModel     = "haiku"
	sandboxExecBin    = "/usr/bin/sandbox-exec"
	epermText         = "Operation not permitted"

	readOnlyInitWait = 120 * time.Second
	readOnlyExitWait = 30 * time.Second
	readOnlyJoinWait = 15 * time.Second
)

// readOnlyFrame is the part of a /ws frame this journey judges.
type readOnlyFrame struct {
	Type, SessionID, Subtype string
	Tools, Servers           []string
}

func parseReadOnlyFrame(raw []byte) readOnlyFrame {
	var f struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Event     struct {
			Type       string   `json:"type"`
			Subtype    string   `json:"subtype"`
			Tools      []string `json:"tools"`
			MCPServers []string `json:"mcp_servers"`
		} `json:"event"`
	}
	_ = json.Unmarshal(raw, &f)
	out := readOnlyFrame{Type: f.Type, SessionID: f.SessionID}
	if f.Type == "llm_event" && f.Event.Type == "system" {
		out.Subtype = f.Event.Subtype
		out.Tools = f.Event.Tools
		out.Servers = f.Event.MCPServers
	}
	return out
}

// builtinTools drops the tools an MCP server contributes (mcp__<server>__*).
func builtinTools(tools []string) []string {
	var out []string
	for _, t := range tools {
		if !strings.HasPrefix(t, "mcp__") {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// profileProbe is one command run under the session's profile. Started false
// means the command never ran, which says nothing about the profile.
type profileProbe struct {
	Started, OK, Eperm bool
	Out                string
}

func runProfileProbe(ctx context.Context, profile string, argv ...string) profileProbe {
	out, err := exec.CommandContext(ctx, sandboxExecBin, append([]string{"-f", profile, "--"}, argv...)...).CombinedOutput()
	p := profileProbe{Out: strings.TrimSpace(string(out))}
	var exit *exec.ExitError
	switch {
	case err == nil:
		p.Started, p.OK = true, true
	case errors.As(err, &exit) && strings.HasPrefix(p.Out, "sandbox-exec:"):
		// sandbox-exec itself failed (a profile it cannot load, a command it
		// cannot exec); the profile refused nothing.
	case errors.As(err, &exit):
		p.Started, p.Eperm = true, strings.Contains(p.Out, epermText)
	}
	return p
}

type readOnlyRun struct {
	Setup                            string // fixture or project setup failed
	DialErr                          string
	Create                           frontendResponse
	SessionID                        string
	JoinSeen                         bool
	SendErr                          string
	InitSeen                         bool
	Tools, Servers                   []string
	ProfileErr                       string
	ReadOther, WriteOwn, ReadOutside profileProbe
	AddProject                       frontendResponse
	ExitSeen                         bool
	Teardown                         string
}

func runReadOnlyProfile(ctx context.Context, e env) result {
	r := driveReadOnlyProfile(ctx, e)
	res := classifyReadOnlyProfile(r)
	res.Detail += r.Teardown
	return res
}

func driveReadOnlyProfile(ctx context.Context, e env) (r readOnlyRun) {
	launch, run, res, ok := screenCreds(e, readOnlyProfileID)
	if !ok {
		r.Setup = res.Detail
		return r
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		r.Setup = err.Error()
		return r
	}
	// The fixtures live under $HOME, not temp: temp is readable and writable
	// to every session, so a probe there would pass for the wrong reason.
	base, err := os.MkdirTemp(home, "devboxverify-ro-")
	if err != nil {
		r.Setup = "cannot create the fixture folder: " + err.Error()
		return r
	}
	defer func() { _ = os.RemoveAll(base) }()
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	otherDir, addedDir, outsideDir := filepath.Join(base, "other"), filepath.Join(base, "added"), filepath.Join(base, "outside")
	for _, d := range []string{otherDir, addedDir, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.Setup = "cannot create the fixture folder: " + err.Error()
			return r
		}
	}
	otherFile, outsideFile := filepath.Join(otherDir, "readable.txt"), filepath.Join(outsideDir, "unreadable.txt")
	for _, f := range []string{otherFile, outsideFile} {
		if err := os.WriteFile(f, []byte("verify fixture\n"), 0o600); err != nil {
			r.Setup = "cannot write the fixture file: " + err.Error()
			return r
		}
	}

	var projectIDs []string
	defer func() {
		var out strings.Builder
		for _, id := range projectIDs {
			if st := frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/projects/"+id, nil).Status; st != http.StatusNoContent && st != http.StatusNotFound {
				fmt.Fprintf(&out, "; teardown: DELETE project status %d", st)
			}
		}
		r.Teardown += out.String()
	}()
	addProject := func(name, dir string) (frontendResponse, string) {
		resp, d := gatedFrontend(ctx, e, run, http.MethodPost, "/api/projects", jsonBody(map[string]any{"name": name, "path": dir}), fmt.Sprintf("%q", name))
		id, errText := createdID("POST /api/projects", resp)
		if id != "" {
			projectIDs = append(projectIDs, id)
		} else {
			resp.Error = fmt.Sprintf("%s (prompt: %s %s)", errText, d.Outcome, d.Detail)
		}
		return resp, id
	}
	if resp, id := addProject("Verify RO Other "+e.Nonce, otherDir); id == "" {
		r.Setup = "fixture project: " + resp.Error
		return r
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: frontendRequestTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}
	conn, resp, err := dialer.DialContext(ctx, "ws://relay/ws", http.Header{"Authorization": {"Bearer " + run}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		r.DialErr = err.Error()
		return r
	}
	defer func() { _ = conn.Close() }()
	frames, stop := make(chan readOnlyFrame, 256), make(chan struct{})
	defer close(stop)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case frames <- parseReadOnlyFrame(raw):
			case <-stop:
				return
			}
		}
	}()

	body := jsonBody(map[string]any{"projectId": acmeID, "name": "verify-" + e.Nonce + "-readonly", "model": readOnlyModel,
		"settings": map[string]bool{"readOnlyProjects": true, "useRelayTools": true}})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return r
	}
	id := r.SessionID
	defer func() {
		st := frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+id, nil).Status
		if st/100 != 2 {
			r.Teardown += fmt.Sprintf("; teardown: DELETE session status %d", st)
		}
	}()

	send := func(m map[string]string) error { return conn.WriteMessage(websocket.TextMessage, jsonBody(m)) }
	if err := send(map[string]string{"type": "join_session", "sessionId": id}); err != nil {
		r.SendErr = "join_session frame: " + err.Error()
		return r
	}
	if _, r.JoinSeen = awaitReadOnlyFrame(ctx, frames, readOnlyJoinWait, func(f readOnlyFrame) bool { return f.Type == "session_joined" && f.SessionID == id }); !r.JoinSeen {
		return r
	}
	// Claude prints system/init once it has input, so one short message is
	// what brings it out; the journey judges the init, not the reply.
	if err := send(map[string]string{"type": "send_message", "sessionId": id, "text": "Reply with exactly: ok"}); err != nil {
		r.SendErr = "send_message frame: " + err.Error()
		return r
	}
	init, seen := awaitReadOnlyFrame(ctx, frames, readOnlyInitWait, func(f readOnlyFrame) bool {
		return f.SessionID == id && f.Subtype == "init"
	})
	if r.InitSeen = seen; !seen {
		return r
	}
	r.Tools, r.Servers = builtinTools(init.Tools), init.Servers

	profile := filepath.Join(e.ConfigDir, "sessions", "profiles", id+".sb")
	if _, err := os.Stat(profile); err != nil {
		r.ProfileErr = err.Error()
		return r
	}
	// A write that wrongly succeeds must not leave a file in the world.
	ownWrite := filepath.Join(acme.Folder, "devboxverify-write-probe-"+e.Nonce)
	defer func() { _ = os.Remove(ownWrite) }()
	r.ReadOther = runProfileProbe(ctx, profile, "/bin/cat", otherFile)
	r.WriteOwn = runProfileProbe(ctx, profile, "/bin/sh", "-c", `echo x > "$1"`, "sh", ownWrite)
	r.ReadOutside = runProfileProbe(ctx, profile, "/bin/cat", outsideFile)

	if r.AddProject, _ = addProject("Verify RO Added "+e.Nonce, addedDir); len(projectIDs) < 2 {
		return r
	}
	_, r.ExitSeen = awaitReadOnlyFrame(ctx, frames, readOnlyExitWait, func(f readOnlyFrame) bool { return f.Type == "process_exited" && f.SessionID == id })
	return r
}

// awaitReadOnlyFrame reads frames until one satisfies pred. The wait ends on
// the frame, never on a timer; d only bounds a frame that never comes.
func awaitReadOnlyFrame(ctx context.Context, frames <-chan readOnlyFrame, d time.Duration, pred func(readOnlyFrame) bool) (readOnlyFrame, bool) {
	timeout := time.NewTimer(d)
	defer timeout.Stop()
	for {
		select {
		case f := <-frames:
			if pred(f) {
				return f, true
			}
		case <-timeout.C:
			return readOnlyFrame{}, false
		case <-ctx.Done():
			return readOnlyFrame{}, false
		}
	}
}

func classifyReadOnlyProfile(r readOnlyRun) result {
	const id = readOnlyProfileID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Setup != "":
		return fail("setup: " + r.Setup)
	case r.DialErr != "":
		return fail("GET /ws: " + r.DialErr)
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return fail(fmt.Sprintf("POST /api/sessions with readOnlyProjects status %d: %s", r.Create.Status, r.Create.Error))
	case r.SendErr != "":
		return fail(r.SendErr)
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case !r.InitSeen:
		return fail("no system/init event on /ws")
	case !slices.Equal(r.Tools, []string{"Glob", "Grep", "Read"}):
		return fail(fmt.Sprintf("init built-in tools %v, want exactly [Glob Grep Read]", r.Tools))
	case !slices.Equal(r.Servers, []string{"relay"}):
		return fail(fmt.Sprintf("init MCP servers %v, want exactly [relay]", r.Servers))
	case r.ProfileErr != "":
		return fail("the session's sandbox profile is missing: " + r.ProfileErr)
	}
	for _, p := range []struct {
		what  string
		probe profileProbe
		want  string // "ok" or "eperm"
	}{
		{"reading a file in another project root", r.ReadOther, "ok"},
		{"writing in the session's own project root", r.WriteOwn, "eperm"},
		{"reading outside every root", r.ReadOutside, "eperm"},
	} {
		switch {
		case !p.probe.Started:
			return fail(p.what + ": the probe never ran: " + p.probe.Out)
		case p.want == "ok" && !p.probe.OK:
			return fail(p.what + " was refused: " + p.probe.Out)
		case p.want == "eperm" && p.probe.OK:
			return fail(p.what + " succeeded; the profile does not enforce it")
		case p.want == "eperm" && !p.probe.Eperm:
			return fail(p.what + " failed, but not with EPERM: " + p.probe.Out)
		}
	}
	switch {
	case r.AddProject.Status != http.StatusCreated:
		return fail(fmt.Sprintf("adding a project: %s", r.AddProject.Error))
	case !r.ExitSeen:
		return fail("no process_exited frame for the session after a project was added")
	}
	return result{id, statePass, "read-only claude session: built-ins Glob, Grep, Read and only the relay MCP server; under its profile another root read, its own root refused a write with EPERM, a folder outside every root refused a read with EPERM; adding a project ended it"}
}
