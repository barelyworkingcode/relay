package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	detachedHostID     = "session-detached-host"
	detachedStartTries = 30 * time.Second
	detachedLinkWait   = 45 * time.Second
	detachedTurnWait   = 120 * time.Second
	detachedCLIWait    = 30 * time.Second
)

// detachedRunFile is run.json, written by the runner on the host once the
// agent leads its own process group.
type detachedRunFile struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	Started    string `json:"started"`
	Generation string `json:"generation"`
}

// runDetachedHost drives a Claude session on a loopback host with
// detach_sessions set: a turn is in flight when the host link is dropped, the
// turn still completes, a later message is answered with no resume, and a
// stop ends the agent's process group and removes its run directory. The
// loopback host is this machine and its login user is this user, so the
// agent's run directory is read as a local path.
func runDetachedHost(ctx context.Context, e env) result {
	const id = detachedHostID
	_, run, res, ok := screenCreds(e, id)
	if !ok {
		return res
	}
	var out result
	setup, teardown := withLoopbackHostProject(ctx, e, run, func(projectID, hostName, _ string) {
		out = driveDetachedHost(ctx, e, run, projectID, hostName)
	})
	if setup != "" {
		return blocked(id, "fixture host or project: "+setup)
	}
	out.Detail += teardown
	return out
}

func driveDetachedHost(ctx context.Context, e env, run, projectID, hostName string) (res result) {
	const id = detachedHostID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	blockedf := func(f string, a ...any) result { return blocked(id, fmt.Sprintf(f, a...)) }

	hostID, err := hostIDByName(ctx, e, run, hostName)
	if err != nil {
		return blockedf("%v", err)
	}
	if err := setHostDetach(e.ConfigDir, hostID); err != nil {
		return blockedf("detach_sessions could not be set: %v", err)
	}
	agentsRoot, err := hostAgentsRoot()
	if err != nil {
		return blockedf("%v", err)
	}

	conn, status, err := dialFrontendWS(ctx, e, run)
	if err != nil {
		return blockedf("GET /ws status %d: %v", status, err)
	}
	defer func() { _ = conn.Close() }()
	log := &dropLog{}
	collectFrames(conn, log)

	// The settings.json edit is imported by relay's file watch, which gives no
	// reply. A detached start returns only once run.json exists, so a start
	// that left no run directory ran before the import: stop it and start
	// again.
	var sid string
	var first detachedRunFile
	started := pollUntil(ctx, detachedStartTries, func() bool {
		var s string
		s, err = startHostSession(ctx, e, projectID)
		if err != nil {
			return false
		}
		if f, rerr := readRunFile(agentsRoot, s, "g1"); rerr == nil {
			sid, first = s, f
			return true
		}
		cliStopSession(ctx, e, s)
		return false
	})
	if sid != "" {
		defer func() {
			cleanup := context.WithoutCancel(ctx)
			cliStopSession(cleanup, e, sid)
			frontendDo(cleanup, e, run, http.MethodDelete, "/api/sessions/"+sid, nil)
			if first.PGID > 1 && groupAlive(first.PGID) { // only our own agent's group, read from its run.json
				_ = syscall.Kill(-first.PGID, syscall.SIGKILL)
			}
		}()
	}
	if !started {
		if err != nil {
			return fail("relay session start: %v", err)
		}
		return fail("a session on a host with detach_sessions set left no run.json under the agents directory")
	}
	if first.PID <= 1 || first.PGID <= 1 || first.Started == "" || !pidAlive(first.PID) {
		return fail("run.json after start reads %+v, or its pid is not alive", first)
	}

	_ = wsSend(conn, map[string]string{"type": "join_session", "sessionId": sid})
	if !log.waitFor(ctx, agentFrameWait, frameSeen(0, func(f dropFrame) bool { return f.Type == "session_joined" && f.SessionID == sid })) {
		return fail("no session_joined frame after join_session")
	}
	mark := len(log.snapshot())

	// The long answer keeps the turn open across the drop; the running frame
	// is the signal that it has started.
	marker1 := "verify-" + e.Nonce + "-turn"
	turn := make(chan cliResult, 1)
	go func() {
		turn <- cliWithTimeout(ctx, e, detachedTurnWait, "session", "message", "--id", sid, "--json", "--text",
			"Count from 1 to 150, one number per line, then on a last line write exactly: "+marker1)
	}()
	if !log.waitFor(ctx, agentFrameWait, stateSeq(sid, mark, "running")) {
		return fail("the session never reported running after relay session message")
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

	select {
	case r := <-turn:
		var reply struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(r.Stdout), &reply)
		if r.Exit != 0 || !bytes.Contains([]byte(reply.Text), []byte(marker1)) {
			return fail("the turn in flight across the drop did not return its answer: exit %d, stderr %q, reply %q", r.Exit, firstLine(r.Stderr), tail(reply.Text, 80))
		}
	case <-ctx.Done():
		return fail("the turn in flight across the drop never returned")
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
		return fail("run.json unreadable after the drop: %v", err)
	}
	if after != first {
		return fail("run.json changed across the drop: before %+v, after %+v", first, after)
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
	raw, err := os.ReadFile(filepath.Join(root, sid, gen, "run.json"))
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("run.json is not JSON: %w", err)
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

// startHostSession is relay session start on the host project.
func startHostSession(ctx context.Context, e env, projectID string) (string, error) {
	r := cliWithTimeout(ctx, e, 60*time.Second, "session", "start", "--project", projectID, "--model", agentModel,
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

func hostIDByName(ctx context.Context, e env, token, name string) (string, error) {
	resp := frontendDo(ctx, e, token, http.MethodGet, "/api/hosts", nil)
	if resp.Status != http.StatusOK {
		return "", fmt.Errorf("GET /api/hosts status %d", resp.Status)
	}
	var hosts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(resp.Body, &hosts) != nil {
		var wrapped struct {
			Hosts []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"hosts"`
		}
		if json.Unmarshal(resp.Body, &wrapped) != nil {
			return "", errors.New("GET /api/hosts body is not a host list")
		}
		hosts = wrapped.Hosts
	}
	for _, h := range hosts {
		if h.Name == name {
			return h.ID, nil
		}
	}
	return "", fmt.Errorf("host %s is not in GET /api/hosts", name)
}

// setHostDetach sets hosts[].detach_sessions on one host in settings.json.
// This is the power-user door: no API route carries the field. The file is
// replaced by rename, which relay's file watch imports.
func setHostDetach(dir, hostID string) error {
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
	var hosts []map[string]json.RawMessage
	if err := json.Unmarshal(top["hosts"], &hosts); err != nil {
		return fmt.Errorf("settings.json hosts: %w", err)
	}
	found := false
	for _, h := range hosts {
		var hid string
		if json.Unmarshal(h["id"], &hid) == nil && hid == hostID {
			h["detach_sessions"] = json.RawMessage("true")
			found = true
		}
	}
	if !found {
		return fmt.Errorf("host %s is not in settings.json", hostID)
	}
	top["hosts"], _ = json.Marshal(hosts)
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
