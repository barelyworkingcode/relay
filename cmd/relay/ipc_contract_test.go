package main

// Drift guard for the JS <-> Go half of the IPC contract, the other
// direction from ipc_handlers_test.go's TestIPCDispatch_AllDeclaredMessageTypesHaveHandlers:
// that test checks every declared Msg* constant has a handler; this one
// checks every message type string the settings UI actually sends has a
// handler. A typo'd or renamed type on the JS side degrades the same way an
// unwired Msg* constant does -- onSettingsIpc logs "unknown IPC message
// type" and the click does nothing -- and neither language's compiler
// catches it, since the type only exists as a string literal on both ends.
//
// It also asserts the stored-XSS pattern (see bind() in app.js) has not
// regressed for the handlers where it actually applies: an
// onclick="fn('...' + value + '...')" attribute is unsafe even through
// esc() when value is a free-text name, because the HTML parser decodes
// entities in an attribute value before the inline script text is
// compiled.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readAppJS(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../web/src/app.js")
	if err != nil {
		t.Fatalf("reading web/src/app.js: %v", err)
	}
	return string(data)
}

// findIPCCallBodies returns the full text of every ipc(JSON.stringify(...))
// call, scanning forward from "ipc(" to its balanced closing paren (quote
// -aware) so a call whose argument is a multi-line object literal is
// captured whole rather than truncated at the first ")" inside it. Plain
// "function ipc(msg) {" (the bridge itself) is excluded by requiring
// "JSON.stringify(" to follow immediately.
func findIPCCallBodies(src string) []string {
	var out []string
	for i := 0; i < len(src); {
		idx := strings.Index(src[i:], "ipc(")
		if idx < 0 {
			break
		}
		start := i + idx
		rest := strings.TrimLeft(src[start+len("ipc("):], " \t\n")
		if !strings.HasPrefix(rest, "JSON.stringify(") {
			i = start + len("ipc(")
			continue
		}
		depth := 0
		var inStr byte
		k := start + len("ipc") // position of the call's own '('
		for ; k < len(src); k++ {
			c := src[k]
			if inStr != 0 {
				if c == '\\' {
					k++
					continue
				}
				if c == inStr {
					inStr = 0
				}
				continue
			}
			switch c {
			case '\'', '"', '`':
				inStr = c
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					out = append(out, src[start:k+1])
				}
			}
			if depth == 0 && c == ')' {
				break
			}
		}
		if k >= len(src) {
			break
		}
		i = k + 1
	}
	return out
}

// typeLiteralPattern matches a `type:` field's literal value, including the
// two-branch form used by the one call that picks its type with a ternary
// (`type: checked ? 'start_service' : 'stop_service'`).
var typeLiteralPattern = regexp.MustCompile(`type:\s*(?:[\w.]+\s*\?\s*)?['"]([a-zA-Z_]+)['"](?:\s*:\s*['"]([a-zA-Z_]+)['"])?`)

// varAssignedTypePattern catches the one call shaped `ipc(JSON.stringify(msg))`
// where the object literal (with its own `type:` field) is built a few lines
// earlier and assigned to a variable rather than inlined into the call.
var varAssignedTypePattern = regexp.MustCompile(`\b\w+\s*=\s*\{\s*type:\s*['"]([a-zA-Z_]+)['"]`)

// extractIPCMessageTypes returns every message type string app.js sends to
// relay, deduplicated.
func extractIPCMessageTypes(t *testing.T, src string) map[string]bool {
	t.Helper()
	types := map[string]bool{}
	for _, call := range findIPCCallBodies(src) {
		m := typeLiteralPattern.FindStringSubmatch(call)
		if m != nil {
			types[m[1]] = true
			if m[2] != "" {
				types[m[2]] = true
			}
			continue
		}
		// No literal inline: the call passes a variable (e.g. `ipc(JSON.stringify(msg))`).
		// Its type must show up via varAssignedTypePattern elsewhere in the file.
	}
	for _, m := range varAssignedTypePattern.FindAllStringSubmatch(src, -1) {
		types[m[1]] = true
	}
	if len(types) == 0 {
		t.Fatal("extracted zero IPC message types from app.js -- the extraction regex or ipc() call shape has drifted")
	}
	return types
}

func TestIPCContract_EveryJSMessageTypeHasAHandler(t *testing.T) {
	src := readAppJS(t)
	types := extractIPCMessageTypes(t, src)
	for msgType := range types {
		if _, ok := ipcHandlers[msgType]; !ok {
			t.Errorf("app.js sends ipc message type %q, which is not a key in ipcHandlers (cmd/relay/ipc_handlers.go) -- it would silently do nothing", msgType)
		}
	}
}

// onclickConcatPattern finds an onclick="..." attribute built by string
// concatenation with a quoted argument -- the shape every stored-XSS case
// here actually needs: the HTML parser decodes entities in an attribute
// value BEFORE the inline script text is compiled, so a value containing a
// quote closes a JS string early and runs as script in this privileged
// WebView, even through esc(). Originally this only banned a curated list
// of free-text-name handlers (a plain id was considered safe, quote-free by
// construction) -- but the id-only exception was pure inconsistency for no
// real safety gain, so every dynamic onclick, id or name, now goes through
// bind()/data-act instead (see bind()'s own comment in app.js). A handful of
// PURELY numeric arguments (a bind-table index, never a value that could
// carry a quote) still build their onclick with concatenation and are not
// what this test is for -- it matches specifically on a quote before the
// `+`, which a bare number never has.
var onclickConcatPattern = regexp.MustCompile(`onclick="[^"]*'\s*\+`)

func TestIPCContract_NoOnclickBuiltByConcatenatingAQuotedValue(t *testing.T) {
	src := readAppJS(t)
	for _, loc := range onclickConcatPattern.FindAllStringIndex(src, -1) {
		line := strings.Count(src[:loc[0]], "\n") + 1
		snippet := src[loc[0]:loc[1]]
		t.Errorf("app.js:%d builds an onclick attribute by concatenating a quoted value (%q) -- it must go through bind()/data-act (see the comment on bind())", line, snippet)
	}
}
