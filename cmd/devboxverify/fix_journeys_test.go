package main

import (
	"strings"
	"testing"
)

func checkDetail(t *testing.T, got result, want string) {
	t.Helper()
	if !strings.Contains(got.Detail, want) {
		t.Errorf("detail %q does not mention %q", got.Detail, want)
	}
}

func TestFrontendRefusal(t *testing.T) {
	cases := []struct {
		name    string
		r       frontendResponse
		want    state
		refused bool
		detail  string
	}{
		{"timeout beats an unreachable socket", frontendResponse{TimedOut: true}, stateFail, true, "press Cancel"},
		{"socket unreachable", frontendResponse{}, stateBlocked, true, ""},
		{"run credential expired or revoked", frontendResponse{Status: 401, Error: "unauthorized"}, stateBlocked, true, ""},
		{"credential lacks configure", frontendResponse{Status: 403, Error: "Forbidden"}, stateBlocked, true, ""},
		{"presence asked", frontendResponse{Status: 403, Error: "presence required"}, stateFail, true, ""},
		{"accepted", frontendResponse{Status: 200}, "", false, ""},
		{"server error", frontendResponse{Status: 500, Error: "boom"}, stateFail, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, refused := frontendRefusal("j1", "save", c.r)
			if refused != c.refused {
				t.Fatalf("refused = %v, want %v (result %+v)", refused, c.refused, res)
			}
			if !c.refused {
				return
			}
			checkState(t, res, c.want)
			checkDetail(t, res, c.detail)
			if strings.Contains(res.Detail, "P7") {
				t.Errorf("detail %q points at the removed P7 credential file", res.Detail)
			}
		})
	}
}

func TestClassifyStaleDerivedEdit(t *testing.T) {
	type in struct {
		before, after staleState
		r             frontendResponse
	}
	derived := "context field file_dirs is derived by relay from the project's path"
	cases := []mutCase[in]{
		{"access edit drops only the stale field", func(*in) {}, statePass},
		{"profile not found", func(c *in) { c.before.Found = false }, stateBlocked},
		{"profile not remote", func(c *in) { c.before.Remote = false }, stateBlocked},
		{"mcp not granted", func(c *in) { c.before.Granted = false }, stateBlocked},
		{"mcp not registered", func(c *in) { c.before.Registered = false }, stateBlocked},
		{"fixture spent", func(c *in) { c.before.FileDirs = false }, stateNotRun},
		{"derived refusal", func(c *in) { c.r = frontendResponse{Status: 400, Error: derived}; c.after = c.before }, stateFail},
		{"credential lacks configure", func(c *in) { c.r = frontendResponse{Status: 403, Error: "Forbidden"}; c.after = c.before }, stateBlocked},
		{"presence prompt held it", func(c *in) { c.r = frontendResponse{TimedOut: true}; c.after = c.before }, stateFail},
		{"stale field kept", func(c *in) { c.after.FileDirs = true }, stateFail},
		{"mail accounts dropped", func(c *in) { c.after.MailAccounts = nil }, stateFail},
		{"mail accounts replaced", func(c *in) { c.after.MailAccounts = []string{"Bob"} }, stateFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			armed := staleState{Found: true, Remote: true, Granted: true, Registered: true, FileDirs: true, MailAccounts: []string{"Alice"}}
			v := in{before: armed, r: frontendResponse{Status: 200}, after: armed}
			v.after.FileDirs, v.after.MailAccounts = false, []string{"Alice"}
			c.mut(&v)
			got := classifyStaleDerivedEdit(v.before, v.r, v.after)
			checkState(t, got, c.want)
			if c.want == stateNotRun || c.want == stateBlocked && v.r.Status == 200 {
				checkDetail(t, got, "P5")
			}
		})
	}
}

func TestClassifyContextNumber(t *testing.T) {
	cases := []mutCase[numbersRun]{
		{"untouched number saves without presence", func(*numbersRun) {}, statePass},
		{"profile not found", func(r *numbersRun) { r.Found = false }, stateBlocked},
		{"mcp not granted", func(r *numbersRun) { r.Granted = false }, stateBlocked},
		{"fixture not restored", func(r *numbersRun) { r.Before = "1" }, stateBlocked},
		{"save held by presence", func(r *numbersRun) {
			r.Save, r.Restore = numbersSave{Resp: frontendResponse{TimedOut: true}, Stored: "1.0"}, numbersSave{}
		}, stateFail},
		{"credential lacks configure", func(r *numbersRun) {
			r.Save, r.Restore = numbersSave{Resp: frontendResponse{Status: 403, Error: "Forbidden"}, Stored: "1.0"}, numbersSave{}
		}, stateBlocked},
		{"save recorded as widening", func(r *numbersRun) { r.Save.Widening = true }, stateFail},
		{"save ignored", func(r *numbersRun) { r.Save.Stored = "1.0" }, stateFail},
		{"restore held by presence", func(r *numbersRun) { r.Restore = numbersSave{Resp: frontendResponse{TimedOut: true}, Stored: "1"} }, stateFail},
		{"restore recorded as widening", func(r *numbersRun) { r.Restore.Widening = true }, stateFail},
		{"restore not stored", func(r *numbersRun) { r.Restore.Stored = "1" }, stateFail},
	}
	details := map[string]string{
		"profile not found": "P6", "mcp not granted": "P6", "fixture not restored": "P6",
		"save held by presence": "presence", "restore held by presence": "n = 1",
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := numbersRun{Found: true, Granted: true, Before: "1.0",
				Save:    numbersSave{Resp: frontendResponse{Status: 200}, Stored: "1"},
				Restore: numbersSave{Resp: frontendResponse{Status: 200}, Stored: "1.0"}}
			c.mut(&v)
			got := classifyContextNumber(v)
			checkState(t, got, c.want)
			if d, ok := details[c.name]; ok {
				checkDetail(t, got, d)
			}
		})
	}
}

func TestMcpListed(t *testing.T) {
	table := "ID                    NAME   TRANSPORT  ENDPOINT\n" +
		"devboxverify-scope-2  Scope  stdio      /opt/acme/testmcp\n"
	cases := []struct {
		table, id string
		want      bool
	}{
		{table, "devboxverify-scope-2", true},
		{table, "devboxverify-scope", false},
		{table, "ID", false},
		{"no mcp servers registered\n", "devboxverify-scope", false},
	}
	for _, c := range cases {
		if got := mcpListed(c.table, c.id); got != c.want {
			t.Errorf("mcpListed(%q, %q) = %v, want %v", c.table, c.id, got, c.want)
		}
	}
}

func TestRunCredential(t *testing.T) {
	if _, res, ok := runCredential(env{Run: &runState{}}, "j1"); ok || res.ID != "j1" || res.State != stateBlocked || !strings.Contains(res.Detail, mintPosID) {
		t.Errorf("no run credential = %+v, ok %v; want BLOCKED j1 naming %s", res, ok, mintPosID)
	}
	if token, _, ok := runCredential(env{Run: &runState{RunCredID: "c1", RunToken: "tok-p1"}}, "j1"); !ok || token != "tok-p1" {
		t.Errorf("run credential = %q, ok %v; want tok-p1", token, ok)
	}
}

func TestFixJourneysNeedRunCredential(t *testing.T) {
	e := blockedEnv(t)
	e.Run = &runState{}
	for _, id := range []string{staleID, "context-number-resave"} {
		t.Run(id, func(t *testing.T) {
			got := runJourney(t, id, e)
			checkState(t, got, stateBlocked)
			checkDetail(t, got, mintPosID)
		})
	}
}
