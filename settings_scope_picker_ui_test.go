package main

// The enumeration picker, under goja (ADR-011 decision 6).
//
// Decision 6 exists because of the ADR's second constraint: typing "INBOX" by
// hand is the error-prone step, and under decision 4 a typo fails CLOSED — the
// agent silently gets nothing, which is safe and baffling. A picker removes the
// typo. What it must not do is introduce a worse failure in its place, and
// there are two of those:
//
//   - rendering a FAILED call as an empty list, which tells an operator there
//     are no mailboxes on a machine full of them;
//   - dropping a stored value the MCP no longer offers, which silently widens
//     or narrows a profile because something was renamed on the host.
//
// Most of this file is about those two.

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// The same worked example the rest of the suite uses, plus one operator field
// the MCP does NOT declare enumerable — the case that must keep its text box.
const pickerFieldsFixture = `{
	macmcp: [
		{name:'mail_accounts', type:'array', item_type:'string', description:'Mail accounts this client may read from or send as', source:'operator', applies_to:['mail_*'], enumerable:true},
		{name:'mail_mailboxes', type:'array', item_type:'string', description:'Mailbox paths within those accounts this client may reach', source:'operator', applies_to:['mail_*'], enumerable:true, depends_on:['mail_accounts']},
		{name:'mail_note', type:'string', description:'A note the MCP will not list values for', source:'operator', applies_to:['mail_*']}
	]
}`

const pickerProjectsFixture = `[
	{id:'p_bob', name:'Hermes — Bob INBOX', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[],
	 allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'read'},
	 context:{macmcp:{mail_accounts:['Bob'], mail_mailboxes:['INBOX','Projects/Archive']}}, disabled_tools:{}}
]`

// seedPickerVM opens p_bob in the editor with the picker fixture loaded.
func seedPickerVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := newAppVM(t)
	script := `(function(){
		window.__sent = [];
		window.webkit = { messageHandlers: { ipc: { postMessage: function(m){ window.__sent.push(String(m)); } } } };
		window.state.page = 'projects';
		window.state.externalMcps = [{id:'macmcp', display_name:'macMCP'}];
		window.state.mcpScopeFields = ` + pickerFieldsFixture + `;
		window.state.projects = ` + pickerProjectsFixture + `;
		window.editProject('p_bob');
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding picker state: %v", err)
	}
	return vm
}

// answer delivers one ContextEnumResult the way relay does, then re-renders.
func answer(t *testing.T, vm *goja.Runtime, json string) string {
	t.Helper()
	return evalString(t, vm, `(function(){
		window.onScopeFieldEnumerated(`+json+`);
		return window.renderProjectForm();
	})()`)
}

func openField(t *testing.T, vm *goja.Runtime, field string) string {
	t.Helper()
	return evalString(t, vm, `(function(){
		window.toggleScopeFieldPicker('macmcp', '`+field+`');
		return window.renderProjectForm();
	})()`)
}

func sentMessages(t *testing.T, vm *goja.Runtime) string {
	t.Helper()
	// Joined rather than JSON.stringify'd: these are already JSON strings, and
	// stringifying the array again escapes every quote in them.
	return evalString(t, vm, `window.__sent.join("\n")`)
}

// ---------------------------------------------------------------------------
// When it is asked for
// ---------------------------------------------------------------------------

// Enumeration is a live call into another process. It happens when an operator
// OPENS a control — not on a paint, not on a keystroke — and it is cached per
// (mcp, field, dependency values) for the life of the form.
func TestScopePicker_FetchesOnOpenAndCaches(t *testing.T) {
	vm := seedPickerVM(t)

	if sent := sentMessages(t, vm); strings.Contains(sent, "enumerate_scope_field") {
		t.Fatalf("opening the editor enumerated something: %s", sent)
	}

	html := openField(t, vm, "mail_accounts")
	sent := sentMessages(t, vm)
	if !strings.Contains(sent, `"type":"enumerate_scope_field"`) || !strings.Contains(sent, `"field":"mail_accounts"`) {
		t.Fatalf("opening the picker did not ask for values: %s", sent)
	}
	if strings.Count(sent, "enumerate_scope_field") != 1 {
		t.Errorf("one open, %d requests: %s", strings.Count(sent, "enumerate_scope_field"), sent)
	}
	if !strings.Contains(html, "Listing values from macmcp") {
		t.Errorf("the wait is not shown as a wait\n%s", html)
	}

	answer(t, vm, `{mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[{value:'Alice',label:'Alice'},{value:'Bob',label:'Bob'}]}`)

	// Closing and reopening does not re-ask: the answer is cached for the life
	// of the form.
	openField(t, vm, "mail_accounts")
	openField(t, vm, "mail_accounts")
	if n := strings.Count(sentMessages(t, vm), "enumerate_scope_field"); n != 1 {
		t.Errorf("re-opening a cached field made %d requests, want 1", n)
	}
}

// The values are real values, the stored one is ticked, and ticking another
// writes it through the same storage the text box uses — so harvest, clearing
// and the "needs a scope value" banner all keep working untouched.
func TestScopePicker_ChoosingWritesTheStoredValue(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_accounts")
	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[{value:'Alice',label:'Alice'},{value:'Bob',label:'Bob (work)'}]}`)

	if !strings.Contains(html, "Bob (work)") {
		t.Errorf("the MCP's own label is not shown\n%s", html)
	}
	// Bob is stored, so Bob is ticked and Alice is not.
	got := evalString(t, vm, `(function(){
		var out = [];
		for (var i = 0; i < window.state._scopeBind.length; i++) out.push(window.state._scopeBind[i].value);
		return JSON.stringify(out);
	})()`)
	if got != `["Alice","Bob"]` {
		t.Fatalf("bindings = %s, want both offered values", got)
	}
	if strings.Count(html, " checked ") != 1 {
		t.Errorf("want exactly the stored value ticked\n%s", html)
	}

	// Tick Alice: the stored value becomes both, through the same text the
	// box edits.
	payload := evalString(t, vm, `(function(){
		window.toggleProjScopeValueAt(0, true);
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":["Bob","Alice"]`) {
		t.Fatalf("ticking a value did not reach the payload: %s", payload)
	}

	// Untick Bob: it goes, and nothing else does.
	payload = evalString(t, vm, `(function(){
		window.toggleProjScopeValueAt(1, false);
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":["Alice"]`) {
		t.Fatalf("unticking removed the wrong thing: %s", payload)
	}
}

// ---------------------------------------------------------------------------
// The behaviour that matters most
// ---------------------------------------------------------------------------

// A stored value the MCP no longer offers stays VISIBLE and stays SELECTED,
// flagged as unrecognised. An account renamed on the host must not quietly
// widen or narrow a profile by disappearing from a form, and a value that
// vanishes from the editor is a value the next save deletes.
func TestScopePicker_AStoredValueNoLongerOfferedSurvives(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_mailboxes")
	// The MCP offers INBOX and something else; "Projects/Archive" — which this
	// profile is confined to — is gone.
	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'INBOX',label:'INBOX'},{value:'Sent',label:'Sent'}]}`)

	if !strings.Contains(html, "Projects/Archive") {
		t.Fatalf("a stored value the MCP no longer offers vanished from the form\n%s", html)
	}
	if !strings.Contains(html, "unrecognised") {
		t.Errorf("the value is shown but not flagged as one the MCP does not offer\n%s", html)
	}
	if !strings.Contains(html, "does not offer Projects/Archive") {
		t.Errorf("the flag does not name the value\n%s", html)
	}
	// It is ticked — it is in force — and it survives a save untouched.
	payload := evalString(t, vm, `(function(){
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_mailboxes":["INBOX","Projects/Archive"]`) {
		t.Fatalf("an unrecognised value did not survive the save: %s", payload)
	}
	// And it is removable, deliberately, by unticking it.
	payload = evalString(t, vm, `(function(){
		var idx = -1;
		for (var i = 0; i < window.state._scopeBind.length; i++)
			if (window.state._scopeBind[i].value === 'Projects/Archive') idx = i;
		window.toggleProjScopeValueAt(idx, false);
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if strings.Contains(payload, "Projects/Archive") {
		t.Fatalf("unticking an unrecognised value did not remove it: %s", payload)
	}
}

// A FAILED call is never rendered as an empty list. Each degraded case keeps
// text entry so an operator is never blocked, and each says something
// different, because the operator's next action differs in each.
func TestScopePicker_DegradedCases(t *testing.T) {
	cases := []struct {
		name       string
		clearFirst bool // no stored value, so an empty answer really is empty
		answer     string
		wantText   []string
		wantNoText []string
		wantBox    bool
	}{
		{
			name:   "the MCP does not implement enumeration (-32601)",
			answer: `{mcp_id:'macmcp', field:'mail_accounts', status:'unsupported', values:null, error:'macmcp does not implement context/enumerate'}`,
			// Degrades to the text box, silently: no error, no retry button,
			// nothing for the operator to act on beyond typing the value.
			wantText:   []string{"cannot list this field's values", "setProjScopeText('macmcp', 'mail_accounts', this.value)"},
			wantNoText: []string{"Try again", "bug in relay"},
			wantBox:    true,
		},
		{
			name:   "relay asked for a field the MCP will not enumerate (-32602)",
			answer: `{mcp_id:'macmcp', field:'mail_accounts', status:'invalid_field', values:null, error:'no enumerable field named mail_accounts'}`,
			// Surfaced, not degraded away: it is a relay bug and saying so is
			// the only thing that gets it fixed.
			wantText:   []string{"bug in relay", "no enumerable field named mail_accounts"},
			wantNoText: []string{"Try again"},
			wantBox:    true,
		},
		{
			name:   "the MCP could not answer right now",
			answer: `{mcp_id:'macmcp', field:'mail_accounts', status:'unavailable', values:null, error:'could not read accounts: Mail timed out'}`,
			wantText: []string{
				"Could not list values from macmcp",
				"could not read accounts: Mail timed out",
				"This is not an empty list",
				"Try again",
			},
			wantBox: true,
		},
		{
			name:       "the MCP answered, and there really are none",
			clearFirst: true,
			answer:     `{mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[]}`,
			// The one case where "there are none" is the truth. No error, no
			// retry, and no text box: the MCP answered.
			wantText:   []string{"offers no values for this field", "not a failure"},
			wantNoText: []string{"Try again", "Could not list"},
			wantBox:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vm := seedPickerVM(t)
			if c.clearFirst {
				evalString(t, vm, `(function(){ window.setProjScopeText('macmcp', 'mail_accounts', ''); return ''; })()`)
			}
			openField(t, vm, "mail_accounts")
			html := answer(t, vm, c.answer)

			for _, want := range c.wantText {
				if !strings.Contains(html, want) {
					t.Errorf("missing %q\n%s", want, html)
				}
			}
			for _, unwanted := range c.wantNoText {
				if strings.Contains(html, unwanted) {
					t.Errorf("unexpectedly present: %q\n%s", unwanted, html)
				}
			}
			// A failure NEVER renders as a list of choices — that is the
			// rendering that means "there are none".
			if !c.wantBox && strings.Contains(html, "proj-scope-choices") {
				t.Errorf("an empty answer drew an empty choice list\n%s", html)
			}
			if c.wantBox {
				if !strings.Contains(html, "setProjScopeText('macmcp', 'mail_accounts'") {
					t.Errorf("a degraded picker left the operator with no way to set a value\n%s", html)
				}
				if strings.Contains(html, "proj-scope-choices") {
					t.Errorf("a failed call drew choices\n%s", html)
				}
			}
			// The stored value is still on screen and still stored, whatever
			// went wrong — in the picker's summary, or in the text box the
			// degraded cases fall back to. A confinement must never vanish
			// because the MCP that lists its values would not answer.
			if !c.clearFirst && !strings.Contains(html, "Bob") {
				t.Errorf("the stored value disappeared behind a failure\n%s", html)
			}
		})
	}
}

// -32601 is a fact about the MCP, not about one field: every field of it
// degrades, and nothing asks again. That is "silently and permanently".
func TestScopePicker_UnsupportedIsPermanentForTheWholeMcp(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_accounts")
	answer(t, vm, `{mcp_id:'macmcp', field:'mail_accounts', status:'unsupported', values:null}`)

	before := strings.Count(sentMessages(t, vm), "enumerate_scope_field")
	html := openField(t, vm, "mail_mailboxes")
	if n := strings.Count(sentMessages(t, vm), "enumerate_scope_field"); n != before {
		t.Errorf("relay re-asked an MCP that already said it does not enumerate (%d requests, was %d)", n, before)
	}
	if strings.Contains(html, "Choose values") {
		t.Errorf("a picker was still offered for an MCP that cannot list values\n%s", html)
	}
	if !strings.Contains(html, "setProjScopeText('macmcp', 'mail_mailboxes', this.value)") {
		t.Errorf("the other field did not fall back to text entry\n%s", html)
	}
}

// ---------------------------------------------------------------------------
// Dependency order
// ---------------------------------------------------------------------------

// mail_mailboxes is read WITHIN the chosen accounts, so the request carries
// them — and changing the account choice re-reads the list rather than leaving
// a stale one on screen under a new account's name.
func TestScopePicker_DependencyOrder(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_mailboxes")

	sent := sentMessages(t, vm)
	if !strings.Contains(sent, `"values":{"mail_accounts":["Bob"]}`) {
		t.Fatalf("the mailbox list was not read within the chosen account: %s", sent)
	}
	answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'INBOX'},{value:'Projects/Archive'}]}`)

	// Now the account choice changes. The old list is not the answer to the
	// new question, so it must not stay on screen as if it were.
	html := evalString(t, vm, `(function(){
		window.setProjScopeText('macmcp', 'mail_accounts', 'Alice');
		window.refreshDependentScopeFields('macmcp', 'mail_accounts');
		return window.renderProjectForm();
	})()`)
	sent = sentMessages(t, vm)
	if !strings.Contains(sent, `"values":{"mail_accounts":["Alice"]}`) {
		t.Fatalf("changing the account did not re-read the mailbox list: %s", sent)
	}
	if !strings.Contains(html, "Listing values from macmcp") {
		t.Errorf("the stale list stayed on screen while the new one was read\n%s", html)
	}
}

// An UNCHOSEN dependency lists across everything rather than showing an empty
// picker. That is the state the control opens in, so getting it wrong means an
// operator's first sight of the feature is an empty list — and macMCP, which
// reads an empty filter as "all", would never even be asked the narrowing
// question this side invented.
func TestScopePicker_UnchosenDependencyListsAcrossEverything(t *testing.T) {
	vm := seedPickerVM(t)
	// Clear the account choice, then open the field that depends on it.
	evalString(t, vm, `(function(){ window.setProjScopeText('macmcp', 'mail_accounts', ''); return ''; })()`)
	openField(t, vm, "mail_mailboxes")

	sent := sentMessages(t, vm)
	if strings.Contains(sent, "mail_accounts") {
		t.Fatalf("an unchosen dependency was sent anyway; a server may read it as 'match nothing': %s", sent)
	}
	if !strings.Contains(sent, `"values":{}`) {
		t.Errorf("want an empty values object, got: %s", sent)
	}

	// And the answer — every account's mailboxes — renders as the choices.
	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'Alice/INBOX'},{value:'Bob/INBOX'}]}`)
	for _, want := range []string{"Alice/INBOX", "Bob/INBOX"} {
		if !strings.Contains(html, want) {
			t.Errorf("with no account chosen the picker does not list across all accounts: missing %q\n%s", want, html)
		}
	}
}

// ---------------------------------------------------------------------------
// The rest of the panel is unchanged
// ---------------------------------------------------------------------------

// A field the MCP does not declare enumerable keeps its text box, with no
// picker, no fetch and no error.
func TestScopePicker_NonEnumerableFieldIsUnchanged(t *testing.T) {
	vm := seedPickerVM(t)
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, `setProjScopeText('macmcp', 'mail_note', this.value)`) {
		t.Errorf("a non-enumerable field lost its text input\n%s", html)
	}
	if strings.Contains(html, `toggleScopeFieldPicker('macmcp', 'mail_note')`) {
		t.Errorf("a picker was offered for a field the MCP cannot list\n%s", html)
	}
}

// The read-only rendering of a source: "project_path" field is untouched — it
// is derived, not chosen, and there is nothing to pick.
func TestScopePicker_ProjectPathFieldStillReadOnly(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_local")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, `readonly value="/Users/x/work"`) {
		t.Errorf("the derived field stopped rendering read-only\n%s", html)
	}
	if strings.Contains(html, `toggleScopeFieldPicker('macmcp', 'write_dirs')`) {
		t.Error("a picker was offered for a value relay derives")
	}
}

// A repaint driven by an arriving answer must not eat what someone was typing.
// The name lives only in the DOM until harvest, and this is the one render in
// the editor that is not triggered by a click.
func TestScopePicker_AnArrivingAnswerDoesNotEatTheNameBeingTyped(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_accounts")
	got := evalString(t, vm, `(function(){
		document.getElementById('projName').value = 'Hermes — Alice, half-typed';
		window.onScopeFieldEnumerated({mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[{value:'Alice'}]});
		return window.state.projectForm.name;
	})()`)
	if got != "Hermes — Alice, half-typed" {
		t.Fatalf("the repaint lost the name being typed: %q", got)
	}
}

// A cross-product scope makes duplicate values normal: every account has an
// INBOX, and the mailbox value is account-independent (ADR-011's worked
// example). Two boxes holding the same value would tick and untick together,
// which reads as a bug, so they are one choice carrying both labels.
func TestScopePicker_OneValueIsOneChoiceHoweverOftenItIsOffered(t *testing.T) {
	vm := seedPickerVM(t)
	evalString(t, vm, `(function(){ window.setProjScopeText('macmcp', 'mail_mailboxes', ''); return ''; })()`)
	openField(t, vm, "mail_mailboxes")
	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[
		{value:'INBOX', label:'INBOX (Alice)'},
		{value:'INBOX', label:'INBOX (Bob)'},
		{value:'Projects/Archive', label:'Projects/Archive (Bob)'}]}`)

	if n := strings.Count(html, "toggleProjScopeValueAt("); n != 2 {
		t.Errorf("want 2 choices for 2 distinct values, got %d\n%s", n, html)
	}
	if !strings.Contains(html, "INBOX (Alice) · INBOX (Bob)") {
		t.Errorf("the labels of a collapsed duplicate were thrown away\n%s", html)
	}
}
