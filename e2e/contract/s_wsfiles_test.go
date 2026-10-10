package contract

import (
	"testing"

	"relaye2e/harness"
)

func watchFrame(project string) map[string]any {
	return map[string]any{"type": "watch", "project_id": project}
}

func onPath(path string) func(map[string]any) bool {
	return func(f map[string]any) bool { return f["path"] == path }
}

func wsFilesSpec() Spec {
	s := filesSpec()
	s.Presence = map[string]harness.Outcome{"project.grant": harness.OutcomeApprove}
	s.Projects = append(s.Projects, Project{ID: "p_beta", Name: "Beta", Mode: "work"})
	return s
}

func TestWSFilesWatchEvent(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSFiles,
		Spec:    wsFilesSpec(),
		Body: func(r *Run) {
			w := r.WS("/ws/files", "ops")
			w.Send(watchFrame("p_acme"))
			w.Expect("watch_ok", nil)
			w.Send(watchFrame("p_acme"))
			w.Expect("watch_ok", nil)
			fileOp(r, "write", map[string]any{"path": "notes.md", "content": "hello\n"})
			w.Expect("fs_event", onPath("notes.md"))
			w.Send(watchFrame("p_missing"))
			w.Expect("watch_error", nil)
		},
	})
}

func TestWSFilesUnwatch(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSFiles,
		Spec:    wsFilesSpec(),
		Body: func(r *Run) {
			w := r.WS("/ws/files", "ops")
			w.Send(watchFrame("p_acme"))
			w.Expect("watch_ok", nil)
			w.Send(watchFrame("p_beta"))
			w.Expect("watch_ok", nil)
			w.Send(map[string]any{"type": "unwatch", "project_id": "p_acme"})
			// Frames for a connection arrive in order, so the first fs_event
			// after the unwatch is the one the beta write causes.
			fileOp(r, "write", map[string]any{"path": "after-unwatch.md", "content": "x\n"})
			r.HTTP("ops", "POST", "/api/projects/p_beta/files/write",
				map[string]any{"path": "beta.md", "content": "x\n"})
			w.Until("fs_event")
		},
	})
}

func TestWSFilesHostWatch(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSFiles,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			// The first op connects the host's agent, so the upgrade has one to report.
			hostFileOp(r, "stat", map[string]any{"path": "main.go"})
			w := r.WS("/ws/files", "ops")
			w.Send(watchFrame(acmeHostProj))
			w.Until("watch_ok")
			hostFileOp(r, "write", map[string]any{"path": "docs/watched.txt", "content": "x\n"})
			w.Expect("fs_event", onPath("docs/watched.txt"))
		},
	})
}

func TestWSFilesProjectChanged(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: WSFiles,
		Spec:    wsFilesSpec(),
		Body: func(r *Run) {
			w := r.WS("/ws/files", "ops")
			w.Send(watchFrame("p_acme"))
			w.Expect("watch_ok", nil)
			r.HTTP("ops", "PUT", "/api/projects/p_acme",
				map[string]any{"path": newProjectDir(r, "moved")})
			w.Expect("watch_error", nil)
		},
	})
}
