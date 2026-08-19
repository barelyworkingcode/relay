package main

// Tool Calls tab (the audit-log viewer) under goja, using the same bundle +
// DOM shim as settings_bundle_test.go. The tab is the primary way anyone will
// actually read the audit log, so the tests pin the things a reviewer depends
// on: that refusals are visually distinct from successes, that the caller and
// project attribution reach the screen, and that a truncated or disabled state
// says so rather than looking like a complete record.

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// seedAuditVM loads the app bundle and puts the given events on the Tool Calls
// tab. Returns the VM with state.page already switched to 'audit'.
func seedAuditVM(t *testing.T, eventsJSON, statusJSON string) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.state.page = 'audit';
		window.state.auditLoaded = true;
		window.state.auditEvents = ` + eventsJSON + `;
		window.state.auditStatus = ` + statusJSON + `;
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding audit state: %v", err)
	}
	return vm
}

const auditEventFixture = `[{
	id: 'ev1', ts: '2026-08-19T10:22:31.412Z', dur_ms: 412, event: 'call_tool',
	actor: { kind:'project', project_id:'p1', project_name:'relay', auth:'token',
	         pid: 4242, proc:'relay', parent:'claude' },
	mcp_id: 'fsmcp', tool: 'read_file', args: {path:'/tmp/notes.md'},
	args_bytes: 24, outcome: 'ok', result_bytes: 2048
}, {
	id: 'ev2', ts: '2026-08-19T10:22:35.000Z', dur_ms: 1, event: 'call_tool',
	actor: { kind:'project', project_id:'p2', project_name:'sandbox', auth:'cwd',
	         cwd:'/Users/x/sandbox', pid: 99 },
	mcp_id: 'macmcp', tool: 'send_mail', outcome: 'denied',
	error: "access denied: tool 'send_mail' is disabled for this token"
}]`

const auditStatusOn = `{enabled:true, path:'/tmp/toolcalls.jsonl', dropped:0, recorded:2, log_args:true, log_lists:false}`

func TestAuditTab_RendersEventsWithAttribution(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	html := evalString(t, vm, `window.renderAudit()`)

	for _, want := range []string{
		"Tool Calls", // page heading
		"read_file",  // tool name
		"send_mail",  // the denied tool
		"relay",      // project name
		"sandbox",    // second project
		"fsmcp",      // owning MCP
		"claude",     // the parent process that actually asked
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered Tool Calls tab is missing %q", want)
		}
	}
}

// A denial must not look like a success at a glance — that distinction is the
// whole reason the tab exists.
func TestAuditTab_OutcomesAreVisuallyDistinct(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	html := evalString(t, vm, `window.renderAudit()`)

	if !strings.Contains(html, "audit-ok") {
		t.Error("successful call did not get the ok pill class")
	}
	if !strings.Contains(html, "audit-denied") {
		t.Error("denied call did not get the denied pill class")
	}
}

// Typing in the filter box must not cross the IPC boundary — it filters what is
// already loaded, so the tab stays responsive on every keystroke.
func TestAuditTab_TextFilterIsLocal(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)

	got := evalString(t, vm, `(function(){
		window.state.auditFilter.text = 'send_mail';
		var rows = window.auditVisible();
		return rows.length + ':' + rows[0].tool;
	})()`)
	if got != "1:send_mail" {
		t.Errorf("text filter returned %q, want 1:send_mail", got)
	}

	// The filter searches the caller and the arguments too, not just the tool.
	got = evalString(t, vm, `(function(){
		window.state.auditFilter.text = 'notes.md';
		return window.auditVisible().length;
	})()`)
	if got != "1" {
		t.Errorf("filtering by an argument value returned %q, want 1", got)
	}

	got = evalString(t, vm, `(function(){
		window.state.auditFilter.text = 'CLAUDE';
		return window.auditVisible().length;
	})()`)
	if got != "1" {
		t.Errorf("caller filter should be case-insensitive, got %q", got)
	}
}

func TestAuditTab_ServerSideFiltersApplyLocallyToo(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	got := evalString(t, vm, `(function(){
		window.state.auditFilter.outcome = 'denied';
		var rows = window.auditVisible();
		return rows.length + ':' + rows[0].id;
	})()`)
	if got != "1:ev2" {
		t.Errorf("outcome filter returned %q, want 1:ev2", got)
	}
}

// Expanding a row is where the full record lives: the arguments, the working
// directory behind a directory-auth grant, the caller pid.
func TestAuditTab_ExpandedRowShowsFullRecord(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	html := evalString(t, vm, `(function(){
		window.state.auditExpanded['ev2'] = true;
		return window.renderAudit();
	})()`)

	for _, want := range []string{"/Users/x/sandbox", "Working dir", "Caller pid", "disabled for this token"} {
		if !strings.Contains(html, want) {
			t.Errorf("expanded row is missing %q", want)
		}
	}
}

func TestAuditTab_TruncatedArgsAreLabelled(t *testing.T) {
	events := `[{
		id:'ev3', ts:'2026-08-19T10:00:00Z', event:'call_tool', outcome:'ok',
		actor:{kind:'project', project_name:'relay', auth:'token'},
		mcp_id:'fsmcp', tool:'write_file',
		args:'{"blob":"xxxxx', args_bytes: 90210, args_truncated: true
	}]`
	vm := seedAuditVM(t, events, auditStatusOn)
	html := evalString(t, vm, `(function(){
		window.state.auditExpanded['ev3'] = true;
		return window.renderAudit();
	})()`)

	// A truncated record that looks complete is worse than no record.
	if !strings.Contains(html, "truncated") || !strings.Contains(html, "90210") {
		t.Errorf("truncated arguments were not labelled as such:\n%s", html)
	}
}

// Disabled auditing must be stated outright. An empty table would otherwise
// read as "nothing happened".
func TestAuditTab_DisabledStateIsExplicit(t *testing.T) {
	vm := seedAuditVM(t, `[]`, `{enabled:false, path:'', dropped:0, recorded:0}`)
	html := evalString(t, vm, `window.renderAudit()`)

	if !strings.Contains(html, "Auditing is disabled") {
		t.Errorf("disabled auditing was not surfaced:\n%s", html)
	}
	if strings.Contains(html, "<table") {
		t.Error("a table was rendered while auditing is disabled")
	}
}

// Dropped events mean the log is incomplete, which the reader has to be told.
func TestAuditTab_DropCountIsSurfaced(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, `{enabled:true, path:'/tmp/x', dropped:7, recorded:2}`)
	html := evalString(t, vm, `window.renderAudit()`)

	if !strings.Contains(html, "7 event(s) were dropped") || !strings.Contains(html, "incomplete") {
		t.Errorf("drop count was not surfaced as a warning:\n%s", html)
	}
}

func TestAuditTab_EmptyStateDistinguishesLoadingFromNoMatches(t *testing.T) {
	vm := seedAuditVM(t, `[]`, auditStatusOn)
	if html := evalString(t, vm, `window.renderAudit()`); !strings.Contains(html, "No tool calls match") {
		t.Errorf("loaded-but-empty state is wrong:\n%s", html)
	}

	vm2 := newAppVM(t)
	html := evalString(t, vm2, `(function(){
		window.state.page='audit'; window.state.auditLoaded=false;
		window.state.auditStatus=`+auditStatusOn+`;
		return window.renderAudit();
	})()`)
	if !strings.Contains(html, "Loading") {
		t.Errorf("unloaded state should say it is loading:\n%s", html)
	}
}

// A live event appends without a full repaint, so whatever the user is typing
// in the filter box survives an inbound tool call.
func TestAuditTab_LiveEventPrepends(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	got := evalString(t, vm, `(function(){
		window.onAuditEvent({id:'ev-live', ts:'2026-08-19T11:00:00Z', event:'call_tool',
			outcome:'ok', tool:'live_tool', mcp_id:'fsmcp',
			actor:{kind:'project', project_name:'relay', auth:'token'}});
		return window.state.auditEvents.length + ':' + window.state.auditEvents[0].id;
	})()`)
	if got != "3:ev-live" {
		t.Errorf("live event handling = %q, want 3:ev-live", got)
	}
}

// With Follow off, the view must stay where the reader put it.
func TestAuditTab_FollowOffIgnoresLiveEvents(t *testing.T) {
	vm := seedAuditVM(t, auditEventFixture, auditStatusOn)
	got := evalString(t, vm, `(function(){
		window.toggleAuditFollow(false);
		window.onAuditEvent({id:'ev-live', event:'call_tool', outcome:'ok', tool:'t', actor:{}});
		return window.state.auditEvents.length;
	})()`)
	if got != "2" {
		t.Errorf("live event was appended with Follow off: length = %q", got)
	}
}

// Switching to the tab must load it: it is the one tab not seeded by the
// initial payload.
func TestAuditTab_ShowPageTriggersInitialLoad(t *testing.T) {
	vm := newAppVM(t)
	// Intercept at the WebKit message handler rather than window.ipc: the
	// bundled module calls its own module-scoped ipc(), so replacing the global
	// would not see the message the tab actually sends.
	got := evalString(t, vm, `(function(){
		var sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(msg){ sent.push(msg); } } } };
		window.showPage('audit');
		return sent.filter(function(m){ return m.indexOf('query_audit') >= 0; }).length;
	})()`)
	if got != "1" {
		t.Errorf("showPage('audit') sent %q query_audit messages, want 1", got)
	}
}
