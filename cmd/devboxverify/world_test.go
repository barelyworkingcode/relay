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
  "projects": [
    {"key": "acme",   "name": "Acme Corp", "mode": "work", "color": "#1F6FD1", "relay_mailboxes": ["INBOX", "Archive", "Clients"]},
    {"key": "globex", "name": "Globex",    "mode": "work", "color": "#2E9E5B", "relay_mailboxes": ["INBOX", "Archive"]},
    {"key": "home",   "name": "Home",      "mode": "home", "color": "#D9822B", "relay_mailboxes": ["INBOX", "Archive"]}
  ],
  "fixtures": ["project:acme", "project:globex", "project:home",
               "file:acme/PROJECT.md", "file:globex/PROJECT.md", "file:home/PROJECT.md"]
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
		{"lstat fails", func(t *testing.T, d string) string {
			return filepath.Join(writeMode(t, filepath.Join(d, "f"), "", 0o600), "machine.json")
		}, yes, refusal("marker is not readable")},
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
	addFixture := func(id string) func(map[string]any) {
		return func(m map[string]any) { m["fixtures"] = append(m["fixtures"].([]any), id) }
	}
	cases := []struct {
		name string
		dir  func(*testing.T) string
		want string
	}{
		{"no world.json", func(t *testing.T) string { return t.TempDir() }, "world data is not readable"},
		{"not JSON", func(t *testing.T) string {
			d := writeWorld(t, nil)
			writeMode(t, filepath.Join(d, "data", "world.json"), "{", 0o600)
			return d
		}, "world data is not valid JSON"},
		{"version absent", func(t *testing.T) string { return writeWorld(t, func(m map[string]any) { delete(m, "world_version") }) }, "world data world_version is not a positive integer"},
		{"version 0", func(t *testing.T) string { return writeWorld(t, set("world_version", 0)) }, "world data world_version is not a positive integer"},
		{"version string", func(t *testing.T) string { return writeWorld(t, set("world_version", "1")) }, "world data world_version is not a positive integer"},
		{"fixtures absent", func(t *testing.T) string { return writeWorld(t, func(m map[string]any) { delete(m, "fixtures") }) }, "world data fixtures is not a list of strings"},
		{"fixtures a string", func(t *testing.T) string { return writeWorld(t, set("fixtures", "project:acme")) }, "world data fixtures is not a list of strings"},
		{"fixtures not strings", func(t *testing.T) string { return writeWorld(t, set("fixtures", []any{1})) }, "world data fixtures is not a list of strings"},
		{"project without a name", func(t *testing.T) string {
			return writeWorld(t, func(m map[string]any) { delete(m["projects"].([]any)[1].(map[string]any), "name") })
		}, "world data project 1 is malformed"},
		{"unknown project", func(t *testing.T) string { return writeWorld(t, addFixture("project:initech")) }, "world data fixture project:initech does not resolve"},
		{"file of an unknown project", func(t *testing.T) string { return writeWorld(t, addFixture("file:initech/PROJECT.md")) }, "world data fixture file:initech/PROJECT.md does not resolve"},
		{"unknown kind", func(t *testing.T) string { return writeWorld(t, addFixture("folder:acme")) }, "world data fixture folder:acme does not resolve"},
		{"dot-dot", func(t *testing.T) string { return writeWorld(t, addFixture("file:acme/../globex/PROJECT.md")) }, "world data fixture file:acme/../globex/PROJECT.md does not resolve"},
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
			"file:acme/PROJECT.md": true, "file:globex/PROJECT.md": true, "file:home/PROJECT.md": true},
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
		{"declared file", w.scoped([]string{"file:acme/PROJECT.md"}), fileOf("acme", "PROJECT.md"), "/w/root/Acme Corp/PROJECT.md", ""},
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
	for _, id := range []string{"blank-model-refused", "oversized-launch-audit-capped", "acme-tools-through-bridge", "tool-call-audited",
		"session-chat-lifecycle", "terminal-lifecycle", "model-list-and-completion", "session-chat-resume"} {
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
			for _, bad := range []string{"Acme Corp", "Globex", "DEVBOXWORLD_ROOT"} {
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
