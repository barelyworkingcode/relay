package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// worldV1 is devboxWorld's published v1 data/world.json, copied verbatim.
const worldV1 = `{
  "schema": 1,
  "world_version": 1,
  "mail_domain": "relaytest.local",
  "relay_mcp": {"id": "macmcp", "tools": "mail_*"},
  "projects": [
    {"key": "acme",   "name": "Acme Corp", "mode": "work", "color": "#1F6FD1", "relay_mailboxes": ["INBOX", "Archive", "Clients"]},
    {"key": "globex", "name": "Globex",    "mode": "work", "color": "#2E9E5B", "relay_mailboxes": ["INBOX", "Archive"]},
    {"key": "home",   "name": "Home",      "mode": "home", "color": "#D9822B", "relay_mailboxes": ["INBOX", "Archive"]}
  ],
  "fixtures": ["project:acme", "project:globex", "project:home",
               "file:acme/PROJECT.md", "file:globex/PROJECT.md", "file:home/PROJECT.md",
               "file:acme/todo.txt", "file:acme/budget/q4-budget-draft.csv"]
}
`

func refusal(reason string) string {
	return "not a test machine: " + reason + "; run devboxWorld bootstrap on a VM"
}

func vmIs(v bool, err error) func() (bool, error) { return func() (bool, error) { return v, err } }

// markerDoc is a valid marker with mut applied; json.RawMessage values
// place exact tokens such as 1.0.
func markerDoc(checkout string, mut func(map[string]any)) string {
	m := map[string]any{"schema": 1, "world_checkout": checkout, "world_root": "/w/root", "world_version": 1, "written_at": "2026-01-01T00:00:00Z"}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func writeMode(t *testing.T, path, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeWorld writes the v1 catalogue with mut applied as <dir>/data/world.json.
func writeWorld(t *testing.T, mut func(map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(worldV1), &m); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeMode(t, filepath.Join(dir, "data", "world.json"), string(b), 0o600)
	return dir
}

func dropFixtures(ids ...string) func(map[string]any) {
	return func(m map[string]any) {
		m["fixtures"] = slices.DeleteFunc(m["fixtures"].([]any), func(v any) bool { return slices.Contains(ids, v.(string)) })
	}
}

func loadTestWorld(t *testing.T, mut func(map[string]any)) world {
	t.Helper()
	w, err := loadWorld(worldMarker{WorldCheckout: writeWorld(t, mut), WorldRoot: "/w/root", WorldVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestReadMarker(t *testing.T) {
	yes, no, fails := vmIs(true, nil), vmIs(false, nil), vmIs(false, errors.New("sysctl failed"))
	file := func(body string, mode os.FileMode) func(*testing.T, string) string {
		return func(t *testing.T, dir string) string {
			return writeMode(t, filepath.Join(dir, "machine.json"), body, mode)
		}
	}
	valid := func(mut func(map[string]any)) func(*testing.T, string) string {
		return file(markerDoc("/w/checkout", mut), 0o600)
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	del := func(ks ...string) func(map[string]any) {
		return func(m map[string]any) {
			for _, k := range ks {
				delete(m, k)
			}
		}
	}
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	// A path under a regular file: lstat fails with ENOTDIR, not ENOENT.
	underFile := func(t *testing.T, d string) string {
		return filepath.Join(writeMode(t, filepath.Join(d, "f"), "", 0o600), "machine.json")
	}
	cases := []struct {
		name string
		path func(*testing.T, string) string
		vm   func() (bool, error)
		want string
	}{
		{"absent", func(_ *testing.T, d string) string { return filepath.Join(d, "none") }, yes, "not a test machine: run devboxWorld bootstrap on a VM"},
		{"absent on a host", func(_ *testing.T, d string) string { return filepath.Join(d, "none") }, no, "not a test machine: run devboxWorld bootstrap on a VM"},
		{"not a VM", valid(nil), no, refusal("not a VM: kern.hv_vmm_present is not 1")},
		{"sysctl fails", valid(nil), fails, refusal("not a VM: kern.hv_vmm_present is not 1")},
		{"not a VM before a bad marker", file("x", 0o644), no, refusal("not a VM: kern.hv_vmm_present is not 1")},
		{"lstat fails", underFile, yes, refusal("marker is not readable")},
		{"lstat fails on a host", underFile, no, refusal("not a VM: kern.hv_vmm_present is not 1")},
		{"symlink", func(t *testing.T, d string) string {
			p := filepath.Join(d, "link")
			if err := os.Symlink(valid(nil)(t, d), p); err != nil {
				t.Fatal(err)
			}
			return p
		}, yes, refusal("marker is not a regular file")},
		{"directory", func(_ *testing.T, d string) string { return d }, yes, refusal("marker is not a regular file")},
		{"group-readable", file(markerDoc("/w/checkout", nil), 0o640), yes, refusal("marker is open to group or others (mode 0640)")},
		{"others-readable", file(markerDoc("/w/checkout", nil), 0o604), yes, refusal("marker is open to group or others (mode 0604)")},
		{"open mode before bad JSON", file("x", 0o644), yes, refusal("marker is open to group or others (mode 0644)")},
		{"unreadable", file(markerDoc("/w/checkout", nil), 0o000), yes, refusal("marker is not readable")},
		{"not JSON", file("not json", 0o600), yes, refusal("marker is not valid JSON")},
		{"invalid UTF-8", file(`{"schema":1,"world_checkout":"/w/`+"\xff"+`","world_root":"/w/root","world_version":1,"written_at":"x"}`, 0o600), yes, refusal("marker is not valid JSON")},
		{"array", file(`[]`, 0o600), yes, refusal("marker is not a JSON object")},
		{"null", file(`null`, 0o600), yes, refusal("marker is not a JSON object")},
		{"schema absent", valid(del("schema")), yes, refusal("marker schema is not 1")},
		{"schema 2", valid(set("schema", 2)), yes, refusal("marker schema is not 1")},
		{"schema string", valid(set("schema", "1")), yes, refusal("marker schema is not 1")},
		{"schema 1.0", valid(set("schema", raw("1.0"))), yes, refusal("marker schema is not 1")},
		{"schema true", valid(set("schema", true)), yes, refusal("marker schema is not 1")},
		{"schema before fields", valid(func(m map[string]any) { m["schema"] = 2; del("world_checkout")(m) }), yes, refusal("marker schema is not 1")},
		{"lacks world_checkout", valid(del("world_checkout")), yes, refusal("marker lacks world_checkout")},
		{"lacks world_root", valid(del("world_root")), yes, refusal("marker lacks world_root")},
		{"lacks world_version", valid(del("world_version")), yes, refusal("marker lacks world_version")},
		{"lacks written_at", valid(del("written_at")), yes, refusal("marker lacks written_at")},
		{"lacks in key order", valid(del("world_checkout", "world_root", "world_version", "written_at")), yes, refusal("marker lacks world_checkout")},
		{"lacks before absolute", valid(func(m map[string]any) { m["world_checkout"] = "rel"; del("written_at")(m) }), yes, refusal("marker lacks written_at")},
		{"world_checkout relative", valid(set("world_checkout", "w/checkout")), yes, refusal("marker world_checkout is not an absolute path")},
		{"world_checkout not a string", valid(set("world_checkout", 5)), yes, refusal("marker world_checkout is not an absolute path")},
		{"world_root relative", valid(set("world_root", "root")), yes, refusal("marker world_root is not an absolute path")},
		{"world_checkout before world_root", valid(func(m map[string]any) { m["world_checkout"], m["world_root"] = "a", "b" }), yes, refusal("marker world_checkout is not an absolute path")},
		{"absolute before version", valid(func(m map[string]any) { m["world_root"], m["world_version"] = "b", 0 }), yes, refusal("marker world_root is not an absolute path")},
		{"version 0", valid(set("world_version", 0)), yes, refusal("marker world_version is not a positive integer")},
		{"version -1", valid(set("world_version", -1)), yes, refusal("marker world_version is not a positive integer")},
		{"version 1.5", valid(set("world_version", 1.5)), yes, refusal("marker world_version is not a positive integer")},
		{"version 1.0", valid(set("world_version", raw("1.0"))), yes, refusal("marker world_version is not a positive integer")},
		{"version string", valid(set("world_version", "1")), yes, refusal("marker world_version is not a positive integer")},
		{"version true", valid(set("world_version", true)), yes, refusal("marker world_version is not a positive integer")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readMarker(c.path(t, t.TempDir()), c.vm)
			if err == nil || err.Error() != c.want {
				t.Fatalf("readMarker error = %v\nwant %s", err, c.want)
			}
		})
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		got, err := readMarker(file(markerDoc("/w/checkout", set("world_version", 2)), mode)(t, t.TempDir()), yes)
		want := worldMarker{Schema: 1, WorldCheckout: "/w/checkout", WorldRoot: "/w/root", WorldVersion: 2, WrittenAt: "2026-01-01T00:00:00Z"}
		if err != nil || got != want {
			t.Errorf("mode %04o: readMarker = %+v, %v; want %+v", mode, got, err, want)
		}
	}
}

func TestMarkerPath(t *testing.T) {
	t.Setenv("DEVBOXWORLD_MARKER", "/w/elsewhere/marker.json")
	if got := markerPath(); got != "/w/elsewhere/marker.json" {
		t.Errorf("markerPath with DEVBOXWORLD_MARKER = %q", got)
	}
	os.Unsetenv("DEVBOXWORLD_MARKER")
	h := t.TempDir()
	t.Setenv("HOME", h)
	if got, want := markerPath(), filepath.Join(h, ".config", "devboxWorld", "machine.json"); got != want {
		t.Errorf("markerPath = %q, want %q", got, want)
	}
}

func TestLoadWorld(t *testing.T) {
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	del := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }
	addFixture := func(ids ...string) func(map[string]any) {
		return func(m map[string]any) {
			for _, id := range ids {
				m["fixtures"] = append(m["fixtures"].([]any), id)
			}
		}
	}
	projectsMut := func(mut func(p []any)) func(map[string]any) {
		return func(m map[string]any) { mut(m["projects"].([]any)) }
	}
	mcp := func(v any) func(map[string]any) { return set("relay_mcp", v) }
	data := func(muts ...func(map[string]any)) func(*testing.T) string {
		return func(t *testing.T) string {
			return writeWorld(t, func(m map[string]any) {
				for _, mut := range muts {
					mut(m)
				}
			})
		}
	}
	body := func(s string) func(*testing.T) string {
		return func(t *testing.T) string {
			d := writeWorld(t, nil)
			writeMode(t, filepath.Join(d, "data", "world.json"), s, 0o600)
			return d
		}
	}
	const (
		unreadable = "world data is not readable"
		notJSON    = "world data is not valid JSON"
		version    = "world_version is not a positive integer"
		fixtures   = "fixtures is not a list of strings"
		projects   = "projects is not a list"
		badMCP     = "relay_mcp is malformed"
	)
	cases := []struct {
		name string
		dir  func(*testing.T) string
		want string
	}{
		{"no world.json", func(t *testing.T) string { return t.TempDir() }, unreadable},
		{"not JSON", body("{"), notJSON},
		{"top level not an object", body("[]"), notJSON},
		{"version absent", data(del("world_version")), version},
		{"version 0", data(set("world_version", 0)), version},
		{"version string", data(set("world_version", "1")), version},
		{"version 1.0", data(set("world_version", json.RawMessage("1.0"))), version},
		{"version before fixtures", data(set("world_version", 0), del("fixtures")), version},
		{"fixtures absent", data(del("fixtures")), fixtures},
		{"fixtures a string", data(set("fixtures", "project:acme")), fixtures},
		{"fixtures not strings", data(set("fixtures", []any{1})), fixtures},
		{"fixtures before projects", data(set("fixtures", "project:acme"), del("projects")), fixtures},
		{"projects absent", data(del("projects")), projects},
		{"projects an object", data(set("projects", map[string]any{"key": "acme"})), projects},
		{"projects before relay_mcp", data(del("projects"), del("relay_mcp")), projects},
		{"project not an object", data(projectsMut(func(p []any) { p[0] = "acme" })), "project 0 is malformed"},
		{"project without a name", data(projectsMut(func(p []any) { delete(p[1].(map[string]any), "name") })), "project 1 is malformed"},
		{"project with an empty mode", data(projectsMut(func(p []any) { p[2].(map[string]any)["mode"] = "" })), "project 2 is malformed"},
		{"project key not a string", data(projectsMut(func(p []any) { p[0].(map[string]any)["key"] = 7 })), "project 0 is malformed"},
		{"project before relay_mcp", data(projectsMut(func(p []any) { delete(p[1].(map[string]any), "name") }), del("relay_mcp")), "project 1 is malformed"},
		{"relay_mcp absent", data(del("relay_mcp")), badMCP},
		{"relay_mcp not an object", data(mcp("macmcp")), badMCP},
		{"relay_mcp id empty", data(mcp(map[string]any{"id": "", "tools": "mail_*"})), badMCP},
		{"relay_mcp tools absent", data(mcp(map[string]any{"id": "macmcp"})), badMCP},
		{"relay_mcp tools not a string", data(mcp(map[string]any{"id": "macmcp", "tools": 1})), badMCP},
		{"relay_mcp before fixture ids", data(del("relay_mcp"), addFixture("project:initech")), badMCP},
		{"unknown project", data(addFixture("project:initech")), "fixture project:initech does not resolve"},
		{"file of an unknown project", data(addFixture("file:initech/PROJECT.md")), "fixture file:initech/PROJECT.md does not resolve"},
		{"unknown kind", data(addFixture("folder:acme")), "fixture folder:acme does not resolve"},
		{"dot-dot", data(addFixture("file:acme/../globex/PROJECT.md")), "fixture file:acme/../globex/PROJECT.md does not resolve"},
		{"empty rel", data(addFixture("file:acme/")), "fixture file:acme/ does not resolve"},
		{"no rel", data(addFixture("file:acme")), "fixture file:acme does not resolve"},
		{"rel starts with a slash", data(addFixture("file:acme//etc/hosts")), "fixture file:acme//etc/hosts does not resolve"},
		{"first unresolved id in catalogue order", data(addFixture("project:zeta", "folder:acme")), "fixture project:zeta does not resolve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadWorld(worldMarker{WorldCheckout: c.dir(t), WorldRoot: "/w/root", WorldVersion: 1})
			if err == nil || err.Error() != c.want {
				t.Fatalf("loadWorld error = %v, want %s", err, c.want)
			}
		})
	}
	dir := writeWorld(t, nil)
	got, err := loadWorld(worldMarker{WorldCheckout: dir, WorldRoot: "/w/root", WorldVersion: 1})
	want := world{Version: 1, Root: "/w/root", Checkout: dir,
		Projects: map[string]worldProject{
			"acme":   {"acme", "Acme Corp", "work", "/w/root/Acme Corp"},
			"globex": {"globex", "Globex", "work", "/w/root/Globex"},
			"home":   {"home", "Home", "home", "/w/root/Home"},
		},
		Fixtures: map[string]bool{"project:acme": true, "project:globex": true, "project:home": true,
			"file:acme/PROJECT.md": true, "file:globex/PROJECT.md": true, "file:home/PROJECT.md": true,
			"file:acme/todo.txt": true, "file:acme/budget/q4-budget-draft.csv": true},
		MCP: worldMCP{ID: "macmcp", Tools: "mail_*"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("loadWorld(v1) = %+v, %v\nwant %+v", got, err, want)
	}
}

func TestWorldViewAdmitsOnlyDeclaredFixtures(t *testing.T) {
	w := loadTestWorld(t, dropFixtures("project:globex", "file:globex/PROJECT.md"))
	cases := []struct {
		name    string
		view    worldView
		lookup  func(worldView) (string, error)
		want    string
		wantErr string
	}{
		{"zero value", worldView{}, projectFolder("acme"), "", "undeclared fixture project:acme"},
		{"declared project", w.scoped([]string{"project:acme"}), projectFolder("acme"), "/w/root/Acme Corp", ""},
		{"other project", w.scoped([]string{"project:acme"}), projectFolder("home"), "", "undeclared fixture project:home"},
		{"project does not admit its file", w.scoped([]string{"project:acme"}), fileOf("acme", "PROJECT.md"), "", "undeclared fixture file:acme/PROJECT.md"},
		// /w/root does not exist: file answers the path without touching it.
		{"declared file", w.scoped([]string{"file:acme/PROJECT.md"}), fileOf("acme", "PROJECT.md"), "/w/root/Acme Corp/PROJECT.md", ""},
		{"declared nested file", w.scoped([]string{"file:acme/budget/q4-budget-draft.csv"}), fileOf("acme", "budget/q4-budget-draft.csv"), "/w/root/Acme Corp/budget/q4-budget-draft.csv", ""},
		{"other file", w.scoped([]string{"file:acme/PROJECT.md"}), fileOf("acme", "notes.md"), "", "undeclared fixture file:acme/notes.md"},
		{"declared project not in world", w.scoped([]string{"project:globex"}), projectFolder("globex"), "", "fixture project:globex is not in world v1"},
		{"declared file not in world", w.scoped([]string{"file:globex/PROJECT.md"}), fileOf("globex", "PROJECT.md"), "", "fixture file:globex/PROJECT.md is not in world v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.lookup(c.view)
			if gotErr := errText(err); got != c.want || gotErr != c.wantErr {
				t.Fatalf("lookup = %q, %q; want %q, %q", got, gotErr, c.want, c.wantErr)
			}
		})
	}
}

func TestRelayMCPNeedsNoDeclaration(t *testing.T) {
	want := worldMCP{ID: "macmcp", Tools: "mail_*"}
	if got := loadTestWorld(t, nil).scoped(nil).relayMCP(); got != want {
		t.Errorf("relayMCP() with no needs = %+v, want %+v", got, want)
	}
}

func projectFolder(key string) func(worldView) (string, error) {
	return func(v worldView) (string, error) { p, err := v.project(key); return p.Folder, err }
}

func fileOf(key, rel string) func(worldView) (string, error) {
	return func(v worldView) (string, error) { return v.file(key, rel) }
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestMissingFixtures(t *testing.T) {
	w := loadTestWorld(t, dropFixtures("project:globex", "file:globex/PROJECT.md", "project:home"))
	js := []journey{
		{ID: "a", Needs: []string{"project:acme"}},
		{ID: "b", Needs: []string{"project:globex", "project:acme", "file:globex/PROJECT.md"}},
		{ID: "c"},
		{ID: "d", Needs: []string{"project:home"}},
	}
	want := []string{"b needs project:globex, file:globex/PROJECT.md", "d needs project:home"}
	if got := missingFixtures(js, w); !slices.Equal(got, want) {
		t.Errorf("missingFixtures = %q, want %q", got, want)
	}
	if got := missingFixtures(js[:1], w); len(got) != 0 {
		t.Errorf("missingFixtures with every fixture present = %q", got)
	}
}

// contractNeeds is the Needs table the contract pins, by journey id.
func contractNeeds() map[string][]string {
	want := map[string][]string{"acme-sandbox-reach": {"project:acme", "project:globex", "file:acme/PROJECT.md", "file:globex/PROJECT.md"}}
	for _, id := range []string{"blank-model-refused", "oversized-launch-audit-capped", "acme-tools-through-bridge", "tool-call-audited", "file-plane-contained",
		"session-chat-lifecycle", "terminal-lifecycle", "model-list-and-completion", "session-chat-resume", "session-agent-state", "session-codex", "session-drop-in", "session-drop-in-tool-refused", "chief-of-staff-send", "cos-start", "cos-start-outside-root", "cos-read-only-profile", "cos-settings"} {
		want[id] = []string{"project:acme"}
	}
	for _, j := range slices.Concat(dialogNegJourneys(), noDoorJourneys()) {
		want[j.ID] = []string{"project:acme"}
	}
	return want
}

func TestJourneyNeedsMatchTheContractAndResolveInV1(t *testing.T) {
	want := contractNeeds()
	w := loadTestWorld(t, nil)
	seen := map[string]bool{}
	for _, j := range journeys {
		seen[j.ID] = true
		got, exp := slices.Sorted(slices.Values(j.Needs)), slices.Sorted(slices.Values(want[j.ID]))
		if !slices.Equal(got, exp) {
			t.Errorf("%s Needs = %q, want %q", j.ID, got, exp)
		}
		for _, id := range j.Needs {
			if !w.Fixtures[id] {
				t.Errorf("%s needs %s, which is not in the v1 catalogue", j.ID, id)
			}
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("no journey %s", id)
		}
	}
}

func TestNoFixtureNameLiteralInHarnessCode(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			for _, bad := range []string{"Acme Corp", "Globex", "DEVBOXWORLD_ROOT", "macmcp", "mail_*"} {
				if strings.Contains(s, bad) {
					t.Errorf("%s: string literal %s names %q", f, lit.Value, bad)
				}
			}
			return true
		})
	}
	if scanned < 5 || !slices.Contains(files, "main.go") || !slices.Contains(files, "journeys.go") {
		t.Fatalf("scanned %d harness files from %q; the guard found nothing to check", scanned, files)
	}
}
