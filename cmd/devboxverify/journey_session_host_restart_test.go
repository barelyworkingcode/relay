package main

import (
	"errors"
	"testing"
)

func TestClassifySessionHostRestart(t *testing.T) {
	base := func() hostRestartRun {
		return hostRestartRun{Before: svcView{State: "running", PIDs: []int{101}},
			After: svcView{State: "running", PIDs: []int{202}}, Back: true}
	}
	checkMuts(t, base, classifySessionHostRestart, []mutCase[hostRestartRun]{
		{"restarted with a new pid", func(*hostRestartRun) {}, statePass},
		{"service.list unreadable", func(r *hostRestartRun) { r.Before = svcView{Err: errors.New("service.list: bridge closed")} }, stateBlocked},
		{"host not running before", func(r *hostRestartRun) { r.Before = svcView{State: "-"} }, stateBlocked},
		{"running without a process", func(r *hostRestartRun) { r.Before.PIDs = nil }, stateBlocked},
		{"restart errored", func(r *hostRestartRun) {
			r.RestartErr = errors.New("restart service: invalid service config: service command is required")
			r.After, r.Back = svcView{}, false
		}, stateFail},
		{"not back within 15 s", func(r *hostRestartRun) { r.After, r.Back = svcView{State: "-"}, false }, stateFail},
		{"poll failed after restart", func(r *hostRestartRun) { r.After, r.Back = svcView{Err: errors.New("pgrep failed")}, false }, stateBlocked},
	}, map[string]string{
		"restart errored":      "service command is required",
		"not back within 15 s": "STATE -",
	})
}
