package contract

import (
	"testing"

	"relaye2e/harness"
)

func TestAuditFileOpRows(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Audit,
		Spec:    filesSpec(),
		Body: func(r *Run) {
			fileOp(r, "write", map[string]any{"path": "two/audited.md", "content": "hello\n"})
			fileOp(r, "mkdir", map[string]any{"parent": "two", "name": "lib"})
			fileOp(r, "write", map[string]any{"path": "link", "content": "x"})

			r.Audit(harness.AuditQuery{Event: "file_op"})
			r.HTTP("ops", "GET", "/api/audit?event=file_op", nil)
			r.HTTP("ops", "GET", "/api/audit?event=file_op&outcome=denied", nil)
			r.HTTP("ops", "GET", "/api/audit?event=file_op&limit=1", nil)
			r.HTTP("ops", "GET", "/api/audit/log", nil)
			r.HTTP("ops", "GET", "/api/audit?outcome=nonsense", nil)
			r.HTTP("ops", "GET", "/api/audit?limit=x", nil)
			r.HTTP("reader", "GET", "/api/audit?event=file_op&limit=1", nil)
			r.CLI("audit", "--json", "--event", "file_op")
			r.CLI("audit", "--json", "--event", "file_op", "--outcome", "denied")
		},
	})
}
