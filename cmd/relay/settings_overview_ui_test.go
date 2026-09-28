package main

import (
	"fmt"
	"strings"
	"testing"
)

func renderOverviewRecent(t *testing.T, eventsJSON string) string {
	t.Helper()
	vm := newAppVM(t)
	return evalString(t, vm, `(function(){
		window.state.auditEvents = `+eventsJSON+`;
		return window.renderOverviewRecentToolCalls();
	})()`)
}

func overviewControlDecision(id string) string {
	return fmt.Sprintf(`{id:'%s', ts:'2026-09-01T10:00:00Z', event:'control_decision', outcome:'denied',
		mcp_id:'', actor:{kind:'control', auth:'token'}, method:'POST', route:'/api/projects'}`, id)
}

func overviewCallTool(id, tool, project string) string {
	return fmt.Sprintf(`{id:'%s', ts:'2026-09-01T09:00:00Z', event:'call_tool', outcome:'ok',
		mcp_id:'fsmcp', tool:'%s', actor:{kind:'project', project_id:'p1', project_name:'%s', auth:'token'}}`,
		id, tool, project)
}

func TestOverviewRecentToolCalls_SkipsNewerControlDecisions(t *testing.T) {
	events := "[" + strings.Join([]string{
		overviewControlDecision("cd1"),
		overviewControlDecision("cd2"),
		overviewControlDecision("cd3"),
		overviewCallTool("ct1", "read_file", "Acme"),
		overviewCallTool("ct2", "send_mail", "Devbox"),
	}, ",") + "]"
	html := renderOverviewRecent(t, events)

	for _, want := range []string{"read_file", "Acme", "send_mail", "Devbox"} {
		if !strings.Contains(html, want) {
			t.Errorf("recent tool calls is missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "control_decision") {
		t.Errorf("recent tool calls shows a control_decision row:\n%s", html)
	}
}

func TestOverviewRecentToolCalls_ShowsSixNewestCallToolRows(t *testing.T) {
	tools := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	var evs []string
	for i, tool := range tools {
		evs = append(evs, overviewControlDecision(fmt.Sprintf("cd%d", i)))
		evs = append(evs, overviewCallTool(fmt.Sprintf("ct%d", i), tool, "Acme"))
	}
	html := renderOverviewRecent(t, "["+strings.Join(evs, ",")+"]")

	if n := strings.Count(html, `class="ov-recent-row"`); n != 6 {
		t.Errorf("rendered %d rows, want 6:\n%s", n, html)
	}
	if strings.Contains(html, "control_decision") {
		t.Errorf("recent tool calls shows a control_decision row:\n%s", html)
	}
	last := -1
	for _, tool := range tools[:6] {
		at := strings.Index(html, tool)
		if at < 0 {
			t.Errorf("recent tool calls is missing %q:\n%s", tool, html)
			continue
		}
		if at < last {
			t.Errorf("%q rendered out of stored order:\n%s", tool, html)
		}
		last = at
	}
	for _, tool := range tools[6:] {
		if strings.Contains(html, tool) {
			t.Errorf("older call %q was rendered past the six-row cap:\n%s", tool, html)
		}
	}
}

func TestOverviewRecentToolCalls_OnlyControlDecisionsShowsEmptyState(t *testing.T) {
	events := "[" + overviewControlDecision("cd1") + "," + overviewControlDecision("cd2") + "]"
	html := renderOverviewRecent(t, events)

	if !strings.Contains(html, "No tool calls recorded yet.") {
		t.Errorf("empty state is missing when only control decisions exist:\n%s", html)
	}
	if strings.Contains(html, "ov-recent-row") {
		t.Errorf("a row was rendered when no call_tool events exist:\n%s", html)
	}
}
