package prooftest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	historyList = "/api/session-history"
	historyConv = "00000000-0000-4000-8000-000000000001"
)

// historyRow is a wire-1 row of a dormant terminal that resumes through the history door.
func historyRow(id string, over J) J {
	row := J{"id": id, "record": "launched", "kind": "terminal", "agent": "claude", "projectId": "p1", "hostId": nil,
		"directory": "/work/acme", "conversationId": historyConv, "title": "Fix the login page", "titleSource": "client",
		"lastActiveAt": "2026-10-22T09:58:12Z", "createdAt": "2026-10-22T09:30:00Z", "state": "dormant",
		"transcript": "present", "resume": "history"}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func historyWorld(rows ...J) func(string) any {
	return func(string) any {
		return J{"schema": 1, "credentials": creds(),
			"projects": []J{
				{"id": "p1", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}, "allowed_templates": []string{"*"}},
				{"id": "p2", "name": "Box", "mode": "work", "allowed_models": []string{"*"}, "allowed_templates": []string{"*"}}},
			"terminal_templates": []J{{"id": "claude-code", "name": "Claude", "command": "/bin/sh"}},
			"sessionHistory":     rows}
	}
}

func historyRows(t *testing.T, in *instance, query string) (J, []J) {
	t.Helper()
	body := in.api(t, "GET", historyList+query, nil).is(t, http.StatusOK).obj(t)
	var rows []J
	for _, r := range body["rows"].([]any) {
		rows = append(rows, r.(J))
	}
	return body, rows
}

func historyIDs(rows []J) []string {
	var ids []string
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	return ids
}

// Regression: the list loses a row, ignores the project filter, drops a wire field, or answers
// before it is ready.
func TestHistoryListShapeAndFilter(t *testing.T) {
	t.Parallel()
	in := startInstance(t, historyWorld(
		historyRow("h1", nil),
		historyRow("h2", J{"projectId": "p2", "title": nil, "titleSource": nil, "lastActiveAt": nil}),
		historyRow("d-9", J{"record": "discovered", "state": "discovered", "projectId": nil})))

	body, rows := historyRows(t, in, "")
	eq[any](t, body["ready"], true, "ready")
	if _, isNumber := body["revision"].(float64); !isNumber {
		t.Fatalf("revision is %#v, want a number", body["revision"])
	}
	eq(t, len(rows), 3, "rows")
	var h1 J
	for _, r := range rows {
		if r["id"] == "h1" {
			h1 = r
		}
	}
	for k, want := range historyRow("h1", nil) {
		eq(t, h1[k], want, "h1."+k)
	}

	_, scoped := historyRows(t, in, "?projectId=p1")
	eq(t, historyIDs(scoped), []string{"h1"}, "rows of project p1")
	_, other := historyRows(t, in, "?projectId=p2")
	eq(t, historyIDs(other), []string{"h2"}, "rows of project p2")
	_, none := historyRows(t, in, "?projectId=p-unknown")
	eq(t, len(none), 0, "rows of an unknown project")
}

// Regression: a history row resumes into a terminal that is not listed or not attachable, keeps its
// row, or answers a body that is not the terminal body plus supersedes.
func TestHistoryResumeOpensTerminal(t *testing.T) {
	t.Parallel()
	in := startInstance(t, historyWorld(historyRow("h1", nil), historyRow("h2", nil)))

	created := in.api(t, "POST", historyList+"/h1/resume", J{"cols": 100, "rows": 30}).is(t, http.StatusCreated).obj(t)
	eq[any](t, created["supersedes"], "h1", "supersedes")
	eq[any](t, created["templateId"], "claude-code", "templateId")
	id, _ := created["terminalId"].(string)
	if id == "" {
		t.Fatalf("terminalId is %#v, want a terminal id", created["terminalId"])
	}
	for _, key := range []string{"name", "directory", "host"} {
		if _, has := created[key]; !has {
			t.Errorf("201 body has no %q: %v", key, created)
		}
	}

	_, rows := historyRows(t, in, "")
	eq(t, historyIDs(rows), []string{"h2"}, "rows after the resume")
	var listed []string
	for _, tm := range in.api(t, "GET", "/api/terminals", nil).is(t, http.StatusOK).obj(t)["terminals"].([]any) {
		listed = append(listed, tm.(J)["id"].(string))
	}
	eq(t, listed, []string{id}, "terminals after the resume")
	c := in.ws(t, "/ws", opsTok)
	must(t, c.WriteJSON(J{"type": "join_terminal", "terminalId": id}), "join the terminal")
	until(t, c, isType("terminal_joined", "terminalId", id))
	in.api(t, "POST", historyList+"/h1/resume", J{"cols": 100, "rows": 30}).is(t, http.StatusNotFound)
}

// Regression: the resume door answers another status or code than relay for a body, an unknown id,
// a row that cannot resume, or a row with a refuse knob.
func TestHistoryResumeRefusals(t *testing.T) {
	t.Parallel()
	in := startInstance(t, historyWorld(
		historyRow("agent", J{"kind": "claude", "resume": "session", "state": "dormant"}),
		historyRow("live", J{"state": "live", "resume": "none", "resumeReason": "live"}),
		historyRow("held", J{"resume": "none", "resumeReason": "held"}),
		historyRow("noconv", J{"conversationId": nil, "resume": "none", "resumeReason": "no_conversation"}),
		historyRow("noproj", J{"projectId": nil, "resume": "none", "resumeReason": "no_project"}),
		historyRow("absent", J{"transcript": "absent", "resume": "none", "resumeReason": "transcript_absent"}),
		historyRow("host", J{"hostId": "h_box", "resume": "none", "resumeReason": "host_unsupported"}),
		historyRow("knob", J{"refuse": "held"}),
		historyRow("knob-down", J{"refuse": "unavailable"}),
		historyRow("knob-first", J{"resume": "none", "resumeReason": "live", "refuse": "no_project"})))

	good := J{"cols": 100, "rows": 30}
	for _, c := range []struct {
		name   string
		id     string
		body   any
		status int
		code   string
	}{
		{"cols below range", "knob", J{"cols": 0, "rows": 30}, http.StatusBadRequest, "invalid_body"},
		{"cols above range", "knob", J{"cols": 1001, "rows": 30}, http.StatusBadRequest, "invalid_body"},
		{"rows below range", "knob", J{"cols": 100, "rows": 0}, http.StatusBadRequest, "invalid_body"},
		{"rows above range", "knob", J{"cols": 100, "rows": 1001}, http.StatusBadRequest, "invalid_body"},
		{"body before record", "no-such-row", J{"cols": 0, "rows": 0}, http.StatusBadRequest, "invalid_body"},
		{"unknown id", "no-such-row", good, http.StatusNotFound, "history_not_found"},
		{"agent record", "agent", good, http.StatusConflict, "use_session_resume"},
		{"live", "live", good, http.StatusConflict, "live"},
		{"held", "held", good, http.StatusConflict, "held"},
		{"no conversation", "noconv", good, http.StatusConflict, "no_conversation"},
		{"no project", "noproj", good, http.StatusConflict, "no_project"},
		{"transcript absent", "absent", good, http.StatusConflict, "transcript_absent"},
		{"host", "host", good, http.StatusConflict, "host_unsupported"},
		{"refuse knob", "knob", good, http.StatusConflict, "held"},
		{"refuse knob with a 502 code", "knob-down", good, http.StatusBadGateway, "unavailable"},
		{"refuse knob before the reason", "knob-first", good, http.StatusConflict, "no_project"},
	} {
		r := in.api(t, "POST", historyList+"/"+c.id+"/resume", c.body)
		if r.Status != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.name, r.Status, c.status, r.Body)
			continue
		}
		eq[any](t, r.obj(t)["code"], c.code, c.name+": code")
	}
	_, rows := historyRows(t, in, "")
	eq(t, len(rows), 10, "rows after the refusals")
	terminals := in.api(t, "GET", "/api/terminals", nil).is(t, http.StatusOK).obj(t)
	eq(t, len(terminals["terminals"].([]any)), 0, "terminals after the refusals")
}

// Regression: the history doors open to the Chief of Staff scope, a read-only bearer or the TCP
// listener.
func TestHistoryDoorClass(t *testing.T) {
	t.Parallel()
	in := startInstance(t, historyWorld(historyRow("h1", nil)))
	resume := historyList + "/h1/resume"
	body := J{"cols": 100, "rows": 30}

	for _, c := range []struct {
		method, path string
		body         any
	}{{"GET", historyList, nil}, {"POST", resume, body}} {
		method, path, body := c.method, c.path, c.body
		in.api(t, method, path, body, "X-Relay-Scope", "chief-of-staff").is(t, http.StatusForbidden)
		in.apiAs(t, roTok, method, path, body).is(t, http.StatusForbidden)
		tcp, err := send(t.Context(), in.tcpClient(), "http://"+in.ready.Listeners.API, method, path, opsTok, body)
		must(t, err, "tcp "+path)
		tcp.is(t, http.StatusNotFound)
	}
	_, rows := historyRows(t, in, "")
	eq(t, historyIDs(rows), []string{"h1"}, "rows after the refused doors")
}

// Regression: the world loads a row with a value outside the wire's enums, or fails without naming
// the row and key.
func TestHistoryWorldValidation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ key, value string }{
		{"record", "found"},
		{"kind", "pty"},
		{"state", "paused"},
		{"transcript", "missing"},
		{"resume", "sideways"},
		{"titleSource", "guess"},
		{"resumeReason", "because"},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			dir, err := newDir()
			must(t, err, "dir")
			over := J{c.key: c.value}
			if c.key == "resumeReason" {
				over["resume"] = "none"
			}
			world := historyWorld(historyRow("h1", nil), historyRow("h2", over))("")
			b, err := json.Marshal(world)
			must(t, err, "encode world")
			must(t, os.WriteFile(filepath.Join(dir, "world.json"), b, 0o600), "write world")
			out, se, code := serveUntilReadyOrExit(t, dir)
			want := "sessionHistory[1]." + c.key
			if code != 1 || out != "" || !strings.Contains(se, want) {
				t.Errorf("exit %d stdout %q stderr %q, want exit 1 naming %s", code, out, se, want)
			}
		})
	}
}

// Regression: the unavailable switch leaves one of the doors answering.
func TestHistoryUnavailable(t *testing.T) {
	t.Parallel()
	in := startInstance(t, func(string) any {
		w := historyWorld(historyRow("h1", nil))("").(J)
		w["sessionHistoryUnavailable"] = true
		return w
	})
	eq[any](t, in.api(t, "GET", historyList, nil).is(t, http.StatusServiceUnavailable).obj(t)["error"], "history_unavailable", "list error")
	eq[any](t, in.api(t, "POST", historyList+"/h1/resume", J{"cols": 100, "rows": 30}).is(t, http.StatusServiceUnavailable).obj(t)["code"],
		"history_unavailable", "resume code")
}

// Regression: deleting a session leaves its history row listed, or the delete removes another row.
func TestHistoryRowGoneWithDeletedSession(t *testing.T) {
	t.Parallel()
	in := startInstance(t, func(string) any {
		w := historyWorld(
			historyRow("s1", J{"kind": "claude", "resume": "session"}),
			historyRow("h2", nil))("").(J)
		w["sessions"] = []J{{"id": "s1", "project_id": "p1", "name": "Fix", "model": "sonnet", "state": "idle", "messages": []J{}}}
		return w
	})
	_, before := historyRows(t, in, "")
	eq(t, historyIDs(before), []string{"s1", "h2"}, "rows before the delete")

	in.api(t, "DELETE", "/api/sessions/s1", nil).is(t, http.StatusNoContent)

	_, rows := historyRows(t, in, "")
	eq(t, historyIDs(rows), []string{"h2"}, "rows after the delete")
}
