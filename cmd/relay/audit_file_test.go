package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// fileOpRows reads the file_op rows the recorder has written. With close, the
// recorder is closed first, which drains its queue so completion rows are in
// the file; without it only what is already durable is read.
func fileOpRows(t *testing.T, env *fileEnv, close bool) []audit.AuditEvent {
	t.Helper()
	if close {
		env.rec.Close()
	}
	raw, err := os.ReadFile(env.rec.Path())
	if os.IsNotExist(err) {
		return nil
	}
	assertNoErr(t, err, "read audit log")
	var out []audit.AuditEvent
	for _, ev := range aiParse(t, string(raw)) {
		if ev.Event == audit.AuditEventFileOp {
			out = append(out, ev)
		}
	}
	return out
}

func fileRowArgs(t *testing.T, ev audit.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	assertNoErr(t, json.Unmarshal(ev.Args, &m), "decode args %s", ev.Args)
	return m
}

func TestFileAudit_IntentRowIsDurableWhenTheRouteAnswers(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	r := env.op(t, "write", map[string]any{"path": "a.md", "content": "hi"})
	if r.status != 200 {
		t.Fatalf("write = %d %s", r.status, r.raw)
	}

	// No Close: the intent row must already be in the file.
	var intents []audit.AuditEvent
	for _, ev := range fileOpRows(t, env, false) {
		if ev.Phase == audit.AuditPhaseIntent {
			intents = append(intents, ev)
		}
	}
	if len(intents) != 1 {
		t.Fatalf("intent rows = %d, want 1", len(intents))
	}
	if in := intents[0]; in.Tool != "write" || in.Outcome != audit.AuditOutcomePending {
		t.Errorf("intent = tool %q outcome %q", in.Tool, in.Outcome)
	}
}

func TestFileAudit_RowsCarryProjectPathAndArgsPerOp(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "notes/keep.md", "k")
	trashName := "relay-audit-" + filepath.Base(env.root) + ".txt"
	env.put(t, "notes/"+trashName, "trash-me")

	steps := []struct {
		op   string
		body map[string]any
		tool string
		want map[string]any
	}{
		{"write", map[string]any{"path": "/notes/a.md", "content": "hi", "create_only": true},
			"write", map[string]any{"path": "notes/a.md", "bytes": 2.0, "encoding": "utf8", "create_only": true}},
		{"mkdir", map[string]any{"parent": "notes", "name": "sub"},
			"mkdir", map[string]any{"path": "notes/sub"}},
		{"rename", map[string]any{"path": "notes/a.md", "new_name": "b.md"},
			"rename", map[string]any{"path": "notes/a.md", "new_path": "notes/b.md"}},
		{"move", map[string]any{"path": "notes/b.md", "dest_dir": "notes/sub"},
			"move", map[string]any{"path": "notes/b.md", "new_path": "notes/sub/b.md"}},
		{"delete", map[string]any{"path": "notes/" + trashName},
			"delete", map[string]any{"path": "notes/" + trashName, "trashed": true}},
		// A failing mutation still has an intent and an error completion.
		{"write", map[string]any{"path": "notes/keep.md", "content": "x", "create_only": true},
			"write", map[string]any{"path": "notes/keep.md", "bytes": 1.0, "encoding": "utf8", "create_only": true}},
	}
	for _, s := range steps {
		r := env.op(t, s.op, s.body)
		if s.op == "delete" {
			cleanTrash(t, trashName, "trash-me")
			if r.status == 403 && r.code(t) == projectfs.CodeEACCES {
				t.Skipf("Trash is not writable here: %s", r.raw)
			}
		}
	}

	rows := fileOpRows(t, env, true)
	if len(rows) != 2*len(steps) {
		t.Fatalf("file_op rows = %d, want %d (intent + completion each)", len(rows), 2*len(steps))
	}
	for i, s := range steps {
		intent, done := rows[2*i], rows[2*i+1]
		for _, row := range []audit.AuditEvent{intent, done} {
			if row.Tool != s.tool {
				t.Errorf("step %d: tool %q, want %q", i, row.Tool, s.tool)
			}
			if row.Actor.ProjectID != env.proj.ID || row.Actor.ProjectName != "Acme" || row.McpRoot != env.proj.Path {
				t.Errorf("step %d: project %q %q root %q, want %q Acme %q", i, row.Actor.ProjectID, row.Actor.ProjectName, row.McpRoot, env.proj.ID, env.proj.Path)
			}
			args := fileRowArgs(t, row)
			for k, want := range s.want {
				if args[k] != want {
					t.Errorf("step %d (%s): args[%s] = %v, want %v", i, s.tool, k, args[k], want)
				}
			}
		}
		if intent.Phase != audit.AuditPhaseIntent || intent.Outcome != audit.AuditOutcomePending ||
			done.Phase != audit.AuditPhaseCompletion || intent.ID == "" || intent.ID != done.ID {
			t.Errorf("step %d: intent %q/%q id %q, completion %q id %q", i, intent.Phase, intent.Outcome, intent.ID, done.Phase, done.ID)
		}
		if i < len(steps)-1 && done.Outcome != audit.AuditOutcomeOK {
			t.Errorf("step %d: completion outcome %q, want ok", i, done.Outcome)
		}
	}
	last := rows[len(rows)-1]
	if last.Outcome != audit.AuditOutcomeError || last.Error != projectfs.CodeEEXIST {
		t.Errorf("failed write completion = %q error %q, want error EEXIST", last.Outcome, last.Error)
	}
}

func TestFileAudit_RefusedMutationWritesOneDeniedRow(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.links(t)

	mutations := func(traversal, symlink bool) []struct {
		op   string
		body map[string]any
	} {
		switch {
		case traversal:
			return []struct {
				op   string
				body map[string]any
			}{
				{"write", map[string]any{"path": "../x", "content": "w"}},
				{"mkdir", map[string]any{"parent": "..", "name": "x"}},
				{"rename", map[string]any{"path": "../x", "new_name": "y"}},
				{"move", map[string]any{"path": "plain.txt", "dest_dir": ".."}},
				{"delete", map[string]any{"path": "../x"}},
			}
		case symlink:
			return []struct {
				op   string
				body map[string]any
			}{
				{"write", map[string]any{"path": "flink", "content": "w"}},
				{"mkdir", map[string]any{"parent": "link", "name": "x"}},
				{"rename", map[string]any{"path": "flink", "new_name": "y"}},
				{"move", map[string]any{"path": "flink", "dest_dir": "real"}},
				{"delete", map[string]any{"path": "flink"}},
			}
		}
		return []struct {
			op   string
			body map[string]any
		}{
			{"write", map[string]any{"path": "ro.txt", "content": "w"}},
			{"mkdir", map[string]any{"parent": "", "name": "ro"}},
			{"rename", map[string]any{"path": "plain.txt", "new_name": "ro"}},
			{"move", map[string]any{"path": "plain.txt", "dest_dir": "real"}},
			{"delete", map[string]any{"path": "plain.txt"}},
		}
	}
	want := map[string]string{} // tool -> codes seen, in order
	run := func(code string, traversal, symlink bool) {
		for _, m := range mutations(traversal, symlink) {
			r := env.op(t, m.op, m.body)
			if r.status != 403 || r.code(t) != code {
				t.Fatalf("%s %v: %d %s, want 403 %s", m.op, m.body, r.status, r.raw, code)
			}
			want[m.op] += code + ","
		}
	}
	run(projectfs.CodeTraversal, true, false)
	run(projectfs.CodeSymlink, false, true)
	env.setProject(t, func(p *config.Project) { p.FilesReadOnly = true })
	run(projectfs.CodeReadOnly, false, false)

	got := map[string]string{}
	rows := fileOpRows(t, env, true)
	for _, row := range rows {
		if row.Outcome != audit.AuditOutcomeDenied || row.Phase != "" || row.Actor.ProjectID != env.proj.ID {
			t.Errorf("row %s %s: outcome %q phase %q project %q; want a phaseless denied row for the project",
				row.Tool, row.Error, row.Outcome, row.Phase, row.Actor.ProjectID)
		}
		got[row.Tool] += row.Error + ","
	}
	if len(rows) != 15 {
		t.Errorf("rows = %d, want 15 (one per refusal)", len(rows))
	}
	for tool, codes := range want {
		if got[tool] != codes {
			t.Errorf("%s denied rows = %q, want %q", tool, got[tool], codes)
		}
	}
}

func TestFileAudit_ReadsWriteNoRows(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "a.txt", "TODO")
	env.op(t, "list", map[string]any{"path": ""})
	env.op(t, "stat", map[string]any{"path": "a.txt"})
	env.op(t, "read", map[string]any{"path": "a.txt"})
	env.op(t, "read", map[string]any{"path": "missing.txt"})
	env.op(t, "read", map[string]any{"path": "../a.txt"})
	env.stream(t, "a.txt", nil)
	env.op(t, "search", map[string]any{"query": "TODO"})
	env.op(t, "git", map[string]any{"args": []string{"status"}})
	if rows := fileOpRows(t, env, true); len(rows) != 0 {
		t.Errorf("reads wrote %d file_op rows: %+v", len(rows), rows)
	}
}

func TestFileAudit_UnwritableIntentRefusesTheMutationWith503(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{})
	env.put(t, "a.txt", "a")
	env.put(t, "dir/keep.txt", "k")
	// Close the log file under the writer so the durable write fails for real.
	assertNoErr(t, env.rec.CloseWriterForTest(), "CloseWriterForTest")

	for _, m := range []struct {
		op   string
		body map[string]any
	}{
		{"write", map[string]any{"path": "new.txt", "content": "w"}},
		{"mkdir", map[string]any{"parent": "", "name": "newdir"}},
		{"rename", map[string]any{"path": "a.txt", "new_name": "b.txt"}},
		{"move", map[string]any{"path": "a.txt", "dest_dir": "dir"}},
		{"delete", map[string]any{"path": "a.txt"}},
	} {
		r := env.op(t, m.op, m.body)
		if r.status != 503 || r.code(t) != projectfs.CodeAuditUnavailable {
			t.Errorf("%s with an unwritable log: %d %s, want 503 AUDIT_UNAVAILABLE", m.op, r.status, r.raw)
		}
	}
	if env.has("new.txt") || env.has("newdir") || env.has("b.txt") || env.has("dir/a.txt") || env.file(t, "a.txt") != "a" {
		t.Error("a mutation ran although its intent row could not be written")
	}
	if r := env.op(t, "read", map[string]any{"path": "a.txt"}); r.status != 200 {
		t.Errorf("read with an unwritable log = %d %s", r.status, r.raw)
	}
}

func TestFileAudit_AuditingOffRunsMutationsAndRecordsNothing(t *testing.T) {
	env := newFileEnv(t, fileEnvOpts{auditOff: true})
	env.put(t, "a.txt", "a")
	for _, m := range []struct {
		op   string
		body map[string]any
	}{
		{"write", map[string]any{"path": "w.txt", "content": "w"}},
		{"mkdir", map[string]any{"parent": "", "name": "d"}},
		{"rename", map[string]any{"path": "a.txt", "new_name": "b.txt"}},
		{"move", map[string]any{"path": "b.txt", "dest_dir": "d"}},
	} {
		if r := env.op(t, m.op, m.body); r.status != 200 {
			t.Errorf("%s with auditing off: %d %s", m.op, r.status, r.raw)
		}
	}
	if !env.has("w.txt") || !env.has("d/b.txt") {
		t.Error("mutations did not run with auditing off")
	}
	if text := aiLogText(t); text != "" {
		t.Errorf("an audit log exists with auditing off: %.200s", text)
	}
}

func TestFileAudit_PasteTmpRowHasNoProjectFields(t *testing.T) {
	pool := &fakeFilePool{}
	env := newFileEnv(t, fileEnvOpts{hosts: pool})
	assertNoErr(t, env.store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "testbox", Target: "testbox"})
	}), "add host")

	r := env.request(t, "POST", "/api/hosts/nope/pastetmp", map[string]any{"name": "eve-paste-1-ab12.png", "data_b64": "AA=="}, nil)
	if r.status != 404 || r.code(t) != projectfs.CodeHostNotFound {
		t.Errorf("unknown host = %d %s, want 404 HOST_NOT_FOUND", r.status, r.raw)
	}
	r = env.request(t, "POST", "/api/hosts/h1/pastetmp", map[string]any{"name": "eve-paste-1-ab12.png", "data_b64": "AAEC"}, nil)
	if m := r.json(t); r.status != 200 || m["path"] != "/tmp/eve-paste-1-ab12.png" {
		t.Fatalf("pastetmp = %d %s", r.status, r.raw)
	}
	rows := fileOpRows(t, env, true)
	if len(rows) != 2 {
		t.Fatalf("pastetmp rows = %d, want intent and completion", len(rows))
	}
	args := fileRowArgs(t, rows[0])
	if rows[0].Tool != "pastetmp" || args["host_id"] != "h1" || args["name"] != "eve-paste-1-ab12.png" || args["bytes"] != 3.0 ||
		rows[0].Actor.ProjectID != "" || rows[1].Outcome != audit.AuditOutcomeOK {
		t.Errorf("pastetmp rows = %+v", rows)
	}
}
