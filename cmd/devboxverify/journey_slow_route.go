package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

const (
	slowRouteID           = "slow-route-keepalive"
	blackholePrefix       = "blackhole-"
	blackholeTarget       = "192.0.2.1"
	unreachableHostPrefix = "Unreachable Host "
	hostCreateTimeout     = 40 * time.Second
	modelsCallTimeout     = 10 * time.Second
	slowCallTimeout       = 30 * time.Second
	slowRouteFloor        = 10 * time.Second
)

// keepaliveCall is one request on the journey's single keep-alive client.
type keepaliveCall struct {
	Status   int    // 0 when no response arrived
	Error    string // transport error text, if any
	TimedOut bool
	Reused   bool // httptrace.GotConnInfo.Reused
	Elapsed  time.Duration
}

type slowRouteRun struct {
	HostErr               string // S2 failed: transport error, non-201, or no id
	ProjectErr            string // S3 failed: transport error, non-201, or no id
	Baseline, Slow, After keepaliveCall
	Teardown              string // "; teardown: DELETE project|host status N"..., appended to every detail; "" when clean
}

type hostEntry struct{ ID, Name, Target string }

func runSlowRouteKeepalive(ctx context.Context, e env) result {
	token, res, ok := runCredential(e, slowRouteID)
	if !ok {
		return res
	}
	return classifySlowRouteKeepalive(driveSlowRoute(ctx, e, token))
}

func driveSlowRoute(ctx context.Context, e env, token string) (r slowRouteRun) {
	hostName := blackholePrefix + e.Nonce
	hostResp := frontendDoTimeout(ctx, e, token, http.MethodPost, "/api/hosts", jsonBody(map[string]string{
		"name": hostName, "target": blackholeTarget, "tmux_path": "/usr/bin/tmux",
	}), hostCreateTimeout)
	hostID, errText := createdID("POST /api/hosts", hostResp)
	if hostID == "" {
		r.HostErr = errText
		return r
	}
	var projectID string
	defer func() { r.Teardown = teardownSlowRoute(context.WithoutCancel(ctx), e, token, projectID, hostID) }()

	projName := unreachableHostPrefix + e.Nonce
	projResp, d := gatedFrontend(ctx, e, token, http.MethodPost, "/api/projects", jsonBody(map[string]string{
		"name": projName, "path": "/srv/devboxverify/" + e.Nonce, "host_id": hostID,
	}), fmt.Sprintf("%q", projName))
	if projectID, errText = createdID("POST /api/projects", projResp); projectID == "" {
		r.ProjectErr = fmt.Sprintf("%s (prompt: %s %s)", errText, d.Outcome, d.Detail)
		return r
	}

	c := keepaliveClient(e)
	defer c.CloseIdleConnections()
	r.Baseline = keepaliveGet(ctx, c, token, "/api/models", modelsCallTimeout)
	if r.Baseline.Status != http.StatusOK {
		return r
	}
	r.Slow = keepaliveGet(ctx, c, token, "/api/projects/"+projectID+"/persistent-sessions", slowCallTimeout)
	r.After = keepaliveGet(ctx, c, token, "/api/models", modelsCallTimeout)
	return r
}

func createdID(act string, resp frontendResponse) (id, errText string) {
	switch {
	case resp.TimedOut:
		return "", act + ": no answer before the timeout"
	case resp.Status == 0:
		return "", act + ": frontend socket unreachable"
	case resp.Status != http.StatusCreated:
		return "", fmt.Sprintf("%s status %d: %s", act, resp.Status, resp.Error)
	}
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resp.Body, &v) != nil || v.ID == "" {
		return "", act + ": 201 without an id"
	}
	return v.ID, ""
}

// keepaliveClient holds one connection so every call after the first either
// reuses it or visibly does not.
func keepaliveClient(e env) *http.Client {
	return &http.Client{Transport: &http.Transport{
		MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}}
}

func keepaliveGet(ctx context.Context, c *http.Client, token, path string, timeout time.Duration) keepaliveCall {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var call keepaliveCall
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { call.Reused = i.Reused }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, "http://relay"+path, nil)
	if err != nil {
		call.Error = err.Error()
		return call
	}
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		call.Elapsed = time.Since(start)
		var ne net.Error
		call.Error = err.Error()
		call.TimedOut = errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
		return call
	}
	// Draining to EOF before Close is what returns the connection to the
	// pool; without it the next call dials fresh and reuse proves nothing.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	call.Elapsed = time.Since(start)
	call.Status = resp.StatusCode
	return call
}

func teardownSlowRoute(ctx context.Context, e env, token, projectID, hostID string) string {
	var out strings.Builder
	del := func(kind, path string) {
		st := frontendDo(ctx, e, token, http.MethodDelete, path, nil).Status
		if st != http.StatusNoContent && st != http.StatusNotFound {
			fmt.Fprintf(&out, "; teardown: DELETE %s status %d", kind, st)
		}
	}
	if projectID != "" {
		del("project", "/api/projects/"+projectID)
	}
	del("host", "/api/hosts/"+hostID)
	return out.String()
}

func describeCall(c keepaliveCall) string {
	reuse := "new connection"
	if c.Reused {
		reuse = "reused connection"
	}
	switch {
	case c.TimedOut:
		return fmt.Sprintf("timed out after %s on a %s", c.Elapsed.Round(100*time.Millisecond), reuse)
	case c.Status == 0:
		return fmt.Sprintf("no response on a %s: %s", reuse, c.Error)
	}
	return fmt.Sprintf("status %d in %s on a %s", c.Status, c.Elapsed.Round(100*time.Millisecond), reuse)
}

func classifySlowRouteKeepalive(r slowRouteRun) result {
	res := judgeSlowRoute(r)
	res.Detail += r.Teardown
	return res
}

func judgeSlowRoute(r slowRouteRun) result {
	const id = slowRouteID
	switch {
	case r.HostErr != "":
		return blocked(id, "fixture host: "+r.HostErr)
	case r.ProjectErr != "":
		return blocked(id, "fixture project: "+r.ProjectErr)
	case r.Baseline.Status != http.StatusOK:
		return blocked(id, "baseline GET /api/models: "+describeCall(r.Baseline))
	case r.Slow.Status != http.StatusBadGateway || r.Slow.Elapsed < slowRouteFloor || !r.Slow.Reused:
		return blocked(id, "the slow route did not answer 502 after 10s on the reused connection: "+describeCall(r.Slow))
	case !r.After.Reused || r.After.TimedOut || r.After.Status != http.StatusOK:
		return result{id, stateFail, "GET /api/models after the slow route: " + describeCall(r.After)}
	}
	return result{id, statePass, "GET /api/models answered 200 on the same connection after a " +
		r.Slow.Elapsed.Round(100*time.Millisecond).String() + " slow route answered 502"}
}
