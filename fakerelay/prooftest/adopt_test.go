package prooftest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

const adoptPath = "/api/sessions/adopt"

func adoptWorld(string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{
			{"id": "p_acme", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}},
			{"id": "p_narrow", "name": "Narrow", "mode": "work", "allowed_models": []string{"haiku"}},
			{"id": "p_box", "name": "Box", "mode": "work", "host_id": "h_box", "path": "/home/acme/app", "allowed_models": []string{"*"}},
		},
		"hosts": []J{{"id": "h_box", "name": "testbox", "target": "acme@testbox", "agent": "connected",
			"probe":              J{"ok": true, "os": "Linux", "arch": "x86_64", "home": "/home/acme", "shell": "/bin/bash"},
			"terminal_templates": []J{{"id": "shell", "name": "Shell"}}}}}
}

// adoptEnv is one instance and the folders of its two console projects.
type adoptEnv struct {
	in     *instance
	dir    string
	narrow string
}

func startAdopt(t *testing.T) adoptEnv {
	t.Helper()
	in := startInstance(t, adoptWorld)
	path := func(id string) string {
		return in.api(t, "GET", "/api/projects/"+id, nil).is(t, 200).obj(t)["path"].(string)
	}
	return adoptEnv{in: in, dir: path("p_acme"), narrow: path("p_narrow")}
}

func adoptID(n int) string   { return fmt.Sprintf("outside:%032x", n) }
func adoptUUID(n int) string { return fmt.Sprintf("0b6e1f2a-3c4d-4e5f-8a9b-%012x", n) }

// adoptRow is a live, adoptable row in project p_acme; kv pairs override keys, a nil value deletes one.
func adoptRow(n int, dir string, kv ...any) J {
	r := outsideFixture("")
	r["id"], r["name"] = adoptID(n), fmt.Sprintf("Acme Claude %d", n)
	r["project_id"], r["directory"] = "p_acme", dir
	r["claude_session_id"], r["adoptable"] = adoptUUID(n), true
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(r, kv[i].(string))
		} else {
			r[kv[i].(string)] = kv[i+1]
		}
	}
	return r
}

func putRows(t *testing.T, in *instance, rows ...J) {
	t.Helper()
	if rows == nil {
		rows = []J{}
	}
	if got := in.ctl(t, "PUT", "/v1/outside-sessions", rows); got.Status != 200 {
		t.Fatalf("fixture replace answered %d: %s", got.Status, got.Body)
	}
}

func adoptCall(t *testing.T, in *instance, body any, trace string) res {
	t.Helper()
	return in.api(t, "POST", adoptPath, body, "X-Trace-Id", trace)
}

// adoptRaw posts bytes as they are, which the JSON client cannot.
func adoptRaw(t *testing.T, in *instance, raw, trace string) res {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "POST", "http://relay"+adoptPath, strings.NewReader(raw))
	must(t, err, "request")
	req.Header.Set("Authorization", "Bearer "+opsTok)
	req.Header.Set("X-Trace-Id", trace)
	resp, err := in.sockClient().Do(req)
	must(t, err, "raw adopt")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return res{resp.StatusCode, b, resp.Header}
}

// adoptEvent waits on the append notice for the session.adopt line of one trace.
func adoptEvent(t *testing.T, in *instance, trace string) J {
	t.Helper()
	out, se, code := in.cliTrace(t, trace, "logs", "--json", "--follow", "--timeout", waitBound.String(), "--event", "session.adopt")
	if code != 0 {
		t.Fatalf("waiting for session.adopt: exit %d: %s%s", code, out, se)
	}
	rows := parseRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("session.adopt lines for trace %s: %v", trace, rows)
	}
	return rows[0]
}

func detail(t *testing.T, in *instance, id string) res {
	t.Helper()
	return outsideScoped(in, t, "GET", outsideAPI+"/"+id, nil)
}

func sessionRows(t *testing.T, in *instance) []J {
	t.Helper()
	var out []J
	for _, s := range in.api(t, "GET", "/api/sessions", nil).is(t, 200).obj(t)["sessions"].([]any) {
		out = append(out, s.(J))
	}
	return out
}

func snapshotAbove(t *testing.T, c *websocket.Conn, rev float64) J {
	t.Helper()
	fr := until(t, c, func(f J) bool { return f["type"] == "outside_sessions" && f["revision"].(float64) > rev })
	return fr[len(fr)-1]
}

func snapshotIDs(snap J) []string {
	var ids []string
	for _, v := range snap["sessions"].([]any) {
		ids = append(ids, v.(map[string]any)["id"].(string))
	}
	return ids
}

// Regression: a row is served without adoptable, with a wrong default reason, or without its
// claudeSessionId on one of the list, detail and snapshot doors.
func TestAdoptRowFieldsServed(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	ended := outsideEnded("outside:gone", "2026-01-01T01:00:00Z")
	host := outsideFixture("")
	host["id"], host["directory"], host["host_id"], host["machine"] = "outside:onhost", "/work/onhost", "h_box", "testbox"
	noProject := adoptRow(3, "/work/noproject", "project_id", nil)
	unregistered := outsideFixture("")
	unregistered["id"], unregistered["directory"], unregistered["project_id"] = "outside:unreg", "/work/unreg", "p_acme"
	reasoned := adoptRow(2, e.dir, "adoptable", false, "adopt_reason", "not_interactive")
	adoptable := adoptRow(1, e.dir)

	c := e.in.ws(t, "/ws/outside-sessions", opsTok, "X-Relay-Scope", "chief-of-staff")
	base := until(t, c, isType("outside_sessions"))[0]["revision"].(float64)
	putRows(t, e.in, adoptable, ended, host, noProject, unregistered, reasoned)
	snap := snapshotAbove(t, c, base)

	type want struct {
		adoptable bool
		reason    string
		claudeID  string
	}
	wants := map[string]want{
		adoptID(1):       {true, "", adoptUUID(1)},
		"outside:gone":   {false, "ended", ""},
		"outside:onhost": {false, "host_unsupported", ""},
		adoptID(3):       {true, "", adoptUUID(3)},
		"outside:unreg":  {false, "not_registered", ""},
		adoptID(2):       {false, "not_interactive", adoptUUID(2)},
	}
	check := func(door, id string, row map[string]any) {
		t.Helper()
		w := wants[id]
		eq[any](t, row["adoptable"], w.adoptable, door+" "+id+" adoptable")
		reason, has := row["adoptReason"]
		if w.reason == "" {
			if has {
				t.Errorf("%s %s: adoptable row carries adoptReason %v", door, id, reason)
			}
		} else {
			eq[any](t, reason, w.reason, door+" "+id+" adoptReason")
		}
		cid, has := row["claudeSessionId"]
		if w.claudeID == "" {
			if has {
				t.Errorf("%s %s: row carries claudeSessionId %v, want none", door, id, cid)
			}
		} else {
			eq[any](t, cid, w.claudeID, door+" "+id+" claudeSessionId")
		}
	}
	rows := func(door string, list []any) {
		t.Helper()
		eq(t, len(list), len(wants), door+" rows")
		for _, v := range list {
			row := v.(map[string]any)
			check(door, row["id"].(string), row)
		}
	}
	rows("snapshot", snap["sessions"].([]any))
	rows("list", outsideScoped(e.in, t, "GET", outsideAPI, nil).is(t, 200).obj(t)["sessions"].([]any))
	for id := range wants {
		check("detail", id, detail(t, e.in, id).is(t, 200).obj(t)["session"].(map[string]any))
	}
}

// Regression: the fixture accepts an adoptable row that cannot be adopted, a reason that contradicts
// the row, or an adopt object that steers nothing.
func TestAdoptFixtureRefused(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	valid := func(kv ...any) J { return adoptRow(1, e.dir, kv...) }
	putRows(t, e.in, valid("adopt", J{"busy": "registry_busy", "model_family": "opus", "refuse": "changed"}))
	putRows(t, e.in, valid("adoptable", false, "adopt_reason", "invalid_id"))
	// A Claude row in no project is adoptable, and its folder may be unusable.
	putRows(t, e.in, valid("project_id", nil, "adopt", J{"refuse": "directory_unusable"}))

	cases := map[string]J{
		"adoptable on an ended row":         valid("state", "ended", "ended_at", "2026-01-01T00:00:00Z"),
		"adoptable on a host row":           valid("host_id", "h_box", "machine", "testbox"),
		"no_project reason on a Claude row": valid("adoptable", false, "project_id", nil, "adopt_reason", "no_project"),
		"no_project reason in a project":    valid("adoptable", false, "adopt_reason", "no_project"),
		"directory_unusable in a project":   valid("adopt", J{"refuse": "directory_unusable"}),
		"adoptable with a short id":         valid("id", "outside:acme"),
		"adoptable with an upper id":        valid("id", "outside:"+strings.Repeat("A", 32)),
		"adoptable with a stem id":          valid("claude_session_id", "stem-id"),
		"adoptable without a stem id":       valid("claude_session_id", nil),
		"adoptable with a reason":           valid("adopt_reason", "not_interactive"),
		"unknown reason":                    valid("adoptable", false, "adopt_reason", "bogus"),
		"ended row with another reason":     valid("adoptable", false, "state", "ended", "ended_at", "2026-01-01T00:00:00Z", "adopt_reason", "not_interactive"),
		"host row with another reason":      valid("adoptable", false, "host_id", "h_box", "machine", "testbox", "adopt_reason", "no_project"),
		"first-rule reason on live row":     valid("adoptable", false, "adopt_reason", "ended"),
		"adopt without adoptable":           valid("adoptable", false, "adopt_reason", "not_interactive", "adopt", J{}),
		"bad busy":                          valid("adopt", J{"busy": "bogus"}),
		"bad model family":                  valid("adopt", J{"model_family": "gpt"}),
		"bad refuse":                        valid("adopt", J{"refuse": "bogus"}),
		"unknown adopt key":                 valid("adopt", J{"idle": true}),
	}
	for name, row := range cases {
		if got := e.in.ctl(t, "PUT", "/v1/outside-sessions", []J{row}); got.Status != 400 {
			t.Errorf("%s: fixture replace answered %d, want 400: %s", name, got.Status, got.Body)
		}
	}
}

// Regression: a successful adopt leaves the row, loses the conversation id, ignores the body's model,
// or launches something other than an attended headless session.
func TestAdoptIdleSucceeds(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	one := adoptRow(1, e.dir, "adopt", J{"model_family": "sonnet"})
	two := adoptRow(2, e.dir, "adopt", J{"model_family": "opus"})
	c := e.in.ws(t, "/ws/outside-sessions", opsTok, "X-Relay-Scope", "chief-of-staff")
	base := until(t, c, isType("outside_sessions"))[0]["revision"].(float64)
	putRows(t, e.in, one, two)
	put := snapshotAbove(t, c, base)
	rev := put["revision"].(float64)

	got := adoptCall(t, e.in, J{"outsideId": adoptID(1), "model": "haiku"}, "adopt-idle-0001").is(t, 201).obj(t)
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	eq(t, keys, []string{"claudeSessionId", "interrupted", "recoverySent", "sessionId"}, "answer keys")
	eq[any](t, got["claudeSessionId"], adoptUUID(1), "claudeSessionId")
	eq[any](t, got["interrupted"], false, "interrupted")
	eq[any](t, got["recoverySent"], false, "recoverySent")
	sid := got["sessionId"].(string)

	var row J
	for _, s := range sessionRows(t, e.in) {
		if s["id"] == sid {
			row = s
		}
	}
	if row == nil {
		t.Fatalf("session %s is not listed", sid)
	}
	eq[any](t, row["headless"], true, "headless")
	eq[any](t, row["attention"].(map[string]any)["state"], "idle", "attention state")
	eq[any](t, [3]any{row["name"], row["directory"], row["model"]}, [3]any{one["name"], e.dir, "haiku"}, "name, directory and model")
	if v, ok := row["origin"]; ok {
		t.Errorf("adopted session carries origin %v, want none", v)
	}
	w := e.in.ws(t, "/ws", opsTok)
	must(t, w.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	joined := until(t, w, isType("session_joined", "sessionId", sid))
	eq[any](t, joined[len(joined)-1]["headless"], true, "joined headless")

	snap := snapshotAbove(t, c, rev)
	eq[any](t, snap["revision"], rev+1, "revision after the adopt")
	eq(t, snapshotIDs(snap), []string{adoptID(2)}, "snapshot rows after the adopt")
	list := outsideScoped(e.in, t, "GET", outsideAPI, nil).is(t, 200).obj(t)["sessions"].([]any)
	eq(t, len(list), 1, "listed rows after the adopt")
	detail(t, e.in, adoptID(1)).is(t, 404)

	ev := adoptEvent(t, e.in, "adopt-idle-0001")
	eq[any](t, ev["status"], "ok", "event status")
	eq[any](t, [5]any{ev["outside_id"], ev["claude_session_id"], ev["project_id"], ev["session_id"], ev["interrupted"]},
		[5]any{adoptID(1), adoptUUID(1), "p_acme", sid, false}, "event keys")

	adoptCall(t, e.in, J{"outsideId": adoptID(1)}, "adopt-idle-0002").is(t, 404)

	// The power selector, and the model family when the body names none.
	second := adoptCall(t, e.in, J{"claudeSessionId": adoptUUID(2)}, "adopt-idle-0003").is(t, 201).obj(t)
	eq[any](t, second["claudeSessionId"], adoptUUID(2), "second claudeSessionId")
	for _, s := range sessionRows(t, e.in) {
		if s["id"] == second["sessionId"] {
			eq[any](t, s["model"], "opus", "model from the family")
			return
		}
	}
	t.Fatalf("second session %v is not listed", second["sessionId"])
}

// joinHistory joins sid on a new connection and returns the history of its session_joined frame.
func joinHistory(t *testing.T, in *instance, sid string) []any {
	t.Helper()
	w := in.ws(t, "/ws", opsTok)
	must(t, w.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	fr := until(t, w, isType("session_joined", "sessionId", sid))
	return fr[len(fr)-1]["history"].([]any)
}

// Regression: a busy row is taken over without the caller asking, an interrupt sends no recovery
// message or names the wrong call, or an idle row with interrupt is treated as interrupted.
func TestAdoptBusyInterrupt(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	at := "2026-01-01T00:00:00Z"
	transcript := []J{
		{"role": "user", "text": "first acme prompt", "at": at},
		{"role": "tool", "text": `{"file":"a.txt"}`, "at": at, "kind": "call", "name": "Quokka"},
		{"role": "tool", "text": "done", "at": at, "kind": "result"},
		{"role": "user", "text": "fix the acme build", "at": at},
		{"role": "assistant", "text": "working", "at": at},
		{"role": "tool", "text": `{ "command": "make" }`, "at": at, "kind": "call", "name": "Bash"},
	}
	busy := adoptRow(1, e.dir, "adopt", J{"busy": "open_tool_call", "model_family": "haiku"}, "transcript", transcript)
	idle := adoptRow(2, e.dir, "adopt", J{"model_family": "haiku"}, "transcript", transcript)
	putRows(t, e.in, busy, idle)

	before := detail(t, e.in, adoptID(1)).is(t, 200).obj(t)
	refused := adoptCall(t, e.in, J{"outsideId": adoptID(1)}, "adopt-busy-0001").is(t, 409).obj(t)
	eq[any](t, refused["error"], "busy", "busy code")
	eq[any](t, detail(t, e.in, adoptID(1)).is(t, 200).obj(t), any(before), "row after the busy refusal")
	eq(t, len(sessionRows(t, e.in)), 0, "sessions after the busy refusal")

	got := adoptCall(t, e.in, J{"outsideId": adoptID(1), "interrupt": true}, "adopt-busy-0002").is(t, 201).obj(t)
	eq[any](t, got["interrupted"], true, "interrupted")
	eq[any](t, got["recoverySent"], true, "recoverySent")
	var recovery []string
	for _, m := range joinHistory(t, e.in, got["sessionId"].(string)) {
		msg := m.(map[string]any)
		if msg["role"] == "user" && msg["origin"] == "relay-recovery" {
			recovery = append(recovery, fmt.Sprint(msg["content"]))
		}
	}
	if len(recovery) != 1 {
		t.Fatalf("relay-recovery messages in history: %q", recovery)
	}
	text := recovery[0]
	for _, want := range []string{"fix the acme build", "Bash", "e5", `{"command":"make"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("recovery message lacks %q: %s", want, text)
		}
	}
	for _, not := range []string{"Quokka", "first acme prompt"} {
		if strings.Contains(text, not) {
			t.Errorf("recovery message names %q, which is not the open call or the newest prompt: %s", not, text)
		}
	}
	if !strings.Contains(strings.ToLower(text), "check whether it took effect") {
		t.Errorf("recovery message does not ask the agent to check: %s", text)
	}
	ev := adoptEvent(t, e.in, "adopt-busy-0002")
	eq[any](t, [2]any{ev["status"], ev["interrupted"]}, [2]any{"ok", true}, "interrupted event")

	// An idle row asked to interrupt is taken over as idle: no recovery message.
	calm := adoptCall(t, e.in, J{"outsideId": adoptID(2), "interrupt": true}, "adopt-busy-0003").is(t, 201).obj(t)
	eq[any](t, [2]any{calm["interrupted"], calm["recoverySent"]}, [2]any{false, false}, "idle row with interrupt")
	for _, m := range joinHistory(t, e.in, calm["sessionId"].(string)) {
		if m.(map[string]any)["origin"] == "relay-recovery" {
			t.Errorf("idle adopt sent a recovery message: %v", m)
		}
	}
}

type adoptCase struct {
	name   string
	rows   func(e adoptEnv) []J
	body   func(e adoptEnv) any // a string is posted as it is
	status int
	code   string
	msg    string // relay's wire text, word for word
	parse  bool   // a step 1 refusal: the event holds no keys
}

func (c adoptCase) run(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	var rows []J
	if c.rows != nil {
		rows = c.rows(e)
	}
	putRows(t, e.in, rows...)
	before := map[string][]byte{}
	for _, r := range rows {
		before[r["id"].(string)] = detail(t, e.in, r["id"].(string)).is(t, 200).Body
	}
	trace := "adopt-" + strings.NewReplacer("_", "-", " ", "-").Replace(c.name) + "-0001"
	var got res
	switch b := c.body(e).(type) {
	case string:
		got = adoptRaw(t, e.in, b, trace)
	default:
		got = adoptCall(t, e.in, b, trace)
	}
	got.is(t, c.status)
	eq[any](t, got.obj(t)["error"], c.code, "error code")
	eq[any](t, got.obj(t)["message"], c.msg, "message")

	for id, was := range before {
		eq(t, string(detail(t, e.in, id).is(t, 200).Body), string(was), "row "+id+" after the refusal")
	}
	eq(t, len(sessionRows(t, e.in)), 0, "sessions after the refusal")

	ev := adoptEvent(t, e.in, trace)
	wantStatus := "denied"
	if c.status >= 500 {
		wantStatus = "error"
	}
	eq[any](t, [2]any{ev["status"], ev["reason"]}, [2]any{wantStatus, c.code}, "event status and reason")
	for _, k := range []string{"outside_id", "claude_session_id", "project_id", "session_id", "interrupted"} {
		if _, has := ev[k]; has == c.parse {
			t.Errorf("event key %s present is %v, want %v", k, has, !c.parse)
		}
	}
}

func idBody(extra J) func(adoptEnv) any {
	return func(adoptEnv) any {
		b := J{"outsideId": adoptID(1)}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
}

func oneRow(kv ...any) func(adoptEnv) []J {
	return func(e adoptEnv) []J { return []J{adoptRow(1, e.dir, kv...)} }
}

// Regression: a refusal answers another status or code, judges steps in another order, changes the
// row, launches a session, or writes an event with other keys.
func TestAdoptRefusals(t *testing.T) {
	t.Parallel()
	ended := func(kv ...any) func(adoptEnv) []J {
		return func(e adoptEnv) []J {
			r := adoptRow(1, e.dir, append([]any{"adoptable", false, "state", "ended", "ended_at", "2026-01-01T00:00:00Z"}, kv...)...)
			return []J{r}
		}
	}
	const (
		msgJSON      = `body must be JSON {"outsideId" or "claudeSessionId", "interrupt", "model"}`
		msgSelector  = "send outsideId or claudeSessionId, not both and not neither"
		msgModel     = "model is not a model name"
		msgNotFound  = "no live outside session matches"
		msgEnded     = "the session has ended; there is no process to adopt"
		msgNoProject = "the session is not in a registered local project"
		msgChanged   = "the process or its registry entry changed"
	)
	busyMsg := func(veto string) string {
		return "the session is not idle (" + veto + "); ask to interrupt to take it over anyway"
	}
	cases := []adoptCase{
		{name: "not json", body: func(adoptEnv) any { return "{" }, status: 400, code: "invalid_body", msg: msgJSON, parse: true},
		{name: "unknown field", body: func(adoptEnv) any { return `{"outsideId":"` + adoptID(1) + `","bogus":1}` }, status: 400, code: "invalid_body", msg: msgJSON, parse: true},
		{name: "wrong type", body: func(adoptEnv) any { return `{"outsideId":5}` }, status: 400, code: "invalid_body", msg: msgJSON, parse: true},
		{name: "trailing data", body: func(adoptEnv) any { return `{"outsideId":"` + adoptID(1) + `"} {}` }, status: 400, code: "invalid_body", msg: msgJSON, parse: true},
		{name: "over the cap", body: func(adoptEnv) any { return `{"outsideId":"` + strings.Repeat("a", 5000) + `"}` }, status: 413, code: "body_too_large", msg: "request body is larger than 4 KiB", parse: true},
		{name: "both selectors", body: idBody(J{"claudeSessionId": adoptUUID(1)}), status: 400, code: "invalid_body", msg: msgSelector, parse: true},
		{name: "neither selector", body: func(adoptEnv) any { return J{"interrupt": true} }, status: 400, code: "invalid_body", msg: msgSelector, parse: true},
		{name: "bad outside id", body: func(adoptEnv) any { return J{"outsideId": "outside:acme"} }, status: 400, code: "invalid_body", msg: "outsideId is not an outside session id", parse: true},
		{name: "bad uuid", body: func(adoptEnv) any { return J{"claudeSessionId": "stem-id"} }, status: 400, code: "invalid_body", msg: "claudeSessionId is not a UUID", parse: true},
		{name: "model too long", body: idBody(J{"model": strings.Repeat("m", 201)}), status: 400, code: "invalid_body", msg: msgModel, parse: true},
		{name: "model control", body: idBody(J{"model": "a\x01b"}), status: 400, code: "invalid_body", msg: msgModel, parse: true},
		{name: "unknown outside id", body: func(adoptEnv) any { return J{"outsideId": adoptID(0xf)} }, status: 404, code: "outside_session_not_found", msg: msgNotFound},
		{name: "unknown uuid", body: func(adoptEnv) any { return J{"claudeSessionId": adoptUUID(0xf)} }, status: 404, code: "outside_session_not_found", msg: msgNotFound},
		{name: "ended by id", rows: ended(), body: idBody(nil), status: 409, code: "ended", msg: msgEnded},
		{name: "ended by uuid", rows: ended(), body: func(adoptEnv) any { return J{"claudeSessionId": adoptUUID(1)} }, status: 409, code: "ended", msg: msgEnded},
		{name: "ambiguous", rows: func(e adoptEnv) []J {
			return []J{adoptRow(1, e.dir), adoptRow(2, e.dir, "claude_session_id", adoptUUID(1))}
		}, body: func(adoptEnv) any { return J{"claudeSessionId": adoptUUID(1)} }, status: 409, code: "ambiguous", msg: "more than one live process holds this conversation"},
		{name: "host", rows: oneRow("adoptable", false, "host_id", "h_box", "machine", "testbox"), body: idBody(nil), status: 409, code: "host_unsupported", msg: "sessions on an SSH host cannot be adopted"},
		{name: "no project row not adoptable", rows: oneRow("adoptable", false, "project_id", nil), body: idBody(nil), status: 409, code: "not_registered", msg: "the session cannot be adopted: not_registered"},
		{name: "not registered", rows: oneRow("adoptable", false), body: idBody(nil), status: 409, code: "not_registered", msg: "the session cannot be adopted: not_registered"},
		{name: "audit off", rows: oneRow("adopt", J{"refuse": "audit_unavailable"}), body: idBody(nil), status: 503, code: "audit_unavailable", msg: "auditing is off; no session can be adopted"},
		{name: "host down", rows: oneRow("adopt", J{"refuse": "unavailable"}), body: idBody(nil), status: 502, code: "unavailable", msg: "the session host is not available"},
		{name: "in progress", rows: oneRow("adopt", J{"refuse": "adopt_in_progress"}), body: idBody(nil), status: 409, code: "adopt_in_progress", msg: "an adopt of this conversation is already running"},
		{name: "changed", rows: oneRow("adopt", J{"refuse": "changed"}), body: idBody(nil), status: 409, code: "changed", msg: msgChanged},
		{name: "refuse before project", rows: oneRow("project_id", "p_gone", "adopt", J{"refuse": "changed"}), body: idBody(nil), status: 409, code: "changed", msg: msgChanged},
		{name: "unknown project", rows: oneRow("project_id", "p_gone", "adopt", J{"model_family": "haiku"}), body: idBody(nil), status: 409, code: "no_project", msg: msgNoProject},
		{name: "host project", rows: oneRow("project_id", "p_box", "adopt", J{"model_family": "haiku"}), body: idBody(nil), status: 409, code: "no_project", msg: msgNoProject},
		{name: "model unknown", rows: oneRow(), body: idBody(nil), status: 409, code: "model_unknown", msg: "the model of the session is unknown; send model"},
		{name: "busy before model", rows: oneRow("adopt", J{"busy": "status_stale"}), body: idBody(nil), status: 409, code: "busy", msg: busyMsg("status_stale")},
		{name: "model not allowed", rows: func(e adoptEnv) []J {
			return []J{adoptRow(1, e.narrow, "project_id", "p_narrow")}
		}, body: idBody(J{"model": "sonnet"}), status: 403, code: "model_not_allowed", msg: "model is not allowed for this project"},
		{name: "directory outside", rows: oneRow("directory", "/elsewhere/acme", "adopt", J{"model_family": "haiku"}), body: idBody(nil), status: 403, code: "directory_outside_project", msg: "requested directory is outside the project"},
		{name: "stop failed", rows: oneRow("adopt", J{"model_family": "haiku", "refuse": "stop_failed"}), body: idBody(nil), status: 502, code: "stop_failed", msg: "the process did not exit after SIGKILL; nothing was launched"},
	}
	for _, reason := range []string{"invalid_id", "start_mismatch", "not_interactive", "not_command_line", "ambiguous"} {
		cases = append(cases, adoptCase{name: "reason " + reason, rows: oneRow("adoptable", false, "adopt_reason", reason), body: idBody(nil), status: 409, code: reason, msg: "the session cannot be adopted: " + reason})
	}
	for _, veto := range []string{"registry_busy", "status_stale", "open_tool_call", "child_process"} {
		cases = append(cases, adoptCase{name: "busy " + veto, rows: oneRow("adopt", J{"busy": veto, "model_family": "haiku"}), body: idBody(nil), status: 409, code: "busy", msg: busyMsg(veto)})
	}
	for _, c := range cases {
		t.Run(c.name, c.run)
	}

	t.Run("managed", func(t *testing.T) {
		t.Parallel()
		e := startAdopt(t)
		row := adoptRow(1, e.dir, "adopt", J{"model_family": "haiku"})
		putRows(t, e.in, row)
		adoptCall(t, e.in, J{"outsideId": adoptID(1)}, "adopt-managed-0001").is(t, 201)
		putRows(t, e.in, row)
		got := adoptCall(t, e.in, J{"outsideId": adoptID(1)}, "adopt-managed-0002").is(t, 409).obj(t)
		eq[any](t, got["error"], "managed", "managed code")
		eq[any](t, got["message"], "relay already holds this conversation", "managed message")
		detail(t, e.in, adoptID(1)).is(t, 200)
		eq(t, len(sessionRows(t, e.in)), 1, "sessions after the second adopt")
		ev := adoptEvent(t, e.in, "adopt-managed-0002")
		eq[any](t, [2]any{ev["status"], ev["reason"]}, [2]any{"denied", "managed"}, "managed event")
	})

	t.Run("launch failed", func(t *testing.T) {
		t.Parallel()
		e := startAdopt(t)
		putRows(t, e.in, adoptRow(1, e.dir, "adopt", J{"model_family": "haiku", "refuse": "launch_failed"}))
		e.in.ctl(t, "POST", "/v1/clock", J{"set": "2030-01-02T03:10:00Z"}).is(t, 200)
		got := adoptCall(t, e.in, J{"outsideId": adoptID(1)}, "adopt-failed-0001").is(t, 502).obj(t)
		eq[any](t, got["error"], "launch_failed", "launch_failed code")
		eq[any](t, got["message"], "the process ended but relay could not start the session; the conversation is intact and can be resumed", "launch_failed message")
		row := detail(t, e.in, adoptID(1)).is(t, 200).obj(t)["session"].(map[string]any)
		eq[any](t, [3]any{row["state"], row["adoptable"], row["adoptReason"]}, [3]any{"ended", false, "ended"}, "row after launch_failed")
		eq[any](t, row["claudeSessionId"], adoptUUID(1), "claudeSessionId kept")
		if at, _ := row["endedAt"].(string); !strings.HasPrefix(at, "2030-01-02T03:10:") || strings.Contains(at, ".") {
			t.Errorf("endedAt %v, want the clock's now in RFC 3339 seconds", row["endedAt"])
		}
		eq(t, len(sessionRows(t, e.in)), 0, "sessions after launch_failed")
		ev := adoptEvent(t, e.in, "adopt-failed-0001")
		eq[any](t, [2]any{ev["status"], ev["reason"]}, [2]any{"error", "launch_failed"}, "launch_failed event")
	})
}

// Regression: the adopt door opens to the Chief of Staff scope, a read-only bearer or the TCP
// listener.
func TestAdoptDoorClass(t *testing.T) {
	t.Parallel()
	e := startAdopt(t)
	putRows(t, e.in, adoptRow(1, e.dir, "adopt", J{"model_family": "haiku"}))
	body := J{"outsideId": adoptID(1)}
	outsideScoped(e.in, t, "POST", adoptPath, body).is(t, http.StatusForbidden)
	e.in.apiAs(t, roTok, "POST", adoptPath, body).is(t, http.StatusForbidden)
	tcp, err := send(t.Context(), e.in.tcpClient(), "http://"+e.in.ready.Listeners.API, "POST", adoptPath, opsTok, body)
	must(t, err, "tcp adopt")
	tcp.is(t, http.StatusNotFound)
	detail(t, e.in, adoptID(1)).is(t, 200)
	eq(t, len(sessionRows(t, e.in)), 0, "sessions after the refused doors")
}

func unsandboxedWorld(string) any {
	w := adoptWorld("").(J)
	w["terminal_templates"] = []J{{"id": "claude-code", "name": "Claude", "command": "/bin/sh"}}
	return w
}

// Regression: a Claude row in no project is refused, adopted with a project, or answers without the
// unsandboxed mark; the mark leaks onto project sessions or terminals; a project that holds the folder,
// or an unusable folder, is not refused with relay's code.
func TestAdoptNoProjectUnsandboxed(t *testing.T) {
	t.Parallel()
	in := startInstance(t, unsandboxedWorld)
	dir := in.api(t, "GET", "/api/projects/p_acme", nil).is(t, 200).obj(t)["path"].(string)
	free := adoptRow(1, "/work/free-acme", "project_id", nil, "adopt", J{"model_family": "haiku"})
	inside := adoptRow(2, dir+"/sub", "project_id", nil, "adopt", J{"model_family": "haiku"})
	unusable := adoptRow(3, "/work/gone-acme", "project_id", nil, "adopt", J{"model_family": "haiku", "refuse": "directory_unusable"})
	member := adoptRow(4, dir, "adopt", J{"model_family": "haiku"})
	putRows(t, in, free, inside, unusable, member)

	for _, id := range []int{1, 2, 3} {
		row := detail(t, in, adoptID(id)).is(t, 200).obj(t)["session"].(map[string]any)
		eq[any](t, row["adoptable"], true, "adoptable")
		if r, has := row["adoptReason"]; has {
			t.Errorf("row %d carries adoptReason %v", id, r)
		}
	}

	changed := adoptCall(t, in, J{"outsideId": adoptID(2)}, "adopt-np-0001").is(t, 409).obj(t)
	eq[any](t, changed["error"], "changed", "changed code")
	gone := adoptCall(t, in, J{"outsideId": adoptID(3)}, "adopt-np-0002").is(t, 409).obj(t)
	eq[any](t, gone["error"], "directory_unusable", "directory_unusable code")
	eq(t, len(sessionRows(t, in)), 0, "sessions after the refusals")
	detail(t, in, adoptID(2)).is(t, 200)
	detail(t, in, adoptID(3)).is(t, 200)

	got := adoptCall(t, in, J{"outsideId": adoptID(1)}, "adopt-np-0003").is(t, 201).obj(t)
	eq[any](t, got["unsandboxed"], true, "adopt answer")
	proj := adoptCall(t, in, J{"outsideId": adoptID(4)}, "adopt-np-0004").is(t, 201).obj(t)
	if v, has := proj["unsandboxed"]; has {
		t.Errorf("project adopt answer carries unsandboxed %v", v)
	}

	marks := map[string]any{}
	for _, s := range sessionRows(t, in) {
		marks[s["id"].(string)] = s["unsandboxed"]
		if s["id"] == got["sessionId"] {
			eq[any](t, s["projectId"], "", "projectId")
		}
	}
	eq(t, marks, map[string]any{got["sessionId"].(string): true, proj["sessionId"].(string): nil}, "unsandboxed on GET /api/sessions")

	held := map[string]any{}
	for _, sid := range []any{got["sessionId"], proj["sessionId"]} {
		d := in.api(t, "POST", "/api/sessions/"+sid.(string)+"/drop-in", nil).is(t, 201).obj(t)
		held[d["terminal"].(map[string]any)["terminalId"].(string)] = sid
	}
	for _, v := range in.api(t, "GET", "/api/terminals", nil).is(t, 200).obj(t)["terminals"].([]any) {
		row := v.(map[string]any)
		sid, ok := held[row["id"].(string)]
		if !ok {
			continue
		}
		if sid == got["sessionId"] {
			eq[any](t, row["unsandboxed"], true, "unsandboxed drop-in terminal")
		} else if m, has := row["unsandboxed"]; has {
			t.Errorf("project drop-in terminal carries unsandboxed %v", m)
		}
		delete(held, row["id"].(string))
	}
	eq(t, len(held), 0, "drop-in terminals not listed")
}
