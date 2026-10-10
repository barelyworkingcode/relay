package prooftest

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func controlsWorld(_ string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{{"id": "p_acme", "name": "Acme", "mode": "work", "allowed_models": []string{"*"},
			"files": J{"notes.md": "old\n"}}},
		"models": []J{{"value": "plan/m", "label": "Plan", "group": "Claude", "provider": "claude",
			"reply": J{"kind": "plan", "text": "# Plan\n- step one\n"}}}}
}

// ctlJSON runs a ctl verb that must exit 0 and returns its one-line body.
func ctlJSON(t *testing.T, in *instance, args ...string) J {
	t.Helper()
	out, se, code := in.cli(t, append([]string{"ctl"}, args...)...)
	if code != 0 {
		t.Fatalf("ctl %v: exit %d: %s%s", args, code, out, se)
	}
	var o J
	must(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &o), "ctl body "+out)
	return o
}

// Criteria: fs-write, plan reply and watch-error controls.
func TestFakerelayControls(t *testing.T) {
	t.Parallel()

	t.Run("fs-write", func(t *testing.T) {
		in := startInstance(t, controlsWorld)
		wf := in.ws(t, "/ws/files", opsTok)
		must(t, wf.WriteJSON(J{"type": "watch", "project_id": "p_acme"}), "watch")
		in.waitEvent(t, "fakerelay.watch")
		until(t, wf, isType("watch_ok", "project_id", "p_acme"))

		got := ctlJSON(t, in, "fs-write", "--project", "p_acme", "--path", "notes.md", "--content", "new\n")
		eq(t, got, J{"path": "notes.md", "kind": "change", "delivered": float64(1)}, "fs-write body")
		fr := until(t, wf, isType("fs_event", "project_id", "p_acme", "path", "notes.md"))
		eq(t, fr[len(fr)-1]["kind"], any("change"), "fs_event kind")
		eq(t, in.api(t, "POST", "/api/projects/p_acme/files/read", J{"path": "notes.md"}).is(t, 200).obj(t)["content"], any("new\n"), "read after write")

		raw := []byte("a\x00b\x00\xff")
		src := filepath.Join(t.TempDir(), "blob.bin")
		must(t, os.WriteFile(src, raw, 0o600), "write source")
		got = ctlJSON(t, in, "fs-write", "--project", "p_acme", "--path", "blob.bin", "--file", src)
		eq(t, got["kind"], any("rename"), "new file kind")
		r := in.api(t, "GET", "/api/projects/p_acme/files/stream?path="+url.QueryEscape("blob.bin"), nil).is(t, 200)
		eq(t, string(r.Body), string(raw), "streamed bytes")

		_, se, code := in.cli(t, "ctl", "fs-write", "--project", "p_acme", "--path", "../x", "--content", "x")
		eq(t, code, 1, "traversal exit")
		if !strings.Contains(se, "Path traversal not allowed") {
			t.Errorf("traversal stderr %q", se)
		}
	})

	t.Run("plan", func(t *testing.T) {
		in := startInstance(t, controlsWorld)
		const text = "# Plan\n- step one\n"
		sid := in.api(t, "POST", "/api/sessions", J{"projectId": "p_acme", "model": "plan/m", "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
		w := in.ws(t, "/ws", opsTok)
		must(t, w.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
		until(t, w, isType("session_joined", "sessionId", sid))
		must(t, w.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "plan it"}), "send_message")
		fr := until(t, w, func(f J) bool { return f["type"] == "message_complete" && f["sessionId"] == sid })

		path, err := filepath.EvalSymlinks(filepath.Join(in.dir, "home", ".claude", "plans", sid+".md"))
		must(t, err, "plan file on disk")
		disk, err := os.ReadFile(path)
		must(t, err, "read plan file")
		eq(t, string(disk), text, "plan file content")

		var uses []J
		var result J
		for _, f := range fr {
			ev, _ := f["event"].(map[string]any)
			if f["type"] != "llm_event" || ev == nil {
				continue
			}
			if cb, ok := ev["content_block"].(map[string]any); ok && cb["type"] == "tool_use" && ev["content_block_stop"] == nil {
				uses = append(uses, cb)
			}
			if ev["type"] == "result" {
				result = ev
			}
		}
		if len(uses) != 2 {
			t.Fatalf("want two tool_use blocks, got %v", uses)
		}
		eq(t, uses[0]["name"], any("Write"), "first tool")
		in0 := uses[0]["input"].(map[string]any)
		eq(t, in0["file_path"], any(path), "Write file_path")
		eq(t, uses[1]["name"], any("ExitPlanMode"), "second tool")
		eq(t, uses[1]["input"].(map[string]any)["plan"], any(text), "ExitPlanMode plan")
		eq(t, result["tool_use_id"], uses[0]["id"], "tool_result pairs with Write")
		last := fr[len(fr)-1]
		eq(t, last["isError"], nil, "message_complete isError")
	})

	t.Run("watch-error", func(t *testing.T) {
		in := startInstance(t, controlsWorld)
		wf := in.ws(t, "/ws/files", opsTok)
		rewatch := func() {
			must(t, wf.WriteJSON(J{"type": "watch", "project_id": "p_acme"}), "watch")
			until(t, wf, isType("watch_ok", "project_id", "p_acme"))
		}
		rewatch()

		eq(t, ctlJSON(t, in, "watch-error", "--project", "p_acme"), J{"delivered": float64(1)}, "watch-error body")
		fr := until(t, wf, isType("watch_error", "project_id", "p_acme"))
		last := fr[len(fr)-1]
		eq(t, [2]any{last["code"], last["error"]}, [2]any{"ERROR", "watch failed"}, "default code and error")

		eq(t, ctlJSON(t, in, "fs-event", "--project", "p_acme", "--path", "notes.md", "--kind", "change")["delivered"], any(float64(0)), "fs-event after lost watch")

		rewatch()
		ctlJSON(t, in, "watch-error", "--project", "p_acme", "--code", "ENOENT", "--error", "No such file or directory")
		fr = until(t, wf, isType("watch_error", "project_id", "p_acme"))
		last = fr[len(fr)-1]
		eq(t, [2]any{last["code"], last["error"]}, [2]any{"ENOENT", "No such file or directory"}, "given code and error")

		_, _, code := in.cli(t, "ctl", "watch-error", "--project", "p_acme", "--code", "BOGUS")
		eq(t, code, 1, "bogus code exit")
	})
}
