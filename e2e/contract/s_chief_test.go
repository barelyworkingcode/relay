package contract

import (
	"strings"
	"testing"

	"relaye2e/harness"
)

func TestChiefOfStaffConfig(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
		},
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 200},
				harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "chief_of_staff.config.set", Trace: tr})
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "gpt", "dailyModelCalls": 200})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_missing", "model": "sonnet", "dailyModelCalls": 200})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 0})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config", map[string]any{"projectId": "p_acme"})
			r.HTTP("reader", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 200})
			r.HTTP("ops", "DELETE", "/api/chief-of-staff/config", nil)
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
		},
	})
}

func TestChiefOfStaffSessionCreate(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
			ChiefOfStaff: &struct {
				ProjectID, Model string
				DailyModelCalls  int
			}{ProjectID: "p_acme", Model: "haiku", DailyModelCalls: 50},
		},
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			// Without the scope header the route is refused for every credential.
			body := map[string]any{"projectId": "p_acme", "prompt": "hello", "model": "haiku"}
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions", body)
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions",
				map[string]any{"projectId": "p_acme", "model": "haiku"}, cosScope())
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions", body, cosScope())
		},
	})
}

func cosAgentsSpec() Spec {
	return Spec{
		Credentials: acmeOpsCreds(),
		Presence: map[string]harness.Outcome{
			"project.agent":         harness.OutcomeApprove,
			"project.chat_template": harness.OutcomeApprove,
		},
		Projects: []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
	}
}

// putChats stores chat templates the way a frontend's project editor does.
// cosTraced is the Chief of Staff scope plus a trace id.
func cosTraced(tr string) harness.ReqOpts {
	o := cosScope()
	o.Trace = tr
	return o
}

func putChats(r *Run, templates ...map[string]any) {
	r.T.Helper()
	r.HTTP("ops", "PUT", "/api/projects/p_acme", map[string]any{"chat_templates": templates})
}

func TestChiefOfStaffAgentList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec:    cosAgentsSpec(),
		Body: func(r *Run) {
			const list = "/api/chief-of-staff/projects/p_acme/agents"
			const preview = list + "/preview"
			r.HTTP("ops", "GET", list, nil, cosScope())
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "Researcher", "kind": "claude", "model": "sonnet"})
			// Stored args stay out of the list.
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "wrapped", "kind": "claude", "command": "/bin/ls", "args": []string{"--profile", "work"}})
			putChats(r,
				map[string]any{"id": "t_review", "name": "Reviewer", "model": "local/chat", "system_prompt": "Review the diff."},
				map[string]any{"id": "t_dup", "name": "Reviewer", "model": "sonnet"},
			)
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", list, nil, cosTraced(tr))
			r.Event(harness.EventQuery{Key: "chief_of_staff.agents.list", Trace: tr, Fields: map[string]any{"status": "ok", "count": float64(4)}})
			r.HTTP("ops", "GET", list, nil)
			r.HTTP("reader", "GET", list, nil, cosScope())
			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/chief-of-staff/projects/p_missing/agents", nil, cosTraced(tr))
			r.Event(harness.EventQuery{Key: "chief_of_staff.agents.list", Trace: tr, Fields: map[string]any{"status": "denied"}})

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", preview, map[string]any{"name": "lister", "kind": "claude", "model": "sonnet", "command": "/bin/ls"}, cosTraced(tr))
			r.Event(harness.EventQuery{Key: "chief_of_staff.agents.preview", Trace: tr, Fields: map[string]any{"status": "ok", "machine": "this Mac"}})
			r.HTTP("ops", "POST", preview, map[string]any{"name": "lister", "kind": "claude", "command": "/bin/ls", "args": []string{"x"}}, cosScope())
			r.HTTP("ops", "POST", preview, map[string]any{"name": "lister", "kind": "claude", "command": "/tmp/a'b"}, cosScope())
			r.HTTP("ops", "POST", preview, map[string]any{"name": "ghost", "kind": "claude", "command": "acme-no-such-wrapper"}, cosScope())
			r.HTTP("ops", "POST", preview, map[string]any{"name": "researcher", "kind": "claude"}, cosScope())
			r.HTTP("ops", "POST", preview, map[string]any{"name": "scout", "kind": "claude"}, cosScope())
			r.HTTP("ops", "POST", "/api/chief-of-staff/projects/p_missing/agents/preview", map[string]any{"name": "scout", "kind": "claude"}, cosScope())
			r.HTTP("ops", "POST", preview, map[string]any{"name": "scout", "kind": "claude"})
			// The preview wrote nothing.
			r.HTTP("ops", "GET", "/api/projects/p_acme/agents", nil)
		},
	})
}

func TestChiefOfStaffStartEntry(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec:    cosAgentsSpec(),
		Body: func(r *Run) {
			const start = "/api/chief-of-staff/sessions"
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "Researcher", "kind": "claude", "model": "sonnet"})
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "odd", "kind": "claude", "model": "gpt-x"})
			putChats(r, map[string]any{"id": "t_review", "name": "Reviewer", "model": "sonnet", "system_prompt": "Review the diff."})
			begin := func(body map[string]any, tr string) harness.Response {
				body["projectId"], body["prompt"] = "p_acme", "Fix the build"
				resp := r.HTTP("ops", "POST", start, body, cosTraced(tr))
				if resp.Status == 201 {
					var out struct {
						SessionID string `json:"sessionId"`
					}
					resp.JSON(r.T, &out)
					r.Learn(out.SessionID)
				}
				return resp
			}
			tr := harness.NewTrace(r.T)
			begin(map[string]any{"entry": "researcher"}, tr)
			r.Event(harness.EventQuery{Key: "chief_of_staff.start", Trace: tr, Fields: map[string]any{"status": "ok", "entry": "Researcher", "entry_type": "agent"}})
			tr = harness.NewTrace(r.T)
			begin(map[string]any{"entry": "REVIEWER"}, tr)
			r.Event(harness.EventQuery{Key: "chief_of_staff.start", Trace: tr, Fields: map[string]any{"status": "ok", "entry": "Reviewer", "entry_type": "chat"}})

			tr = harness.NewTrace(r.T)
			begin(map[string]any{"entry": "codx"}, tr)
			r.Event(harness.EventQuery{Key: "chief_of_staff.start", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "entry_not_found"}})
			begin(map[string]any{"entry": "researcher", "model": "haiku"}, "")
			begin(map[string]any{}, "")
			begin(map[string]any{"entry": "researcher", "mode": "terminal"}, "")
			tr = harness.NewTrace(r.T)
			begin(map[string]any{"entry": "odd"}, tr)
			r.Event(harness.EventQuery{Key: "chief_of_staff.start", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "model_not_offered"}})
			begin(map[string]any{"model": "haiku", "entry": ""}, "")

			// Two chats that differ only in case, and a chat with no model.
			putChats(r,
				map[string]any{"id": "t_a", "name": "Helper", "model": "sonnet"},
				map[string]any{"id": "t_b", "name": "helper", "model": "sonnet"},
				map[string]any{"id": "t_d", "name": "Blank", "model": ""},
			)
			tr = harness.NewTrace(r.T)
			begin(map[string]any{"entry": "HELPER"}, tr)
			r.Event(harness.EventQuery{Key: "chief_of_staff.start", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "entry_ambiguous"}})
			begin(map[string]any{"entry": "blank"}, "")
			// An unscoped caller never reaches the route.
			r.HTTP("ops", "POST", start, map[string]any{"projectId": "p_acme", "prompt": "hi", "entry": "odd"})
			r.HTTP("ops", "POST", start, map[string]any{"projectId": "p_missing", "prompt": "hi", "entry": "odd"}, cosScope())
		},
	})
}

func TestChatTemplateAdd(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    cosAgentsSpec(),
		Body: func(r *Run) {
			const base = "/api/projects/p_acme/chat-templates"
			post := func(body any) harness.Response { return r.HTTP("ops", "POST", base, body) }
			tr := harness.NewTrace(r.T)
			resp := r.HTTP("ops", "POST", base, map[string]any{"name": "Researcher", "model": "local/chat"}, harness.ReqOpts{Trace: tr})
			var out struct {
				Template struct {
					ID string `json:"id"`
				} `json:"template"`
			}
			resp.JSON(r.T, &out)
			r.Learn(out.Template.ID)
			r.Event(harness.EventQuery{Key: "chat_template.add", Trace: tr, Fields: map[string]any{"status": "ok", "project_id": "p_acme", "name": "Researcher"}})
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
			r.HTTP("ops", "GET", "/api/chief-of-staff/projects/p_acme/agents", nil, cosScope())

			tr = harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"name": "researcher", "model": "sonnet"}, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "chat_template.add", Trace: tr, Fields: map[string]any{"status": "error", "reason": "conflict"}})
			r.HTTP("ops", "POST", "/api/projects/p_acme/agents", map[string]any{"name": "Scout", "kind": "claude"})
			post(map[string]any{"name": "scout", "model": "sonnet"})
			post(map[string]any{"name": "Planner", "model": "sonnet", "colour": "red"})
			post("not a template")
			post(map[string]any{"name": strings.Repeat("n", 61), "model": "sonnet"})
			post(map[string]any{"name": "  ", "model": "sonnet"})
			post(map[string]any{"name": "Planner"})
			post(map[string]any{"name": "Planner", "model": "has space"})
			r.HTTP("ops", "POST", "/api/projects/p_missing/chat-templates", map[string]any{"name": "Planner", "model": "sonnet"})
			r.HTTP("reader", "POST", base, map[string]any{"name": "Planner", "model": "sonnet"})
			// The scope never reaches the gated door.
			r.HTTP("ops", "POST", base, map[string]any{"name": "Planner", "model": "sonnet"}, cosScope())
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
		},
	})
}

func TestChatTemplateAddRefused(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    cosAgentsSpec(),
		Body: func(r *Run) {
			r.Target.SetPresence(map[string]harness.Outcome{"project.chat_template": harness.OutcomeDeny})
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", "/api/projects/p_acme/chat-templates", map[string]any{"name": "Researcher", "model": "sonnet"}, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "chat_template.add", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
			r.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"})
		},
	})
}
