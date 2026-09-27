package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func slowRoutePass() slowRouteRun {
	return slowRouteRun{
		Baseline: keepaliveCall{Status: http.StatusOK, Elapsed: 50 * time.Millisecond},
		Slow:     keepaliveCall{Status: http.StatusBadGateway, Reused: true, Elapsed: 10 * time.Second},
		After:    keepaliveCall{Status: http.StatusOK, Reused: true, Elapsed: 50 * time.Millisecond},
	}
}

func TestClassifySlowRouteKeepalive(t *testing.T) {
	afterBroken := keepaliveCall{Status: http.StatusBadGateway, Reused: true, Elapsed: time.Millisecond}
	cases := []mutCase[slowRouteRun]{
		{"slow route 502 after 10s, after 200 on the reused connection", func(*slowRouteRun) {}, statePass},
		{"nothing ran", func(r *slowRouteRun) { *r = slowRouteRun{} }, stateBlocked},
		{"host create failed", func(r *slowRouteRun) { r.HostErr = "POST /api/hosts status 409" }, stateBlocked},
		{"project create failed", func(r *slowRouteRun) { r.ProjectErr = "POST /api/projects status 500" }, stateBlocked},
		{"baseline not 200", func(r *slowRouteRun) { r.Baseline.Status = http.StatusBadGateway }, stateBlocked},
		{"baseline no response", func(r *slowRouteRun) { r.Baseline = keepaliveCall{Error: "dial unix: no such file"} }, stateBlocked},
		{"slow answered under 10s", func(r *slowRouteRun) { r.Slow.Elapsed = 10*time.Second - time.Millisecond }, stateBlocked},
		{"slow not reused", func(r *slowRouteRun) { r.Slow.Reused = false }, stateBlocked},
		{"after not reused", func(r *slowRouteRun) { r.After.Reused = false }, stateFail},
		{"after timed out", func(r *slowRouteRun) {
			r.After = keepaliveCall{TimedOut: true, Reused: true, Elapsed: 10 * time.Second, Error: "context deadline exceeded"}
		}, stateFail},
		{"after no response", func(r *slowRouteRun) { r.After = keepaliveCall{Reused: true, Error: "EOF"} }, stateFail},
		{"after 502", func(r *slowRouteRun) { r.After = afterBroken }, stateFail},
		{"host failed wins over after 200", func(r *slowRouteRun) { r.HostErr = "POST /api/hosts: 201 without an id" }, stateBlocked},
		{"project failed wins over after 502", func(r *slowRouteRun) { r.ProjectErr = "boom"; r.After = afterBroken }, stateBlocked},
		{"baseline failed wins over after 502", func(r *slowRouteRun) { r.Baseline.Status = 0; r.After = afterBroken }, stateBlocked},
		{"slow 404 wins over after 502", func(r *slowRouteRun) { r.Slow.Status = http.StatusNotFound; r.After = afterBroken }, stateBlocked},
	}
	for _, st := range []int{http.StatusNotFound, http.StatusConflict, http.StatusOK, http.StatusInternalServerError} {
		cases = append(cases, mutCase[slowRouteRun]{"slow status " + http.StatusText(st), func(r *slowRouteRun) { r.Slow.Status = st }, stateBlocked})
	}
	checkMuts(t, slowRoutePass, classifySlowRouteKeepalive, cases, nil)
}

func TestClassifySlowRouteKeepaliveAppendsTeardown(t *testing.T) {
	const teardown = "; teardown: DELETE project status 500; teardown: DELETE host status 500"
	cases := map[string]func(*slowRouteRun){
		"pass":    func(*slowRouteRun) {},
		"fail":    func(r *slowRouteRun) { r.After.Status = http.StatusBadGateway },
		"blocked": func(r *slowRouteRun) { r.ProjectErr = "POST /api/projects status 500" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			clean := slowRoutePass()
			mut(&clean)
			dirty := clean
			dirty.Teardown = teardown
			want, got := classifySlowRouteKeepalive(clean), classifySlowRouteKeepalive(dirty)
			if got.State != want.State {
				t.Errorf("teardown changed the state: %s, want %s", got.State, want.State)
			}
			if !strings.HasSuffix(got.Detail, teardown) {
				t.Errorf("detail %q does not end with the teardown %q", got.Detail, teardown)
			}
		})
	}
}
