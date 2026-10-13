package prooftest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func agentWorld(dir string) any {
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "mywrapper"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		panic(err)
	}
	return J{"schema": 1, "credentials": creds(), "presence": J{"project.agent": "approve"},
		"local_path": bin, "host_path": bin,
		"models": []J{{"value": "codex/fake-echo", "label": "Fake", "group": "Codex", "provider": "codex"}},
		"projects": []J{
			{"id": "p_acme", "name": "Acme", "chat_templates": []J{{"name": "Researcher"}},
				"agents": []J{{"name": "seeded", "kind": "claude", "model": "sonnet"}}},
			{"id": "p_box", "name": "Box", "host_id": "h_box", "path": "/home/acme/app"},
		},
		"hosts": []J{{"id": "h_box", "name": "testbox", "target": "acme@testbox", "agent": "connected"}}}
}

// Criteria: agent list routes, command lookup, coded refusals, presence gate,
// start by entry and the launcher in ctl state.
func TestAgentList(t *testing.T) {
	t.Parallel()
	in := startInstance(t, agentWorld)
	bin := filepath.Join(in.dir, "bin", "mywrapper")

	r := in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "mine", "kind": "claude", "model": "haiku", "command": "mywrapper", "args": []string{"--x"}}).is(t, 201).obj(t)
	eq(t, [3]any{r["projectId"], r["projectName"], r["machine"]}, [3]any{"p_acme", "Acme", "this Mac"}, "add result")
	eq(t, r["entry"].(map[string]any)["command"], any(bin), "stored command is the absolute path")

	got := in.api(t, "GET", "/api/projects/p_acme/agents", nil).is(t, 200).obj(t)
	eq(t, len(got["entries"].([]any)), 2, "entries")
	eq(t, len(in.api(t, "GET", "/api/projects/p_acme", nil).is(t, 200).obj(t)["agents"].([]any)), 2, "view agents")
	_, omitted := in.api(t, "GET", "/api/projects/p_box", nil).is(t, 200).obj(t)["agents"]
	eq(t, omitted, false, "agents omitted when empty")

	refusal := func(res J, code string) {
		t.Helper()
		eq(t, res["error"], any(code), "error code")
	}
	refusal(in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "nope", "kind": "claude", "command": "missing"}).is(t, 422).obj(t), "command_not_found")
	nf := in.api(t, "POST", "/api/projects/p_box/agents", J{"name": "nope", "kind": "claude", "command": "missing"}).is(t, 422).obj(t)
	eq(t, [3]any{nf["message"], nf["command"], nf["machine"]}, [3]any{"missing was not found on testbox", "missing", "testbox"}, "host lookup refusal")
	refusal(in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "mine", "kind": "claude"}).is(t, 409).obj(t), "name_taken")
	tk := in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "researcher", "kind": "claude"}).is(t, 409).obj(t)
	eq(t, tk["message"], any(`"researcher" is already a chat template in Acme`), "template clash")
	refusal(in.api(t, "POST", "/api/projects/p_box/agents", J{"name": "p", "kind": "pi", "model": "m"}).is(t, 400).obj(t), "kind_not_available_on_host")
	refusal(in.api(t, "PUT", "/api/projects/p_acme/agents/zzz", J{"name": "zzz", "kind": "claude"}).is(t, 404).obj(t), "entry_not_found")
	refusal(in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "x", "kind": "claude", "bogus": 1}).is(t, 400).obj(t), "invalid_body")

	ct := in.api(t, "PUT", "/api/projects/p_acme", J{"chat_templates": []J{{"name": "Seeded"}}}).is(t, 400).obj(t)
	eq(t, ct["error"], any(`"Seeded" is already an agent in Acme`), "reverse clash")

	in.api(t, "PUT", "/api/projects/p_acme/agents/mine", J{"name": "renamed", "kind": "claude", "model": "haiku", "command": "mywrapper"}).is(t, 200)
	evs := in.events(t, "", "agent.update")
	eq(t, [2]any{evs[0]["reason"], evs[1]["name"]}, [2]any{"not_found", "renamed"}, "update events")

	s := in.api(t, "POST", "/api/sessions", J{"projectId": "p_acme", "entry": "renamed"}).is(t, 201).obj(t)
	eq(t, [2]any{s["model"], s["providerType"]}, [2]any{"haiku", "claude"}, "session by entry")
	out, se, code := in.cli(t, "ctl", "state", "--json")
	eq(t, code, 0, "ctl state: "+se)
	var st J
	must(t, json.Unmarshal([]byte(out), &st), "ctl state output")
	rows := st["sessions"].([]any)
	eq(t, rows[0].(map[string]any)["launcher"].(map[string]any)["command"], any(bin), "launcher in ctl state")

	both := in.api(t, "POST", "/api/sessions", J{"projectId": "p_acme", "entry": "renamed", "model": "haiku"}).is(t, 400).obj(t)
	eq(t, both["error"], any("send model or entry, not both"), "model and entry")
	unk := in.api(t, "POST", "/api/sessions", J{"projectId": "p_acme", "entry": "x"}).is(t, 404).obj(t)
	eq(t, unk["error"], any(`no agent "x" in Acme; agents: seeded, renamed`), "unknown entry")

	in.api(t, "POST", "/api/projects/p_acme/agents", J{"name": "cx", "kind": "codex", "model": "gone"}).is(t, 201)
	no := in.api(t, "POST", "/api/sessions", J{"projectId": "p_acme", "entry": "cx"}).is(t, 400).obj(t)
	eq(t, no["error"], any(`model "gone" is not offered for codex; models: fake-echo`), "model not offered")

	in.api(t, "DELETE", "/api/projects/p_acme/agents/renamed", nil).is(t, 200)
	eq(t, len(in.api(t, "GET", "/api/projects/p_acme/agents", nil).is(t, 200).obj(t)["entries"].([]any)), 2, "after remove")

	_, se, code = in.cli(t, "ctl", "presence", "project.agent=deny")
	eq(t, code, 0, "ctl presence: "+se)
	eq(t, in.api(t, "DELETE", "/api/projects/p_acme/agents/seeded", nil).is(t, 403).obj(t)["error"], any("presence was refused"), "presence refusal")
}
