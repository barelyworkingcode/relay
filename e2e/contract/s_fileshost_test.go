package contract

import (
	"net/http"
	"os"
	"testing"

	"relaye2e/harness"
)

const boxFiles = "/api/projects/" + acmeHostProj + "/files/"

func hostFileOp(r *Run, op string, body any) harness.Response {
	return r.HTTP("ops", "POST", boxFiles+op, body)
}

func TestFilesHostListReadWriteStat(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			hostFileOp(r, "list", map[string]any{"path": "docs"})
			hostFileOp(r, "stat", map[string]any{"path": "main.go"})
			hostFileOp(r, "stat", map[string]any{"path": "missing.go"})
			hostFileOp(r, "read", map[string]any{"path": "main.go"})
			hostFileOp(r, "write", map[string]any{"path": "docs/two.txt", "content": "two\n"})
			hostFileOp(r, "read", map[string]any{"path": "docs/two.txt"})
			hostFileOp(r, "write", map[string]any{"path": "docs/two.txt", "content": "x", "create_only": true})
			hostFileOp(r, "mkdir", map[string]any{"parent": "docs", "name": "lib"})
			hostFileOp(r, "read", map[string]any{"path": "../outside"})
		},
	})
}

func TestFilesHostRenameExisting(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			hostFileOp(r, "rename", map[string]any{"path": "docs/one.txt", "new_name": "util.go"})
			hostFileOp(r, "write", map[string]any{"path": "src/one.txt", "content": "taken\n"})
			hostFileOp(r, "move", map[string]any{"path": "docs/one.txt", "dest_dir": "src"})
			hostFileOp(r, "read", map[string]any{"path": "docs/one.txt"})
			hostFileOp(r, "rename", map[string]any{"path": "docs/one.txt", "new_name": "uno.txt"})
			hostFileOp(r, "read", map[string]any{"path": "docs/uno.txt"})
		},
	})
}

func TestFilesHostStreamIgnoresRange(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", boxFiles+"stream?path=main.go", nil)
			r.HTTP("ops", "GET", boxFiles+"stream?path=main.go", nil,
				harness.ReqOpts{Header: http.Header{"Range": {"bytes=0-3"}}})
		},
	})
}

func TestFilesHostDelete(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			hostFileOp(r, "delete", map[string]any{"path": "docs/one.txt"})
			hostFileOp(r, "stat", map[string]any{"path": "docs/one.txt"})
		},
	})
}

func TestFilesHostPastetmp(t *testing.T) {
	t.Parallel()
	const name = "eve-paste-1789000001-ab12cd34.png"
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			// The agent writes to the real /tmp. Remove only the exact path the
			// scenario asked for.
			r.T.Cleanup(func() { _ = os.Remove("/tmp/" + name) })
			r.HTTP("ops", "POST", "/api/hosts/"+acmeHostID+"/pastetmp",
				map[string]any{"name": name, "data_b64": "iVBORw0KGgo="})
			r.HTTP("ops", "POST", "/api/hosts/"+acmeHostID+"/pastetmp",
				map[string]any{"name": "not-a-paste.png", "data_b64": "iVBORw0KGgo="})
			r.HTTP("ops", "POST", "/api/hosts/h_missing/pastetmp",
				map[string]any{"name": name, "data_b64": "iVBORw0KGgo="})
		},
	})
}

func TestFilesHostUnreachable(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Files,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			hostFileOp(r, "stat", map[string]any{"path": "main.go"})
			r.Target.SetHostStatus(acmeHostID, "unreachable")
			hostFileOp(r, "stat", map[string]any{"path": "main.go"})
			hostFileOp(r, "write", map[string]any{"path": "docs/late.txt", "content": "x"})
			r.Target.SetHostStatus(acmeHostID, "connected")
			hostFileOp(r, "stat", map[string]any{"path": "main.go"})
		},
	})
}
