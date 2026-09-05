package main

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

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

	openField(t, vm, "mail_accounts")
	openField(t, vm, "mail_accounts")
	if n := strings.Count(sentMessages(t, vm), "enumerate_scope_field"); n != 1 {
		t.Errorf("re-opening a cached field made %d requests, want 1", n)
	}
}

func TestScopePicker_ChoosingWritesTheStoredValue(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_accounts")
	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[{value:'Alice',label:'Alice'},{value:'Bob',label:'Bob (work)'}]}`)

	if !strings.Contains(html, "Bob (work)") {
		t.Errorf("the MCP's own label is not shown\n%s", html)
	}
	// Per-row bindings come first, in offer order -- toggleProjScopeValueAt(0,
	// ...) below depends on that -- with the bulk "select all" binding
	// appended last, carrying every offered value as one array rather than
	// one value each.
	got := evalString(t, vm, `(function(){
		var out = [];
		for (var i = 0; i < window.state._scopeBind.length; i++) out.push(window.state._scopeBind[i].value);
		return JSON.stringify(out);
	})()`)
	if got != `["Alice","Bob",["Alice","Bob"]]` {
		t.Fatalf("bindings = %s, want both offered values plus the bulk-select binding", got)
	}
	if strings.Count(html, " checked ") != 1 {
		t.Errorf("want exactly the stored value ticked\n%s", html)
	}

	payload := evalString(t, vm, `(function(){
		window.toggleProjScopeValueAt(0, true);
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":["Bob","Alice"]`) {
		t.Fatalf("ticking a value did not reach the payload: %s", payload)
	}

	payload = evalString(t, vm, `(function(){
		window.toggleProjScopeValueAt(1, false);
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":["Alice"]`) {
		t.Fatalf("unticking removed the wrong thing: %s", payload)
	}
}

func TestScopePicker_AStoredValueNoLongerOfferedSurvives(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_mailboxes")
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
	payload := evalString(t, vm, `(function(){
		document.getElementById('projName').value = 'Hermes — Bob INBOX';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_mailboxes":["INBOX","Projects/Archive"]`) {
		t.Fatalf("an unrecognised value did not survive the save: %s", payload)
	}
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

func TestScopePicker_DegradedCases(t *testing.T) {
	cases := []struct {
		name       string
		clearFirst bool
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
			if !c.clearFirst && !strings.Contains(html, "Bob") {
				t.Errorf("the stored value disappeared behind a failure\n%s", html)
			}
		})
	}
}

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

func TestScopePicker_DependencyOrder(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_mailboxes")

	sent := sentMessages(t, vm)
	if !strings.Contains(sent, `"values":{"mail_accounts":["Bob"]}`) {
		t.Fatalf("the mailbox list was not read within the chosen account: %s", sent)
	}
	answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'INBOX'},{value:'Projects/Archive'}]}`)

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

func TestScopePicker_UnchosenDependencyListsAcrossEverything(t *testing.T) {
	vm := seedPickerVM(t)
	evalString(t, vm, `(function(){ window.setProjScopeText('macmcp', 'mail_accounts', ''); return ''; })()`)
	openField(t, vm, "mail_mailboxes")

	sent := sentMessages(t, vm)
	if strings.Contains(sent, "mail_accounts") {
		t.Fatalf("an unchosen dependency was sent anyway; a server may read it as 'match nothing': %s", sent)
	}
	if !strings.Contains(sent, `"values":{}`) {
		t.Errorf("want an empty values object, got: %s", sent)
	}

	html := answer(t, vm, `{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'Alice/INBOX'},{value:'Bob/INBOX'}]}`)
	for _, want := range []string{"Alice/INBOX", "Bob/INBOX"} {
		if !strings.Contains(html, want) {
			t.Errorf("with no account chosen the picker does not list across all accounts: missing %q\n%s", want, html)
		}
	}
}

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

func TestScopePicker_ProjectPathFieldStillReadOnly(t *testing.T) {
	vm := seedScopeVM(t, scopeProjectsFixture, "p_local")
	html := evalString(t, vm, `window.renderProjectForm()`)
	if !strings.Contains(html, `readonly value="/Users/x/work"`) {
		t.Errorf("the derived field stopped rendering read-only\n%s", html)
	}
	if strings.Contains(html, `toggleScopeFieldPicker('macmcp', 'file_dirs')`) {
		t.Error("a picker was offered for a value relay derives")
	}
}

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

// ---------------------------------------------------------------------------
// "Select all" / "Confirm: nothing to grant here" (ADR-011 addendum, "A star
// and an empty array"). These write through the same setProjScopeText path
// every other control does, so save/validate/audit need no changes; what's
// under test here is that the buttons produce the right STORED value, that a
// bulk write does not disturb dependency refresh, and that a field this
// session never touches stays omitted regardless of what these buttons do to
// OTHER fields.
// ---------------------------------------------------------------------------

func TestScopePicker_SelectAllStoresEveryOfferedValue(t *testing.T) {
	vm := seedPickerVM(t)
	openField(t, vm, "mail_accounts")
	answer(t, vm, `{mcp_id:'macmcp', field:'mail_accounts', status:'ok', values:[{value:'Alice',label:'Alice'},{value:'Bob',label:'Bob'},{value:'Carol',label:'Carol'}]}`)

	html := evalString(t, vm, `(function(){
		window.selectAllScopeValuesAt(3); // 3 per-row bindings (0,1,2), then the bulk one
		return window.renderProjectForm();
	})()`)
	if strings.Count(html, " checked") != 3 {
		t.Errorf("want all 3 offered values ticked after select-all\n%s", html)
	}

	payload := evalString(t, vm, `(function(){
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":["Alice","Bob","Carol"]`) {
		t.Fatalf("select-all did not reach the payload as every offered value: %s", payload)
	}
}

// Clearing a field that already held a real value ("Bob", from the fixture)
// has to OMIT it from the payload -- back to unset, not a confirmed-empty
// grant. This is the deliberate distinction confirmScopeFieldEmpty's own
// comment names: the two look identical once resolved to a VALUE ([]), and
// have to be told apart before that, in the text they write.
func TestScopePicker_ClearAllOmitsAPreviouslySetField(t *testing.T) {
	vm := seedPickerVM(t)
	payload := evalString(t, vm, `(function(){
		window.clearScopeValues('macmcp', 'mail_accounts');
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if strings.Contains(payload, "mail_accounts") {
		t.Errorf("a cleared field that had a real value was still sent: %s", payload)
	}
	// mail_mailboxes was never touched and must be unaffected.
	if !strings.Contains(payload, "mail_mailboxes") {
		t.Errorf("clearing one field disturbed another untouched one: %s", payload)
	}
}

// The whole point of the button: contacts_list_groups (or any tool a field
// like this governs) must stop erroring for an account that genuinely has
// none. Zero offered values -> the button appears -> clicking it stores [],
// which reaches the payload as an explicit empty array, not an omission.
func TestScopePicker_ConfirmEmptyAppearsOnlyWithZeroOfferedValuesAndStoresAnEmptyArray(t *testing.T) {
	vm := seedPickerVM(t)
	evalString(t, vm, `(function(){ window.setProjScopeText('macmcp', 'mail_mailboxes', ''); return ''; })()`)
	openField(t, vm, "mail_mailboxes")

	// bindNamesAfter resets _actBind (answer()/renderProjectForm() calls do
	// not go through render(), so the table would otherwise accumulate
	// every prior render's entries) and returns the fresh snapshot's bound
	// function names — confirmScopeFieldEmpty and selectAllScopeValuesAt go
	// through bind()/data-act, so their markup carries no function-call
	// text left to grep for.
	bindNamesAfter := func(enumJSON string) string {
		return evalString(t, vm, `(function(){
			window.onScopeFieldEnumerated(`+enumJSON+`);
			window.state._actBind = [];
			window.renderProjectForm();
			return JSON.stringify(window.state._actBind.map(function(e){ return [e[0].name].concat(e[1]); }));
		})()`)
	}

	// Nonzero offered: no confirm-empty button, select-all instead.
	withValuesBinds := bindNamesAfter(`{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[{value:'INBOX',label:'INBOX'}]}`)
	if strings.Contains(withValuesBinds, "confirmScopeFieldEmpty") {
		t.Errorf("the confirm-empty button appeared alongside real offered values\n%s", withValuesBinds)
	}
	if !strings.Contains(withValuesBinds, "selectAllScopeValuesAt") {
		t.Errorf("select-all is missing when there are values to select\n%s", withValuesBinds)
	}

	// A second answer needs a second in-flight request armed first --
	// onScopeFieldEnumerated drops an answer with none outstanding, which is
	// what stops a late reply to a since-changed dependency from landing.
	evalString(t, vm, `(function(){ window.retryScopeEnum('macmcp', 'mail_mailboxes'); return ''; })()`)

	// Zero offered: the confirm-empty button appears, select-all does not.
	emptyBinds := bindNamesAfter(`{mcp_id:'macmcp', field:'mail_mailboxes', status:'ok', values:[]}`)
	if !strings.Contains(emptyBinds, `["confirmScopeFieldEmpty","macmcp","mail_mailboxes"]`) {
		t.Fatalf("the confirm-empty button did not appear for zero offered values\n%s", emptyBinds)
	}
	if strings.Contains(emptyBinds, "selectAllScopeValuesAt") {
		t.Errorf("select-all appeared with nothing to select\n%s", emptyBinds)
	}

	payload := evalString(t, vm, `(function(){
		window.confirmScopeFieldEmpty('macmcp', 'mail_mailboxes');
		document.getElementById('projName').value = 'X';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_mailboxes":[]`) {
		t.Fatalf("confirm-empty did not store an explicit empty array: %s", payload)
	}
}

// Reopening a project whose mail_accounts was already confirmed-empty, and
// saving WITHOUT touching that field, must preserve [] -- not silently
// revert to "unset" because the in-session text for an untouched field is
// indistinguishable from blank. This is scopeFieldWasEverAsserted's
// "not touched this session" branch, which reads the persisted record
// instead of the lossy text round-trip.
func TestScopePicker_ReopeningAConfirmedEmptyFieldPreservesItUntouched(t *testing.T) {
	vm := newAppVM(t)
	script := `(function(){
		window.state.page = 'projects';
		window.state.externalMcps = [{id:'macmcp', display_name:'macMCP'}];
		window.state.mcpScopeFields = ` + pickerFieldsFixture + `;
		window.state.projects = [
			{id:'p_empty', name:'Confirmed Empty', kind:'remote', path:'', allowed_mcp_ids:['macmcp'], allowed_models:[],
			 allowed_tools:{macmcp:['mail_*']}, access:{macmcp:'read'},
			 context:{macmcp:{mail_accounts:[], mail_mailboxes:['INBOX']}}, disabled_tools:{}}
		];
		window.editProject('p_empty');
		return true;
	})()`
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	payload := evalString(t, vm, `(function(){
		document.getElementById('projName').value = 'Confirmed Empty';
		return JSON.stringify(window.harvestProjectForm().context);
	})()`)
	if !strings.Contains(payload, `"mail_accounts":[]`) {
		t.Fatalf("an untouched confirmed-empty field reverted on save: %s", payload)
	}
	if !strings.Contains(payload, `"mail_mailboxes":["INBOX"]`) {
		t.Fatalf("an untouched field with real values did not survive save: %s", payload)
	}
}
