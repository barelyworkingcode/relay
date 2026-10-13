package contract

import (
	"testing"

	"relaye2e/harness"
)

func agentsSpec() Spec {
	return Spec{
		Credentials: acmeOpsCreds(),
		Presence:    map[string]harness.Outcome{"project.agent": harness.OutcomeApprove},
		Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
	}
}

// agentEvent waits for the event the request's trace wrote.
func agentEvent(r *Run, key, tr string, fields map[string]any) {
	r.T.Helper()
	r.Event(harness.EventQuery{Key: key, Trace: tr, Fields: fields})
}

func TestAgentsCRUD(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    agentsSpec(),
		Body: func(r *Run) {
			const base = "/api/projects/p_acme/agents"
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", base, nil, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.list", tr, nil)
			// An entry with no command still names the machine in its event.
			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "Researcher", "kind": "claude", "model": "sonnet"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.add", tr, map[string]any{"status": "ok", "machine": "this Mac"})
			// An absolute path goes through the lookup and is stored as found.
			// The events are read on entries with a command, which both targets
			// write with the machine.
			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "wrapped", "kind": "claude", "command": "/bin/ls", "args": []string{"--profile", "work"}}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.add", tr, map[string]any{"status": "ok"})
			r.HTTP("ops", "POST", base, map[string]any{"name": "piLocal", "kind": "pi", "model": "local/coder"})
			r.HTTP("ops", "GET", base, nil)
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", base+"/researcher", map[string]any{"name": "Analyst", "kind": "claude", "model": "haiku"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.update", tr, map[string]any{"status": "ok", "machine": "this Mac"})
			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", base+"/wrapped", map[string]any{"name": "wrapped", "kind": "claude", "model": "opus", "command": "/bin/ls"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.update", tr, map[string]any{"status": "ok"})
			r.HTTP("ops", "GET", base, nil)

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "DELETE", base+"/wrapped", nil, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.remove", tr, map[string]any{"status": "ok"})
			r.HTTP("ops", "GET", base, nil)

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "DELETE", base+"/analyst", nil, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.remove", tr, map[string]any{"status": "ok", "machine": "this Mac"})
		},
	})
}

func TestAgentsRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    agentsSpec(),
		Body: func(r *Run) {
			const base = "/api/projects/p_acme/agents"
			post := func(body any) { r.HTTP("ops", "POST", base, body) }
			post(map[string]any{"name": "Researcher", "kind": "claude", "model": "sonnet"})

			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "scout", "kind": "gemini"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.add", tr, map[string]any{"status": "error", "reason": "invalid"})
			r.HTTP("ops", "POST", base, map[string]any{"name": "scout", "kind": "claude", "colour": "red"})
			r.HTTP("ops", "POST", base, "not an entry")
			post(map[string]any{"name": "bad name", "kind": "claude"})
			post(map[string]any{"name": "scout", "kind": "pi"})
			post(map[string]any{"name": "scout", "kind": "claude", "args": []string{"x"}})
			post(map[string]any{"name": "ghost", "kind": "claude", "command": "nosuchwrapper"})

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "researcher", "kind": "codex"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.add", tr, map[string]any{"status": "error", "reason": "conflict"})
			r.HTTP("ops", "PUT", base+"/nobody", map[string]any{"name": "nobody", "kind": "claude"})
			r.HTTP("ops", "DELETE", base+"/nobody", nil)
			r.HTTP("ops", "POST", "/api/projects/p_missing/agents", map[string]any{"name": "scout", "kind": "claude"})
			r.HTTP("ops", "GET", "/api/projects/p_missing/agents", nil)

			// A chat template and an agent cannot share a name, in either order.
			r.HTTP("ops", "PUT", "/api/projects/p_acme", map[string]any{"chat_templates": []map[string]any{
				{"id": "t_review", "name": "Reviewer", "model": "claude-sonnet"},
			}})
			post(map[string]any{"name": "reviewer", "kind": "claude"})
			r.HTTP("ops", "PUT", "/api/projects/p_acme", map[string]any{"chat_templates": []map[string]any{
				{"id": "t_review", "name": "Reviewer", "model": "claude-sonnet"},
				{"id": "t_res", "name": "researcher", "model": "claude-sonnet"},
			}})

			r.HTTP("reader", "POST", base, map[string]any{"name": "scout", "kind": "claude"})
			r.HTTP("nobody", "GET", base, nil)

			r.Target.SetPresence(map[string]harness.Outcome{"project.agent": harness.OutcomeDeny})
			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "scout", "kind": "claude", "command": "/bin/ls"}, harness.ReqOpts{Trace: tr})
			agentEvent(r, "agent.add", tr, map[string]any{"status": "denied", "reason": "presence_refused"})
			r.HTTP("ops", "GET", base, nil)
			// The refusal row names the project by id.
			r.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"})
		},
	})
}

func TestAgentsStartByEntry(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    agentsSpec(),
		Body: func(r *Run) {
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "Researcher", "kind": "claude", "model": "sonnet"})
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "odd", "kind": "claude", "model": "gpt-x"})
			start := func(body map[string]any) {
				body["projectId"], body["name"] = "p_acme", "Fix the build"
				resp := r.HTTP("ops", "POST", "/api/sessions", body)
				if resp.Status == 201 {
					var out struct {
						SessionID string `json:"sessionId"`
					}
					resp.JSON(r.T, &out)
					r.Learn(out.SessionID)
				}
			}
			start(map[string]any{"entry": "researcher"})
			start(map[string]any{"entry": "nope"})
			start(map[string]any{"entry": "odd"})
			start(map[string]any{"entry": "researcher", "model": "haiku"})
			start(map[string]any{"model": "haiku"})
		},
	})
}
