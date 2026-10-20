package prooftest

import "testing"

func terminalLaunchWorld(string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{{"id": "p1", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}, "allowed_templates": []string{"*"},
			"repos": []J{{"dir": "", "branch": "main", "commits": []J{{"message": "initial", "files": J{"README.md": "# Acme\n"}}}}}}},
		"terminal_templates": []J{{"id": "claude-code", "name": "Claude", "command": "/bin/sh"}}}
}

// Regression: the session.launch line loses the template, the resolved directory or the extra
// arguments a client sent, or serves extra_args as null instead of an array when there are none.
func TestTerminalLaunchEventCarriesRequest(t *testing.T) {
	const id = "0b6e1f2a-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	for _, c := range []struct {
		name string
		body J
		want []any
	}{
		{"with extraArgs", J{"templateId": "claude-code", "projectId": "p1", "extraArgs": []string{"--resume", id}}, []any{"--resume", id}},
		{"without extraArgs", J{"templateId": "claude-code", "projectId": "p1"}, []any{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := startInstance(t, terminalLaunchWorld)
			created := in.api(t, "POST", "/api/terminals", c.body).is(t, 201).obj(t)
			ev := in.waitEvent(t, "session.launch")
			eq[any](t, ev["status"], "ok", "launch status")
			eq[any](t, ev["template_id"], "claude-code", "template_id")
			eq[any](t, ev["directory"], created["directory"], "directory equals the 201 body")
			args, isArray := ev["extra_args"].([]any)
			if !isArray {
				t.Fatalf("extra_args is %#v, want an array", ev["extra_args"])
			}
			eq(t, args, c.want, "extra_args")
		})
	}
}
