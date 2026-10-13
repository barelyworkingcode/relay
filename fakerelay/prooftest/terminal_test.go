package prooftest

import (
	"encoding/base64"
	"testing"
)

// Regression: fixture viewers lose read-only metadata on join/reconnect, accept
// input/resize, prefix-match a target, or close the observed outside session.
func TestOutsideViewerFixtureMetadataAndRefusals(t *testing.T) {
	host := J{"id": "h_acme", "name": "testbox", "target": "acme@testbox", "show_all_tmux_sessions": true, "tmux_path": "/usr/bin/tmux", "tmux_view_supported": true,
		"persistent_sessions": []J{{"name": "acme-work", "created": 1767225600, "attached": 0}}}
	row := outsideFixture("Acme working")
	row["host_id"] = "h_acme"
	row["machine"] = "testbox"
	row["tmux_session"] = "acme-work"
	row["viewable"] = true
	row["view_reason"] = ""
	in := outsideFake(t, []J{row}, []J{host})
	for _, c := range []struct {
		body   J
		status int
		code   string
	}{
		{J{"hostId": "h_acme", "sessionName": "acme"}, 404, "tmux_session_not_found"},
		{J{"hostId": "h_acme", "sessionName": "acme-work", "writable": true}, 400, "invalid_request"},
		{J{"hostId": "h_acme", "sessionName": "acme-work", "cols": -1}, 400, "invalid_request"},
		{J{"hostId": "h_acme", "sessionName": "acme-work", "rows": 65536}, 400, "invalid_request"},
	} {
		eq[any](t, in.api(t, "POST", "/api/tmux/views", c.body).is(t, c.status).obj(t)["error"], c.code, "viewer refusal")
	}
	v := in.api(t, "POST", "/api/tmux/views", J{"hostId": "h_acme", "sessionName": "acme-work"}).is(t, 201).obj(t)
	eq[any](t, v["readOnly"], true, "POST readOnly")
	eq[any](t, v["outside"], true, "POST outside")
	id := v["terminalId"]
	for n := 0; n < 2; n++ {
		c := in.ws(t, "/ws", opsTok)
		must(t, c.WriteJSON(J{"type": "join_terminal", "terminalId": id}), "join viewer")
		f := until(t, c, isType("terminal_joined", "terminalId", id))
		joined := f[len(f)-1]
		eq[any](t, joined["readOnly"], true, "join/reconnect readOnly")
		eq[any](t, joined["outside"], true, "join/reconnect outside")
		eq[any](t, joined["cols"], float64(80), "join/reconnect cols")
		eq[any](t, joined["rows"], float64(24), "join/reconnect rows")
		for _, message := range []J{{"type": "terminal_input", "terminalId": id, "data": base64.StdEncoding.EncodeToString([]byte("input"))}, {"type": "terminal_resize", "terminalId": id, "cols": 120, "rows": 40}} {
			must(t, c.WriteJSON(message), "viewer mutation")
			until(t, c, isType("error", "code", "terminal_read_only"))
		}
		must(t, c.WriteJSON(J{"type": "terminal_list"}), "viewer list")
		list := until(t, c, isType("terminal_list"))
		terms := list[len(list)-1]["terminals"].([]any)
		found := false
		for _, x := range terms {
			r := x.(map[string]any)
			if r["id"] == id {
				found = true
				eq[any](t, r["readOnly"], true, "list readOnly")
				eq[any](t, r["outside"], true, "list outside")

			}
		}
		if !found {
			t.Fatal("viewer missing from existing hub list")
		}
		c.Close()
	}
	c := in.ws(t, "/ws", opsTok)
	must(t, c.WriteJSON(J{"type": "terminal_close", "terminalId": id}), "close viewer")
	until(t, c, isType("terminal_closed", "terminalId", id))
	outsideScoped(in, t, "GET", outsideAPI+"/outside:acme", nil).is(t, 200)
}

// Regression: host viewer opt-in, read-only capability or fresh reachability is ignored.
func TestOutsideViewerFixtureHostRefusals(t *testing.T) {
	for _, c := range []struct {
		name               string
		enabled, supported bool
		status             int
		code               string
	}{
		{"opt_in_off", false, true, 403, "outside_view_disabled"}, {"unsupported", true, false, 409, "tmux_view_unsupported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			host := J{"id": "h_acme", "name": "testbox", "target": "acme@testbox", "show_all_tmux_sessions": c.enabled, "tmux_path": "/usr/bin/tmux", "tmux_view_supported": c.supported, "persistent_sessions": []J{{"name": "acme-work"}}}
			in := outsideFake(t, nil, []J{host})
			eq[any](t, in.api(t, "POST", "/api/tmux/views", J{"hostId": "h_acme", "sessionName": "acme-work"}).is(t, c.status).obj(t)["error"], c.code, "host refusal")
		})
	}
}
