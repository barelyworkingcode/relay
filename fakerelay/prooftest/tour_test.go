package prooftest

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	instances   = 24
	sessDormant = "0b9f4c1e-6d3a-4f60-9a57-2c1d8e7b4a10"
)

func tourWorld(dir string, i int) J {
	all := []string{"*"}
	return J{"schema": 1, "credentials": creds(),
		"presence": J{"project.grant": "approve", "eve.enrolment.open": "approve", "eve.passkey.revoke": "approve"},
		"projects": []J{
			{"id": "p_main", "name": fmt.Sprintf("Acme %02d", i), "mode": "work", "allowed_mcp_ids": []string{"fsmcp"},
				"allowed_models": all, "allowed_templates": all,
				"repos": []J{{"dir": "", "branch": "main", "commits": []J{{"message": "initial", "files": J{"README.md": "# Acme\n"}}}}},
				"files": J{"README.md": "# Acme changed\n", "src/": nil, "src/a.js": "// TODO x\n", "link": J{"symlink": "README.md"}}},
			{"id": "p_box", "name": fmt.Sprintf("Box %02d", i), "mode": "work", "host_id": "h_box", "path": "/home/acme/app",
				"allowed_models": all, "files": J{"main.go": "package main\n"}},
		},
		"hosts": []J{{"id": "h_box", "name": "testbox", "target": "acme@testbox", "agent": "connected",
			"probe":              J{"ok": true, "os": "Linux", "arch": "x86_64", "home": "/home/acme", "shell": "/bin/bash"},
			"terminal_templates": []J{{"id": "shell", "name": "Shell"}}}},
		"models": []J{
			{"value": "claude-haiku-5-5", "label": "Haiku", "group": "Claude", "provider": "claude"},
			{"value": "pi/perm", "label": "Perm", "group": "Pi", "provider": "pi", "reply": J{"kind": "permission", "tool": "Bash"}}},
		"mcps":               []J{{"id": "fsmcp", "name": "fsMCP", "transport": "stdio", "catalogue": J{"tools": []J{{"name": "fs_read", "inputSchema": J{"type": "object"}}}}}},
		"terminal_templates": []J{{"id": "shell", "name": "Shell", "command": "/bin/sh"}},
		"sessions": []J{{"id": sessDormant, "project_id": "p_main", "name": "Old", "model": "claude-haiku-5-5", "state": "dormant",
			"messages": []J{{"role": "user", "text": "hi"}}}},
		"services": []J{service(dir, "probe", "normal", "probe", "frontend", "manifest")},
	}
}

// Criteria: many instances in parallel with no shared file, socket or port;
// the ready file lists every bound address; the client surface works end to end.
func TestManyInstancesAtOnce(t *testing.T) {
	t.Parallel()
	insts := make([]*instance, instances)
	errs := make([]error, instances)
	var wg sync.WaitGroup
	for i := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := newDir()
			if err != nil {
				errs[i] = err
				return
			}
			insts[i], errs[i] = launch(dir, tourWorld(dir, i))
			if insts[i] != nil {
				insts[i].idx = i
			}
		}()
	}
	barrier := make(chan struct{})
	go func() { wg.Wait(); close(barrier) }()
	select {
	case <-barrier:
	case <-time.After(waitBound):
		t.Error("not all instances became ready before the deadline")
	}
	<-barrier // launch bounds every wait itself
	for _, in := range insts {
		if in != nil {
			t.Cleanup(func() { in.stop(t) })
		}
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d: %v", i, err)
		}
	}
	seen := map[string]int{}
	for i, in := range insts {
		for _, v := range []string{in.dir, in.ready.Listeners.API, in.ready.Sockets.Bridge, in.ready.Sockets.Frontend, in.ready.Sockets.Control} {
			if j, dup := seen[v]; dup {
				t.Errorf("instances %d and %d share %s", j, i, v)
			}
			seen[v] = i
		}
		if len(in.ready.Sockets.Frontend) > 103 {
			t.Errorf("socket path over 103 bytes: %s", in.ready.Sockets.Frontend)
		}
	}
	for i, in := range insts {
		t.Run(fmt.Sprintf("tour%02d", i), func(t *testing.T) {
			t.Parallel()
			tour(t, in)
		})
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func deltaText(frames []J) string {
	var sb strings.Builder
	for _, f := range frames {
		if ev, _ := f["event"].(J); f["type"] == "llm_event" && ev != nil {
			if d, _ := ev["delta"].(J); d != nil {
				sb.WriteString(fmt.Sprint(d["text"]))
			}
		}
	}
	return sb.String()
}

func tour(t *testing.T, in *instance) {
	i := in.idx
	trace := func(k string) string { return fmt.Sprintf("trc-%s-%02d", k, i) }
	own := []string{fmt.Sprintf("Acme %02d", i), fmt.Sprintf("Box %02d", i)}

	// Dispatch: the probe's manifest, its identity request and the stripped bearer.
	in.waitRegistered(t)
	rep := in.report(t, "probe", "X-Trace-Id", trace("probe"))
	if !rep.HelloOK || !rep.AuthOK || rep.ProjectsStatus != 200 {
		t.Fatalf("probe report: %+v", rep)
	}
	eq(t, rep.Trace, trace("probe"), "trace forwarded to the service")
	eq(t, rep.ProjectNames, own, "projects the probe sees by identity")
	pc, _, err := in.dialWS("/ws/probe", opsTok)
	must(t, err, "dial /ws/probe")
	must(t, pc.WriteMessage(1, []byte("ping")), "write")
	_, msg, err := pc.ReadMessage()
	must(t, err, "read")
	eq(t, string(msg), "probe:ping", "ws dispatch echo")
	pc.Close()

	// Doors: TCP is bearer only and carries no execute route.
	tcp := func(method, path, tok string) res {
		r, err := send(t.Context(), in.tcpClient(), "http://"+in.ready.Listeners.API, method, path, tok, nil)
		must(t, err, "tcp "+path)
		return r
	}
	tcp("GET", "/api/projects", "").is(t, 401)
	var names []string
	for _, p := range tcp("GET", "/api/projects", roTok).is(t, 200).arr(t) {
		names = append(names, p["name"].(string))
	}
	sort.Strings(names)
	eq(t, names, own, "projects over TCP")
	tcp("POST", "/api/projects/p_main/files/list", opsTok).is(t, 404)
	in.apiAs(t, roTok, "POST", "/api/projects/p_main/files/list", J{"path": ""}).is(t, 403)

	// Projects CRUD, the create event by trace, and grant --json.
	chgA, chgB := filepath.Join(in.dir, "chgA"), filepath.Join(in.dir, "chgB")
	must(t, os.MkdirAll(chgA, 0o755), "mkdir")
	must(t, os.MkdirAll(chgB, 0o755), "mkdir")
	created := in.api(t, "POST", "/api/projects", J{"name": fmt.Sprintf("Chg %02d", i), "path": chgA, "mode": "work"},
		"X-Trace-Id", trace("create")).is(t, 201).obj(t)
	cid := created["id"].(string)
	ev := in.events(t, trace("create"), "project.create")
	if len(ev) != 1 || ev[0]["project_id"] != cid || ev[0]["status"] != "ok" {
		t.Fatalf("project.create events: %v", ev)
	}
	out, se, code := in.cli(t, "grant", "--json")
	eq(t, code, 0, "grant --json exit: "+se)
	granted := map[any]bool{}
	for _, g := range parseRows(t, out) {
		granted[g["id"]] = true
	}
	if !granted["p_main"] || !granted[cid] {
		t.Fatalf("grant --json lacks projects: %s", out)
	}
	in.api(t, "PUT", "/api/projects/"+cid, J{"name": "Renamed"}).is(t, 200)
	eq(t, in.api(t, "GET", "/api/projects/"+cid, nil).is(t, 200).obj(t)["name"], any("Renamed"), "renamed project")

	// /ws/files: host_status on upgrade, watch, PROJECT_CHANGED on a path change and on delete.
	wf := in.ws(t, "/ws/files", opsTok)
	until(t, wf, isType("host_status", "host_id", "h_box", "status", "connected"))
	watch := func(id string) { must(t, wf.WriteJSON(J{"type": "watch", "project_id": id}), "watch") }
	watch("p_main")
	until(t, wf, isType("watch_ok", "project_id", "p_main"))
	watch(cid)
	until(t, wf, isType("watch_ok", "project_id", cid))
	in.api(t, "PUT", "/api/projects/"+cid, J{"path": chgB}).is(t, 200)
	until(t, wf, isType("watch_error", "project_id", cid, "code", "PROJECT_CHANGED"))
	watch(cid)
	until(t, wf, isType("watch_ok", "project_id", cid))
	in.api(t, "DELETE", "/api/projects/"+cid, nil).is(t, 204)
	until(t, wf, isType("watch_error", "project_id", cid, "code", "PROJECT_CHANGED"))
	in.api(t, "GET", "/api/projects/"+cid, nil).is(t, 404)

	// File plane on the console project.
	fop := func(op string, body any) res { return in.api(t, "POST", "/api/projects/p_main/files/"+op, body) }
	entries := map[any]any{}
	for _, e := range fop("list", J{"path": "", "show_hidden": true}).is(t, 200).obj(t)["entries"].([]any) {
		entries[e.(J)["name"]] = e.(J)["type"]
	}
	eq(t, entries["README.md"], any("file"), "list README.md")
	eq(t, entries["src"], any("directory"), "list src")
	eq(t, entries["link"], any("symlink"), "list link")
	eq(t, fop("stat", J{"path": "src/a.js"}).is(t, 200).obj(t)["size"], any(10.0), "stat size")
	eq(t, fop("read", J{"path": "README.md"}).is(t, 200).obj(t)["content"], any("# Acme changed\n"), "read")
	rng := in.api(t, "GET", "/api/projects/p_main/files/stream?path=README.md", nil, "Range", "bytes=0-3").is(t, 206)
	eq(t, string(rng.Body), "# Ac", "ranged stream")
	fop("write", J{"path": "src/new.txt", "content": "hello", "encoding": "utf8"}).is(t, 200)
	until(t, wf, isType("fs_event", "project_id", "p_main", "path", "src/new.txt"))
	fop("write", J{"path": "src/new.txt", "content": "again", "create_only": true}).is(t, 409)
	eq(t, fop("mkdir", J{"parent": "src", "name": "lib"}).is(t, 200).obj(t)["path"], any("src/lib"), "mkdir")
	eq(t, fop("rename", J{"path": "src/new.txt", "new_name": "r.txt"}).is(t, 200).obj(t)["path"], any("src/r.txt"), "rename")
	eq(t, fop("move", J{"path": "src/r.txt", "dest_dir": "src/lib"}).is(t, 200).obj(t)["path"], any("src/lib/r.txt"), "move")
	m := fop("search", J{"query": "TODO"}).is(t, 200).obj(t)["matches"].([]any)
	if len(m) != 1 || m[0].(J)["path"] != "src/a.js" || m[0].(J)["line"] != 1.0 || m[0].(J)["col"] != 4.0 || m[0].(J)["len"] != 4.0 {
		t.Fatalf("search matches: %v", m)
	}
	g := fop("git", J{"cwd": "", "args": []string{"status", "--porcelain=v2"}}).is(t, 200).obj(t)
	raw, _ := base64.StdEncoding.DecodeString(fmt.Sprint(g["stdout_b64"]))
	if g["exit_code"] != 0.0 || !strings.Contains(string(raw), "README.md") {
		t.Fatalf("git status: %v %q", g, raw)
	}
	eq(t, fop("git", J{"cwd": "", "args": []string{"push"}}).is(t, 400).obj(t)["code"], any("INVALID"), "git allowlist")
	eq(t, fop("delete", J{"path": "src/lib/r.txt"}).is(t, 200).obj(t)["trashed"], any(true), "console delete")
	if trashed, _ := os.ReadDir(filepath.Join(in.dir, "trash")); len(trashed) == 0 {
		t.Error("console delete left nothing in DIR/trash")
	}
	_, _, code = in.cli(t, "ctl", "fs-event", "--project", "p_main", "--path", "src/ctl.txt", "--kind", "change")
	eq(t, code, 0, "ctl fs-event exit")
	until(t, wf, isType("fs_event", "project_id", "p_main", "path", "src/ctl.txt"))
	for _, c := range []struct {
		path string
		code string
	}{{"../outside", "TRAVERSAL"}, {"link", "SYMLINK"}} {
		eq(t, fop("read", J{"path": c.path}).is(t, 403).obj(t)["code"], any(c.code), "read "+c.path)
		eq(t, fop("write", J{"path": c.path, "content": "x"}).is(t, 403).obj(t)["code"], any(c.code), "write "+c.path)
	}
	_, _, code = in.cli(t, "project", "update", "--id", "p_main", "--files-read-only=true")
	eq(t, code, 0, "project update exit")
	eq(t, fop("write", J{"path": "ro.txt", "content": "x"}).is(t, 403).obj(t)["code"], any("READ_ONLY"), "read-only write")
	out, _, _ = in.cli(t, "audit", "--json", "--event", "file_op", "--project", "p_main")
	type key struct{ tool, outcome, errCode any }
	have := map[key]bool{}
	for _, r := range parseRows(t, out) {
		have[key{r["tool"], r["outcome"], r["error"]}] = true
	}
	// Reads write no file_op row, refused or not (doc, File audit rows).
	for k := range have {
		if k.tool == "read" {
			t.Errorf("a read wrote a file_op row: %v", k)
		}
	}
	for _, k := range []key{{"write", "ok", nil}, {"write", "denied", "TRAVERSAL"}, {"write", "denied", "SYMLINK"}, {"write", "denied", "READ_ONLY"}} {
		if !have[k] {
			t.Errorf("audit lacks file_op row %v; have %v", k, have)
		}
	}

	// Host project: host_status through each state, 503 while not connected, pastetmp, permanent delete.
	hostStatus := func(status string, extra ...string) {
		_, se, code := in.cli(t, append([]string{"ctl", "host", "status", "--id", "h_box", "--status", status}, extra...)...)
		eq(t, code, 0, "ctl host status exit: "+se)
		until(t, wf, isType("host_status", "host_id", "h_box", "status", status))
	}
	hop := func(op string, body any) res { return in.api(t, "POST", "/api/projects/p_box/files/"+op, body) }
	hostStatus("connecting")
	hostStatus("unreachable", "--error", "offline")
	eq(t, hop("list", J{"path": ""}).is(t, 503).obj(t)["code"], any("HOST_UNREACHABLE"), "file op while unreachable")
	watch("p_box")
	until(t, wf, isType("watch_error", "project_id", "p_box", "code", "HOST_UNREACHABLE"))
	hostStatus("connected")
	eq(t, hop("list", J{"path": ""}).is(t, 200).obj(t)["entries"].([]any)[0].(J)["name"], any("main.go"), "host list")
	hop("write", J{"path": "tmp.txt", "content": "x"}).is(t, 200)
	eq(t, hop("delete", J{"path": "tmp.txt"}).is(t, 200).obj(t)["trashed"], any(false), "host delete is permanent")
	paste := "eve-paste-1789000000000-ab12cd34.png"
	in.api(t, "POST", "/api/hosts/h_box/pastetmp", J{"name": paste, "data_b64": b64("png")}).is(t, 200)

	// Models and sessions: the echo turn, attention, the permission script, resume_required.
	var models []string
	for _, r := range in.api(t, "GET", "/api/models", nil).is(t, 200).obj(t)["models"].([]any) {
		models = append(models, r.(J)["value"].(string))
	}
	for _, want := range []string{"haiku", "claude-haiku-5-5", "pi/perm"} {
		if !contains(models, want) {
			t.Errorf("models %v lack %s", models, want)
		}
	}
	w := in.ws(t, "/ws", opsTok)
	newSession := func(model string) string {
		return in.api(t, "POST", "/api/sessions", J{"projectId": "p_main", "model": model, "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
	}
	join := func(sid string) J {
		must(t, w.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
		f := until(t, w, isType("session_joined", "sessionId", sid))
		return f[len(f)-1]
	}
	say := func(sid, text, tr string) {
		must(t, w.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": text, "trace_id": tr}), "send_message")
	}
	turnEnd := func(sid string) []J {
		return until(t, w, func(f J) bool { return f["type"] == "message_complete" && f["sessionId"] == sid })
	}
	sid := newSession("haiku")
	eq(t, join(sid)["live"], any(true), "joined live")
	say(sid, fmt.Sprintf("hello-%02d", i), trace("turn"))
	fr := turnEnd(sid)
	at := func(from int, typ, state string) int {
		for k := from; k < len(fr); k++ {
			if fr[k]["type"] == typ && (state == "" || fr[k]["state"] == state) {
				return k
			}
		}
		return -1
	}
	order := []int{at(0, "user_message", "")}
	for _, s := range [][2]string{{"session_state", "running"}, {"llm_event", ""}, {"stats_update", ""}, {"turn_done", ""}} {
		order = append(order, at(order[len(order)-1]+1, s[0], s[1]))
	}
	order = append(order, at(order[len(order)-1]+1, "session_state", "idle"))
	for _, k := range order {
		if k < 0 {
			t.Fatalf("turn frames out of order: %v", fr)
		}
	}
	eq(t, order[len(order)-1] < len(fr)-1, true, "message_complete is the last frame")
	eq(t, fr[order[4]]["excerpt"], any(fmt.Sprintf("echo: hello-%02d", i)), "turn_done excerpt")
	eq(t, deltaText(fr), fmt.Sprintf("echo: hello-%02d", i), "echoed text")
	if ev := in.events(t, trace("turn"), "chat.turn"); len(ev) != 1 || ev[0]["session_id"] != sid {
		t.Errorf("chat.turn events by trace: %v", ev)
	}
	var attention J
	for _, s := range in.api(t, "GET", "/api/sessions", nil).is(t, 200).obj(t)["sessions"].([]any) {
		if s.(J)["id"] == sid {
			attention, _ = s.(J)["attention"].(J)
		}
	}
	idle := fr[order[5]]
	if attention == nil || attention["state"] != "idle" || attention["since"] != idle["since"] {
		t.Errorf("attention %v does not match the last session_state %v", attention, idle)
	}
	psid := newSession("pi/perm")
	join(psid)
	say(psid, "run it", trace("perm"))
	fr = until(t, w, isType("permission_request", "sessionId", psid))
	last := fr[len(fr)-1]
	eq(t, last["toolName"], any("Bash"), "permission tool")
	must(t, w.WriteJSON(J{"type": "permission_response", "permissionId": last["permissionId"], "approved": true}), "permission_response")
	fr = turnEnd(psid)
	if !strings.Contains(deltaText(fr), "echo: run it") {
		t.Errorf("approved turn did not echo: %v", fr)
	}
	eq(t, join(sessDormant)["live"], any(false), "dormant session joined")
	say(sessDormant, "wake", trace("wake"))
	until(t, w, isType("error", "code", "resume_required"))
	eq(t, in.api(t, "POST", "/api/sessions/"+sessDormant+"/resume", nil).is(t, 200).obj(t)["resumed"], any(true), "resumed")
	say(sessDormant, "wake", trace("wake"))
	turnEnd(sessDormant)

	// Terminals: echo, list, delete broadcast; templates.
	term := in.api(t, "POST", "/api/terminals", J{"templateId": "shell", "projectId": "p_main", "name": "T"}).is(t, 201).obj(t)["terminalId"].(string)
	must(t, w.WriteJSON(J{"type": "join_terminal", "terminalId": term}), "join_terminal")
	until(t, w, isType("terminal_joined", "terminalId", term))
	must(t, w.WriteJSON(J{"type": "terminal_input", "terminalId": term, "data": b64("ab\r")}), "terminal_input")
	var echoed strings.Builder
	until(t, w, func(f J) bool {
		if f["type"] == "terminal_output" && f["terminalId"] == term {
			d, _ := base64.StdEncoding.DecodeString(fmt.Sprint(f["data"]))
			echoed.Write(d)
		}
		return strings.Contains(echoed.String(), "ab\r\n")
	})
	if !strings.Contains(string(in.api(t, "GET", "/api/terminals", nil).is(t, 200).Body), term) {
		t.Error("terminal missing from the list")
	}
	in.api(t, "DELETE", "/api/terminals/"+term, nil).is(t, 204)
	until(t, w, isType("terminal_closed", "terminalId", term))
	tmpl := J{"id": "t2", "name": "Two", "command": "/bin/sh"}
	in.api(t, "POST", "/api/terminal/templates", tmpl).is(t, 201)
	in.api(t, "GET", "/api/terminal/templates/t2", nil).is(t, 200)
	in.api(t, "DELETE", "/api/terminal/templates/t2", nil).is(t, 204)
	in.api(t, "GET", "/api/terminal/templates/t2", nil).is(t, 404)
	if !strings.Contains(string(in.api(t, "GET", "/api/terminal/templates?project=p_main", nil).is(t, 200).Body), `"shell"`) {
		t.Error("project templates lack shell")
	}
	if !strings.Contains(string(in.api(t, "GET", "/api/hosts/h_box/templates", nil).is(t, 200).Body), `"shell"`) {
		t.Error("host templates lack shell")
	}

	// Chief of Staff scope.
	cos := "X-Relay-Scope"
	in.api(t, "GET", "/api/sessions", nil, cos, "chief-of-staff").is(t, 200)
	if r := in.api(t, "GET", "/api/projects", nil, cos, "chief-of-staff").is(t, 403); strings.TrimSpace(string(r.Body)) != "Forbidden" {
		t.Errorf("scope refusal body %q", r.Body)
	}
	in.apiAs(t, roTok, "GET", "/api/sessions", nil, cos, "chief-of-staff").is(t, 403)
	in.api(t, "GET", "/api/sessions", nil, cos, "other").is(t, 403)
	in.api(t, "POST", "/api/chief-of-staff/messages", J{"sessionId": sid, "text": "from cos"}).is(t, 403)
	in.api(t, "POST", "/api/chief-of-staff/messages", J{"sessionId": sid, "text": "from cos"}, cos, "chief-of-staff").is(t, 202)
	sc := in.ws(t, "/ws", opsTok, cos, "chief-of-staff")
	must(t, sc.WriteJSON(J{"type": "join_session", "sessionId": sid}), "scoped write")
	// A scoped /ws also receives hub broadcasts (doc, Scope), so frames may
	// arrive before the close; the close frame is the signal.
	_ = sc.SetReadDeadline(time.Now().Add(waitBound))
	var scopedErr error
	for scopedErr == nil {
		_, _, scopedErr = sc.ReadMessage()
	}
	if !websocketClosed(scopedErr, 1008, "chief-of-staff scope is read-only") {
		t.Errorf("scoped /ws send: %v", scopedErr)
	}

	// Eve window and passkeys.
	_, se, code = in.cli(t, "eve", "enrol")
	eq(t, code, 0, "eve enrol exit: "+se)
	eq(t, in.api(t, "GET", "/api/eve/passkey-enrolment", nil).is(t, 200).obj(t)["open"], any(true), "window open")
	in.api(t, "POST", "/api/eve/passkey-enrolment/consume", J{"ip": "203.0.113.9", "label": "Phone"}).is(t, 200)
	in.api(t, "POST", "/api/eve/passkey-enrolment/consume", J{"ip": "203.0.113.9", "label": "Phone"}).is(t, 409)
	eq(t, in.api(t, "GET", "/api/eve/passkey-enrolment", nil).is(t, 200).obj(t)["open"], any(false), "window closed")
	in.api(t, "PUT", "/api/eve/passkeys", J{"passkeys": []J{
		{"id": "pk1", "label": "Phone", "created": "2026-10-09T10:00:00Z", "last_used": "2026-10-09T10:00:00Z"},
		{"id": "pk2", "label": "Laptop", "created": "2026-10-09T10:00:00Z", "last_used": "2026-10-09T10:00:00Z"},
	}}).is(t, 200)
	out, _, code = in.cli(t, "eve", "list")
	if code != 0 || !strings.Contains(out, "pk1") {
		t.Errorf("eve list: exit %d %q", code, out)
	}
	_, se, code = in.cli(t, "eve", "revoke", "--id", "pk1")
	eq(t, code, 0, "eve revoke exit: "+se)
	eq(t, in.api(t, "GET", "/api/eve/passkeys/revocations", nil).is(t, 200).obj(t)["revocations"], any([]any{"pk1"}), "revocations")

	// MCPs.
	eq(t, in.api(t, "GET", "/api/mcps", nil).is(t, 200).arr(t), []J{{"id": "fsmcp", "display_name": "fsMCP"}}, "mcp list")
	tools := in.api(t, "GET", "/api/mcps/fsmcp/tools", nil).is(t, 200).arr(t)
	if len(tools) != 1 || tools[0]["name"] != "fs_read" || tools[0]["category"] != "Fs" || tools[0]["inputSchema"] != nil {
		t.Errorf("tools: %v", tools)
	}
	in.api(t, "GET", "/api/mcps/nope/tools", nil).is(t, 404)
	out, _, code = in.cli(t, "mcp", "list")
	if code != 0 || !strings.Contains(out, "fsmcp") {
		t.Errorf("mcp list: exit %d %q", code, out)
	}

	// Logs and state.
	_, _, code = in.cliTrace(t, "no-such-trace-id", "logs", "--event", "project.remove")
	eq(t, code, 1, "logs with no match exits 1")
	_, _, code = in.cli(t, "logs", "--event", "project.remove")
	eq(t, code, 0, "the same event without a trace filter matches")
	out, se, code = in.cli(t, "ctl", "state", "--json")
	if code != 0 || !strings.Contains(out, "p_main") {
		t.Errorf("ctl state: exit %d %q %s", code, out, se)
	}
	eq(t, in.report(t, "probe").PID, rep.PID, "same probe throughout")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
