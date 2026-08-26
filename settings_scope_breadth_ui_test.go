package main

// Issue #41 on the operator's screen. The profile card already printed the
// real value — and "/" is one character, rendered inline beside everything
// else, which is exactly how an over-broad grant survived review. These tests
// are about whether the screen says what the grant reaches, not only what it
// is set to.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// mustRun evaluates a statement for its side effect.
func mustRun(t *testing.T, vm *goja.Runtime, src string) {
	t.Helper()
	if _, err := vm.RunString(src); err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
}

// jsQuote renders a Go string as a JS string literal.
func jsQuote(s string) string { return strconv.Quote(s) }

// fsScopeFieldsFixture is fsMCP's declaration as ScopeFieldView projects it.
const fsScopeFieldsFixture = `{
	fsmcp: [
		{name:'allowed_dirs', type:'array', item_type:'string', description:'Directories this client may read, search and modify within', source:'operator'}
	]
}`

const fsMcpsFixture = `[{id:'fsmcp', display_name:'fsMCP'}]`

// breadthProjectsFixture is issue #41's comparison as two rows: the probe
// profile that read /etc/passwd, and a correctly scoped one beside it.
const breadthProjectsFixture = `[
	{id:'p_root', name:'Probe', kind:'remote', path:'', allowed_mcp_ids:['fsmcp'], allowed_models:[],
	 allowed_tools:{fsmcp:['fs_*']}, access:{fsmcp:'write'},
	 context:{fsmcp:{allowed_dirs:['/']}}, disabled_tools:{}},
	{id:'p_folder', name:'Bounded', kind:'remote', path:'', allowed_mcp_ids:['fsmcp'], allowed_models:[],
	 allowed_tools:{fsmcp:['fs_*']}, access:{fsmcp:'write'},
	 context:{fsmcp:{allowed_dirs:['/Users/admin/source/project']}}, disabled_tools:{}},
	{id:'p_home', name:'Home', kind:'remote', path:'', allowed_mcp_ids:['fsmcp'], allowed_models:[],
	 allowed_tools:{fsmcp:['fs_*']}, access:{fsmcp:'write'},
	 context:{fsmcp:{allowed_dirs:['/Users/admin']}}, disabled_tools:{}}
]`

func seedBreadthVM(t *testing.T, editingID string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.__sent = [];
		window.__confirms = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.externalMcps = ` + fsMcpsFixture + `;
		window.state.mcpScopeFields = ` + fsScopeFieldsFixture + `;
		window.state.projects = ` + breadthProjectsFixture + `;
		window.state.editingProjectId = null;
		window.state.projectForm = null;
		` + editingScript(editingID) + `
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding breadth state: %v", err)
	}
	return vm
}

// TestProjectList_AFilesystemRootIsNotJustAValue is the card half of issue
// #41. On main every one of these three rows renders its value inline and
// nothing else, so the row for a grant of "/" and the row for a grant of one
// project folder differ by six characters in a monospace span.
func TestProjectList_AFilesystemRootIsNotJustAValue(t *testing.T) {
	vm := seedBreadthVM(t, "")
	html := evalString(t, vm, `window.renderProjects()`)

	// The coordinates stay. `disclose` governs what reaches the CLIENT and
	// must never govern what relay shows the person who typed the grant.
	if !strings.Contains(html, "allowed_dirs: /Users/admin/source/project") {
		t.Errorf("the card stopped printing the real value:\n%s", html)
	}
	if !strings.Contains(html, "allowed_dirs is unrestricted (the whole filesystem)") {
		t.Errorf("a grant of \"/\" is not called unrestricted anywhere on the card:\n%s", html)
	}
	if !strings.Contains(html, "allowed_dirs is a whole home directory") {
		t.Errorf("a grant of a home directory carries no warning:\n%s", html)
	}
	// Exactly two warnings: the bounded row must not acquire one.
	if n := strings.Count(html, "proj-auth-unrestricted"); n != 2 {
		t.Errorf("expected 2 breadth warnings across three rows, got %d:\n%s", n, html)
	}
}

// TestProjectEditor_WarnsWhileTheValueIsBeingTyped puts the finding where the
// decision is made. The list row catches a grant on review; this catches it
// before it is saved.
func TestProjectEditor_WarnsWhileTheValueIsBeingTyped(t *testing.T) {
	vm := seedBreadthVM(t, "p_root")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, "proj-scope-unrestricted") {
		t.Errorf("the editor does not warn on a filesystem root:\n%s", html)
	}
	if !strings.Contains(html, "unrestricted (the whole filesystem)") {
		t.Errorf("the editor's warning does not name the finding:\n%s", html)
	}

	bounded := evalString(t, seedBreadthVM(t, "p_folder"), `window.renderProjectForm()`)
	if strings.Contains(bounded, "proj-scope-unrestricted") {
		t.Errorf("a one-folder grant was warned about in the editor:\n%s", bounded)
	}
}

// TestSaveProject_AsksBeforeStoringAFilesystemRoot is issue #41's priority 3.
// fsMCP documents `--allowed-dir /` as a deliberate opt-out that must be
// spelled out explicitly; on a CLI, typing it IS the spelling out, and in a
// text box it is not. This is the UI's equivalent — a confirmation, never a
// refusal, because a grant of "/" is legal and an operator who means it can
// mean it.
func TestSaveProject_AsksBeforeStoringAFilesystemRoot(t *testing.T) {
	t.Run("declining stops the save", func(t *testing.T) {
		vm := seedBreadthVM(t, "p_root")
		mustRun(t, vm, `window.confirm = function(msg){ window.__confirms.push(msg); return false; };`)
		mustRun(t, vm, `window.saveProjectForm();`)
		if n := evalString(t, vm, `String(window.__confirms.length)`); n != "1" {
			t.Fatalf("expected one confirmation, got %s", n)
		}
		if msg := evalString(t, vm, `window.__confirms[0]`); !strings.Contains(msg, "unrestricted (the whole filesystem)") ||
			!strings.Contains(msg, "allowed_dirs") || !strings.Contains(msg, "fsmcp") {
			t.Errorf("the confirmation does not name the MCP, the field and the finding: %q", msg)
		}
		if n := evalString(t, vm, `String(window.__sent.length)`); n != "0" {
			t.Errorf("the save went ahead after the operator declined (%s messages sent)", n)
		}
	})

	t.Run("accepting saves exactly as before", func(t *testing.T) {
		vm := seedBreadthVM(t, "p_root")
		mustRun(t, vm, `window.confirm = function(){ return true; };`)
		mustRun(t, vm, `window.saveProjectForm();`)
		if n := evalString(t, vm, `String(window.__sent.length)`); n != "1" {
			t.Fatalf("accepting did not save (%s messages sent)", n)
		}
	})

	t.Run("a bounded grant is never asked about", func(t *testing.T) {
		vm := seedBreadthVM(t, "p_folder")
		mustRun(t, vm, `window.confirm = function(msg){ window.__confirms.push(msg); return true; };`)
		mustRun(t, vm, `window.saveProjectForm();`)
		if n := evalString(t, vm, `String(window.__confirms.length)`); n != "0" {
			t.Errorf("a one-folder grant produced %s confirmation(s)", n)
		}
		if n := evalString(t, vm, `String(window.__sent.length)`); n != "1" {
			t.Errorf("the ordinary save path changed (%s messages sent)", n)
		}
	})
}

// TestScopeBreadthJs_MirrorsGo pins the two implementations against each
// other. They are separate on purpose — one runs in Go and one in a WKWebView
// — and a rule that disagreed between them would show an operator a warning
// the CLI does not, or the reverse.
func TestScopeBreadthJs_MirrorsGo(t *testing.T) {
	vm := newAppVM(t)
	for _, entry := range []string{
		"/", "//", "/..", "/Users/admin/../..", "  /  ", "~", "~/",
		"/Users", "/Users/admin", "/Users/admin/", "/home/someone",
		"/Users/admin/source/project", "/etc", "/tmp/work", "relative/path", "Bob", "",
	} {
		want := scopeEntryBreadth(entry)
		got := evalString(t, vm, `window.scopeEntryBreadth(`+jsQuote(entry)+`)`)
		if got != want {
			t.Errorf("scopeEntryBreadth(%q): js=%q go=%q", entry, got, want)
		}
	}
}
