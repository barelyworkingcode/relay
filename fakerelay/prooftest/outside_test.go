package prooftest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const outsideAPI = "/api/chief-of-staff/outside-sessions"

func outsideFixture(text string) J {
	return J{"id": "outside:acme", "name": "Acme Claude", "project_id": nil, "host_id": nil, "machine": "Console", "directory": "/work/acme",
		"branch": "main", "state": "idle", "since": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z", "source": "probe",
		"outside": true, "read_only": true, "tmux_session": nil, "viewable": false, "view_reason": "not_in_tmux",
		"latest_turn": J{"text": text, "at": "2026-01-01T00:00:00Z", "complete": true}}
}
func outsideFake(t *testing.T, rows []J, hosts []J) *instance {
	return startInstance(t, func(string) any {
		return J{"schema": 1, "credentials": creds(), "outside_sessions": rows, "hosts": hosts}
	})
}
func outsideScoped(in *instance, t *testing.T, method, path string, body any) res {
	return in.api(t, method, path, body, "X-Relay-Scope", "chief-of-staff")
}

// Regression: fixture updates retain old membership/turns or lose display-only wire fields.
func TestOutsideFixtureSnapshotAndFreshDetail(t *testing.T) {
	row := outsideFixture("old reply")
	in := outsideFake(t, []J{row}, nil)
	c := in.ws(t, "/ws/outside-sessions", opsTok, "X-Relay-Scope", "chief-of-staff")
	s := until(t, c, isType("outside_sessions"))[0]
	eq[any](t, s["probeIntervalMs"], float64(15000), "probe cadence")
	listed := s["sessions"].([]any)[0].(map[string]any)
	for key, want := range (J{"id": "outside:acme", "outside": true, "readOnly": true, "directory": "/work/acme", "branch": "main", "state": "idle", "source": "probe", "viewable": false, "viewReason": "not_in_tmux"}) {
		eq[any](t, listed[key], want, key)
	}
	before := s["revision"].(float64)
	text := strings.Repeat("界", 8200) + "tail"
	row["latest_turn"] = J{"text": text, "at": "2026-01-01T00:01:00Z", "complete": false}
	row["state"] = "running"
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}).is(t, 200)
	frames := until(t, c, func(f J) bool { return f["type"] == "outside_sessions" && f["revision"].(float64) > before })
	next := frames[len(frames)-1]
	eq[any](t, next["sessions"].([]any)[0].(map[string]any)["state"], "running", "updated state")
	d := outsideScoped(in, t, "GET", outsideAPI+"/outside:acme", nil).is(t, 200).obj(t)
	turn := d["lastTurn"].(map[string]any)
	eq[any](t, turn["text"], strings.Repeat("界", 8188)+"tail", "latest text cap")
	eq[any](t, d["lastExcerpt"], strings.Repeat("界", 496)+"tail", "same-turn excerpt")
	eq[any](t, turn["complete"], false, "partial complete flag")
	row["latest_turn"] = J{"text": "", "at": "", "complete": false}
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}).is(t, 200)
	empty := outsideScoped(in, t, "GET", outsideAPI+"/outside:acme", nil).is(t, 200).obj(t)["lastTurn"].(map[string]any)
	eq[any](t, empty["text"], "", "empty latest text")
	eq[any](t, empty["at"], "", "empty latest at")
	eq[any](t, empty["complete"], false, "empty completion")

	row["machine_status"] = "unreachable"
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}).is(t, 200)
	eq[any](t, outsideScoped(in, t, "GET", outsideAPI+"/outside:acme", nil).is(t, 502).obj(t)["error"], "outside_status_unavailable", "failed read")
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{}).is(t, 200)
	until(t, c, func(f J) bool { return f["type"] == "outside_sessions" && len(f["sessions"].([]any)) == 0 })
	eq[any](t, outsideScoped(in, t, "GET", outsideAPI+"/outside:acme", nil).is(t, 404).obj(t)["error"], "outside_session_not_found", "expired identity")
	reconnect := in.ws(t, "/ws/outside-sessions", opsTok, "X-Relay-Scope", "chief-of-staff")
	reseed := until(t, reconnect, isType("outside_sessions"))[0]
	eq[any](t, len(reseed["sessions"].([]any)), 0, "reconnect full membership")
}

// Regression: fixture mutation accepts invalid identity/state or outside data gains action authority.
func TestOutsideFixtureValidationAndScope(t *testing.T) {
	in := outsideFake(t, []J{outsideFixture("")}, nil)
	for _, c := range []struct {
		field string
		value any
	}{{"id", "managed"}, {"state", "unknown"}, {"source", "unknown"}} {
		row := outsideFixture("")
		row[c.field] = c.value
		in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}).is(t, 400)
	}
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{outsideFixture(""), outsideFixture("")}).is(t, 400)
	for _, path := range []string{"/api/tmux/views", "/api/sessions/outside:acme/send"} {
		outsideScoped(in, t, "POST", path, J{"sessionName": "acme", "text": "input"}).is(t, http.StatusForbidden)
	}
	in.apiAs(t, roTok, "GET", outsideAPI, nil).is(t, 403)
}

// Regression: the fixture transcript route pages from the wrong end, repeats or drops an entry,
// leaks past the scope or the read class, or accepts a transcript the contract bounds.
func TestOutsideFixtureTranscriptPages(t *testing.T) {
	row := outsideFixture("")
	var entries []J
	for n := 0; n < 120; n++ {
		entries = append(entries, J{"role": []string{"user", "assistant", "tool"}[n%3], "text": fmt.Sprintf("acme %03d", n), "at": ""})
	}
	row["transcript"] = entries
	in := outsideFake(t, []J{row}, nil)
	path := "/api/outside-sessions/outside:acme/transcript"

	var got []string
	query, pages := "", 0
	for {
		page := in.api(t, "GET", path+query, nil).is(t, 200).obj(t)
		pages++
		es := page["entries"].([]any)
		var texts []string
		for _, e := range es {
			texts = append(texts, e.(map[string]any)["text"].(string))
		}
		got = append(texts, got...)
		if page["cursor"] == nil {
			eq[any](t, page["hasMore"], false, "last page hasMore")
			break
		}
		eq[any](t, page["hasMore"], true, "hasMore with a cursor")
		query = "?before=" + page["cursor"].(string)
		if pages > 5 {
			t.Fatalf("still paging after %d pages", pages)
		}
	}
	eq(t, pages, 3, "pages of 50 over 120 entries")
	eq(t, len(got), 120, "entries over all pages")
	for n, text := range got {
		eq(t, text, fmt.Sprintf("acme %03d", n), "entry order")
	}

	row["machine_status"] = "unreachable"
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}).is(t, 200)
	eq[any](t, in.api(t, "GET", path, nil).is(t, 502).obj(t)["error"], "outside_status_unavailable", "unreachable machine")
	outsideScoped(in, t, "GET", path, nil).is(t, http.StatusForbidden)
	for _, q := range []string{"?limit=%zz", "?before=5;x=1"} {
		eq[any](t, in.api(t, "GET", path+q, nil).is(t, 400).obj(t)["error"], "invalid_request", "malformed query "+q)
	}

	for name, bad := range map[string]J{
		"unknown role":  {"role": "system", "text": "x", "at": ""},
		"text too long": {"role": "user", "text": strings.Repeat("界", 8193), "at": ""},
	} {
		r := outsideFixture("")
		r["transcript"] = []J{bad}
		if got := in.ctl(t, "PUT", "/v1/outside-sessions", []J{r}); got.Status != 400 {
			t.Fatalf("%s: fixture replace answered %d, want 400: %s", name, got.Status, got.Body)
		}
	}
}

// Regression: the fixture transcript drops or invents kind and name, tags an entry an older relay
// would not, or accepts a tool tag the contract refuses.
func TestOutsideFixtureTranscriptToolKinds(t *testing.T) {
	const at = "2026-01-01T00:00:00Z"
	row := outsideFixture("")
	row["transcript"] = []J{
		{"role": "user", "text": "acme prompt", "at": at},
		{"role": "tool", "text": "Bash ls", "at": at, "kind": "call", "name": "Bash"},
		{"role": "tool", "text": "unnamed", "at": at, "kind": "call", "name": ""},
		{"role": "tool", "text": "acme output", "at": at, "kind": "result"},
		{"role": "tool", "text": "old shape", "at": at},
	}
	in := outsideFake(t, []J{row}, nil)
	page := in.api(t, "GET", "/api/outside-sessions/outside:acme/transcript", nil).is(t, 200).obj(t)
	entries := page["entries"].([]any)
	eq(t, len(entries), 5, "entries")
	wantKeys := [][]string{{"at", "id", "role", "text"}, {"at", "id", "kind", "name", "role", "text"}, {"at", "id", "kind", "name", "role", "text"}, {"at", "id", "kind", "role", "text"}, {"at", "id", "role", "text"}}
	for n, e := range entries {
		m := e.(map[string]any)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		eq(t, keys, wantKeys[n], fmt.Sprintf("entry %d keys", n))
	}
	eq[any](t, entries[1].(map[string]any)["kind"], "call", "call kind")
	eq[any](t, entries[1].(map[string]any)["name"], "Bash", "call name")
	eq[any](t, entries[2].(map[string]any)["name"], "", "empty call name")
	eq[any](t, entries[3].(map[string]any)["kind"], "result", "result kind")

	for name, bad := range map[string]J{
		"kind on a user entry": {"role": "user", "text": "x", "at": at, "kind": "call", "name": "Bash"},
		"unknown kind":         {"role": "tool", "text": "x", "at": at, "kind": "other"},
		"name on a result":     {"role": "tool", "text": "x", "at": at, "kind": "result", "name": "Bash"},
		"call without name":    {"role": "tool", "text": "x", "at": at, "kind": "call"},
		"name too long":        {"role": "tool", "text": "x", "at": at, "kind": "call", "name": strings.Repeat("界", 8193)},
	} {
		r := outsideFixture("")
		r["transcript"] = []J{bad}
		if got := in.ctl(t, "PUT", "/v1/outside-sessions", []J{r}); got.Status != 400 {
			t.Fatalf("%s: fixture replace answered %d, want 400: %s", name, got.Status, got.Body)
		}
	}
}

func outsideEnded(id, endedAt string) J {
	r := outsideFixture("last words")
	r["id"] = id
	r["directory"] = "/work/" + strings.TrimPrefix(id, "outside:")
	r["state"] = "ended"
	r["ended_at"] = endedAt
	return r
}

// Regression: ended fixture rows lose endedAt, leak onto live rows, ignore the order or the cap of 50,
// or open the live-only transcript route.
func TestOutsideFixtureEndedRows(t *testing.T) {
	liveB, liveA := outsideFixture(""), outsideFixture("")
	liveB["id"], liveB["directory"] = "outside:live-b", "/work/live-b"
	liveA["id"], liveA["directory"] = "outside:live-a", "/work/live-a"
	rows := []J{outsideEnded("outside:old", "2026-01-01T01:00:00Z"), liveB, outsideEnded("outside:new", "2026-01-01T03:00:00Z"), liveA}
	in := outsideFake(t, rows, nil)
	c := in.ws(t, "/ws/outside-sessions", opsTok, "X-Relay-Scope", "chief-of-staff")
	s := until(t, c, isType("outside_sessions"))[0]
	var ids []string
	for _, v := range s["sessions"].([]any) {
		row := v.(map[string]any)
		ids = append(ids, row["id"].(string))
		_, has := row["endedAt"]
		if (row["state"] == "ended") != has {
			t.Fatalf("row %v: state %v with endedAt present %v, want endedAt on ended rows only", row["id"], row["state"], has)
		}
		if has {
			eq[any](t, row["readOnly"], true, "ended row readOnly")
			eq[any](t, row["outside"], true, "ended row outside")
		}
	}
	eq[any](t, strings.Join(ids, ","), "outside:live-b,outside:live-a,outside:new,outside:old", "live rows in fixture order, then ended newest first")
	first := s["sessions"].([]any)[2].(map[string]any)
	eq[any](t, first["endedAt"], "2026-01-01T03:00:00Z", "endedAt")

	d := outsideScoped(in, t, "GET", outsideAPI+"/outside:new", nil).is(t, 200).obj(t)
	eq[any](t, d["session"].(map[string]any)["state"], "ended", "detail state")
	eq[any](t, d["lastTurn"].(map[string]any)["text"], "last words", "detail last turn")
	eq[any](t, in.api(t, "GET", "/api/outside-sessions/outside:new/transcript", nil).is(t, 404).obj(t)["error"], "outside_session_not_found", "ended transcript route")

	var many []J
	for k := 0; k < 52; k++ {
		many = append(many, outsideEnded(fmt.Sprintf("outside:e%02d", k), fmt.Sprintf("2026-01-01T%02d:%02d:00Z", k/60, k%60)))
	}
	before := s["revision"].(float64)
	in.ctl(t, "PUT", "/v1/outside-sessions", many).is(t, 200)
	frames := until(t, c, func(f J) bool { return f["type"] == "outside_sessions" && f["revision"].(float64) > before })
	list := frames[len(frames)-1]["sessions"].([]any)
	eq[any](t, len(list), 50, "ended cap")
	eq[any](t, list[0].(map[string]any)["id"], "outside:e51", "newest ended first")
	eq[any](t, list[49].(map[string]any)["id"], "outside:e02", "oldest kept")

	bad := outsideFixture("")
	bad["ended_at"] = "2026-01-01T00:00:00Z"
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{bad}).is(t, 400)
	noStamp := outsideEnded("outside:nostamp", "")
	delete(noStamp, "ended_at")
	in.ctl(t, "PUT", "/v1/outside-sessions", []J{noStamp}).is(t, 400)
}
