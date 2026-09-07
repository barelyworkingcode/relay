package main

// doc_reason_strings_test.go closes a gap none of this repo's other guards
// touch: nothing checked that a documentation quote of a presence-prompt
// reason string, or of any other requireGate() call's message, still
// matches the fmt.Sprintf format actually producing it. Six such
// inaccuracies were found by hand on this branch -- stale command output,
// missing table columns, one security-surface doc overstating what an
// authentication dialog can show. A human reading carefully is not a
// control; this is the control.
//
// The design: read every requireGate(gate, ctx, op, digest, reason) call
// site in package main via go/ast, resolve `reason` back to the literal
// fmt.Sprintf format string(s) that can produce it -- following a named
// "...Reason" helper and, one level deep, a helper it itself calls (the
// only case that needs it: enrolmentApproveReason wraps
// enrolmentSignReason) -- and turn each format string into a regex, with
// %q becoming a quoted-wildcard and %s/%d becoming a bare one. Two things
// then get checked against those regexes: docs/cli.md's own "Prompt names"
// reference table, cell by cell, and every other non-ADR doc under docs/,
// by searching for each template's literal prefix and requiring the regex
// to match somewhere nearby. The prefix-anchor search is deliberately
// tolerant of an ellipsis, a placeholder word, or surrounding prose -- %s
// and %q are wildcards, so truncated or genericised argument text still
// matches; only the literal connective words between them are pinned down.
//
// What this does NOT cover, on purpose: command *output* -- tables, `relay
// ... list` captures, JSON blobs -- is not format-string-shaped the way a
// reason string is, and turning "real capture" prose into a checkable
// template would mean re-deriving relay's own rendering logic inside the
// test. Judged not worth it; see the task write-up this file's commit
// message points at. docs/decisions/**.md is excluded deliberately too:
// docs/decisions/000-readme.md says those are "immutable once accepted,
// and superseded by writing a new ADR" -- a stale illustrative example in
// an accepted ADR is not a bug this guard should be fixing by editing the
// ADR in place.
//
// Both failure directions are exercised by hand, not just asserted: see
// the commit message for the two mutation proofs (a doc string edited to
// drift, a Go format string edited to drift), each shown turning this
// suite red.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docReasonQ/S/D are private-use code points standing in for a resolved
// %q/%s/%d verb inside a template string, chosen so regexp.QuoteMeta (run
// over the whole template before these are swapped for real wildcard
// regex fragments) never mangles them -- none of the ASCII metacharacters
// QuoteMeta escapes collide with the Unicode private-use area.
const (
	docReasonQ = ""
	docReasonS = ""
	docReasonD = ""
)

// docReasonFuncIndex is every free (non-method) function declared across
// package main's non-test source, keyed by name -- enough to follow a
// requireGate call's reason argument to a named "...Reason" helper, and
// that helper to one it calls in turn.
type docReasonFuncIndex map[string]*ast.FuncDecl

// docReasonModuleFiles parses every non-test .go file directly under root
// and returns them keyed by path, alongside the fset needed to resolve
// positions for failure messages.
func docReasonModuleFiles(t *testing.T, root string, names []string) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(names))
	for _, name := range names {
		path := filepath.Join(root, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files[name] = f
	}
	return files, fset
}

func docReasonBuildFuncIndex(files map[string]*ast.File) docReasonFuncIndex {
	idx := make(docReasonFuncIndex)
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			idx[fn.Name.Name] = fn
		}
	}
	return idx
}

// docReasonConstString evaluates a compile-time-constant string expression
// -- a literal, or literals joined with "+" (sealedResetReason's format
// string is split across two lines this way). Anything else reports false
// rather than guessing.
func docReasonConstString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return v, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok1 := docReasonConstString(e.X)
		r, ok2 := docReasonConstString(e.Y)
		if !ok1 || !ok2 {
			return "", false
		}
		return l + r, true
	case *ast.ParenExpr:
		return docReasonConstString(e.X)
	}
	return "", false
}

// docReasonIsSprintf reports whether call is fmt.Sprintf(...).
func docReasonIsSprintf(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt"
}

// docReasonExpandSprintf walks a format string's %-verbs left to right,
// substituting a marker for each -- unless the corresponding argument is
// itself a call to another "...Reason" helper in idx, in which case that
// helper's own templates are inlined and multiplied out. This is what
// makes enrolmentApproveReason's "approve an enrolment request from %s and
// %s" resolve to real, checkable composites with enrolmentSignReason's two
// branches inlined into the second %s, rather than a wildcard that would
// hide a real mismatch there.
func docReasonExpandSprintf(t *testing.T, format string, args []ast.Expr, idx docReasonFuncIndex, depth int) []string {
	t.Helper()
	out := []string{""}
	argi := 0
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			for j := range out {
				out[j] += string(c)
			}
			continue
		}
		if i+1 >= len(format) {
			t.Fatalf("doc_reason_strings_test.go: dangling %% in format %q", format)
		}
		verb := format[i+1]
		i++
		var marker string
		switch verb {
		case 'q':
			marker = docReasonQ
		case 's':
			marker = docReasonS
		case 'd':
			marker = docReasonD
		default:
			t.Fatalf("doc_reason_strings_test.go does not model verb %%%c in format %q -- extend it before trusting this guard", verb, format)
		}
		if argi >= len(args) {
			t.Fatalf("doc_reason_strings_test.go: format %q has more verbs than arguments", format)
		}
		nested := docReasonResolveNested(t, args[argi], idx, depth)
		argi++
		if nested == nil {
			for j := range out {
				out[j] += marker
			}
			continue
		}
		var next []string
		for _, o := range out {
			for _, n := range nested {
				next = append(next, o+n)
			}
		}
		out = next
	}
	return out
}

// docReasonResolveNested only recurses into an argument expression when it
// is a call to a sibling helper whose name ends in "Reason" -- joinWithAnd,
// classStrings and the like stay opaque wildcards, deliberately: they
// produce caller data, not more reason-string shape to check.
func docReasonResolveNested(t *testing.T, expr ast.Expr, idx docReasonFuncIndex, depth int) []string {
	if depth <= 0 {
		return nil
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || !strings.HasSuffix(id.Name, "Reason") {
		return nil
	}
	fn, ok := idx[id.Name]
	if !ok {
		return nil
	}
	return docReasonFuncTemplates(t, fn, idx, depth-1)
}

// docReasonResolveExpr resolves one expression -- a return value, or a
// requireGate reason argument -- to every template it can produce.
func docReasonResolveExpr(t *testing.T, expr ast.Expr, idx docReasonFuncIndex, depth int) []string {
	t.Helper()
	if s, ok := docReasonConstString(expr); ok {
		return []string{s}
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil
	}
	if docReasonIsSprintf(call) {
		format, ok := docReasonConstString(call.Args[0])
		if !ok {
			t.Fatalf("doc_reason_strings_test.go: fmt.Sprintf call with a non-constant format string")
		}
		return docReasonExpandSprintf(t, format, call.Args[1:], idx, depth)
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		if fn, ok := idx[id.Name]; ok {
			return docReasonFuncTemplates(t, fn, idx, depth)
		}
	}
	return nil
}

// docReasonFuncTemplates collects every template a "...Reason"-shaped
// function can return, across all its return statements (mcpRegisterReason
// and friends each branch on an if and return a different Sprintf per
// arm).
func docReasonFuncTemplates(t *testing.T, fn *ast.FuncDecl, idx docReasonFuncIndex, depth int) []string {
	t.Helper()
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		tmpls := docReasonResolveExpr(t, ret.Results[0], idx, depth)
		if tmpls == nil {
			t.Fatalf("doc_reason_strings_test.go: could not resolve a return statement in %s to a template -- its shape changed in a way this guard does not model; extend it rather than let it go quiet", fn.Name.Name)
		}
		out = append(out, tmpls...)
		return true
	})
	return out
}

// docReasonOp is one requireGate call site's resolved op name and reason
// templates, plus enough position info to name it in a failure.
type docReasonSite struct {
	op        string
	templates []string
	pos       string
}

// docReasonExtractSites walks every FuncDecl (free function or method) in
// files and returns one docReasonSite per requireGate(...) call found,
// resolving its reason argument -- following a local variable assignment
// first if the argument is a bare identifier (EnrolmentOps.Approve assigns
// `reason := enrolmentApproveReason(...)` before passing it on).
func docReasonExtractSites(t *testing.T, files map[string]*ast.File, fset *token.FileSet, idx docReasonFuncIndex) []docReasonSite {
	t.Helper()
	var sites []docReasonSite
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "requireGate" {
					return true
				}
				if len(call.Args) != 5 {
					t.Fatalf("doc_reason_strings_test.go: requireGate call at %s has %d args, want 5 -- its signature changed; update this guard", fset.Position(call.Pos()), len(call.Args))
				}
				pos := fset.Position(call.Pos()).String()
				op, ok := docReasonConstString(call.Args[2])
				if !ok {
					t.Fatalf("doc_reason_strings_test.go: requireGate call at %s has a non-constant op argument", pos)
				}
				reasonArg := call.Args[4]
				if reasonID, ok := reasonArg.(*ast.Ident); ok {
					if assigned := docReasonFindAssignment(fn.Body, reasonID.Name); assigned != nil {
						reasonArg = assigned
					}
				}
				tmpls := docReasonResolveExpr(t, reasonArg, idx, 3)
				if len(tmpls) == 0 {
					t.Fatalf("doc_reason_strings_test.go: could not resolve the reason argument of the requireGate call at %s to any template -- extend the resolver rather than silently skip a gated operation's prompt text", pos)
				}
				sites = append(sites, docReasonSite{op: op, templates: tmpls, pos: pos})
				return true
			})
		}
	}
	return sites
}

// docReasonFindAssignment looks for `name := expr` or `name = expr`
// anywhere in body's top-level statement list (and one level into if/else
// blocks, which is as deep as any current call site nests) and returns
// expr from the last such assignment found before returning nil.
func docReasonFindAssignment(body *ast.BlockStmt, name string) ast.Expr {
	var found ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == name && i < len(assign.Rhs) {
				found = assign.Rhs[i]
			}
		}
		return true
	})
	return found
}

// docReasonRequiredOps is every gated operation this guard insists on
// resolving at least one template for. If extraction ever finds zero for
// one of these, that is the guard going quiet, not the guard passing --
// see TestDocReasonStrings_OpCoverageIsNotVacuous.
var docReasonRequiredOps = []string{
	"credential.mint",
	"credential.revoke",
	"enrolment.create",
	"enrolment.sign",
	"enrolment.update",
	"enrolment.revoke",
	"login.bootstrap.mint",
	"login.passkey.revoke",
	"eve.enrolment.open",
	"eve.passkey.revoke",
	"mcp.register",
	"mcp.oauth.start",
	"service.register",
	"project.grant",
	"project.rotate_token",
}

// docReasonOpTemplates re-derives, from source, every template each gated
// operation's reason string can take. Building this is the "read the Go
// source" half of the guard; docReasonCheckTable and
// docReasonCheckProseAnchors below are the "read the documentation" half.
func docReasonOpTemplates(t *testing.T) map[string][]string {
	t.Helper()
	root := gateASTModuleRoot(t)
	names := gateASTFiles(t, root)
	files, fset := docReasonModuleFiles(t, root, names)
	idx := docReasonBuildFuncIndex(files)
	sites := docReasonExtractSites(t, files, fset, idx)
	if len(sites) == 0 {
		t.Fatalf("doc_reason_strings_test.go: found zero requireGate call sites in %s -- the AST walk is broken, not the codebase", root)
	}
	byOp := make(map[string]map[string]bool)
	for _, s := range sites {
		if byOp[s.op] == nil {
			byOp[s.op] = make(map[string]bool)
		}
		for _, tmpl := range s.templates {
			byOp[s.op][tmpl] = true
		}
	}
	out := make(map[string][]string, len(byOp))
	for op, set := range byOp {
		var list []string
		for tmpl := range set {
			list = append(list, tmpl)
		}
		sort.Strings(list)
		out[op] = list
	}
	return out
}

// TestDocReasonStrings_OpCoverageIsNotVacuous is the guard's own
// anti-vacuity check on the extraction side: if AST-walking the source
// finds no template for an operation this file knows to be gated and
// reason-bearing, that is this guard silently losing coverage, and must
// fail loudly rather than let the table/prose checks below quietly have
// nothing to compare against.
func TestDocReasonStrings_OpCoverageIsNotVacuous(t *testing.T) {
	ops := docReasonOpTemplates(t)
	for _, op := range docReasonRequiredOps {
		tmpls := ops[op]
		if len(tmpls) == 0 {
			t.Errorf("extraction found zero reason templates for gated op %q; this guard cannot check documentation against an operation it cannot see", op)
		}
	}
}

// docReasonBuildRegex turns a marker-embedded template into a compiled,
// unanchored regex: %q becomes a quoted span with no interior quote, %s
// and %d become permissive wildcards (a real doc quote may show a
// placeholder word, a truncated value, or an ellipsis in either verb's
// place -- all of those are just non-empty text to the wildcard).
func docReasonBuildRegex(tmpl string) *regexp.Regexp {
	escaped := regexp.QuoteMeta(tmpl)
	escaped = strings.ReplaceAll(escaped, docReasonQ, `"[^"\r\n]+"`)
	escaped = strings.ReplaceAll(escaped, docReasonD, `[0-9]+`)
	escaped = strings.ReplaceAll(escaped, docReasonS, `[\s\S]+`)
	return regexp.MustCompile(escaped)
}

func docReasonFullMatch(tmpl, text string) bool {
	re := regexp.MustCompile("^(?:" + docReasonBuildRegex(tmpl).String() + ")$")
	return re.MatchString(text)
}

// docReasonAnyFullMatch reports whether text is a complete match for any
// of the given operation's known templates -- the doc is free to show just
// one illustrative branch (e.g. only the non-empty-grant-list form of
// enrol create), not every branch the Go function can take.
func docReasonAnyFullMatch(text string, tmpls []string) bool {
	for _, tmpl := range tmpls {
		if docReasonFullMatch(tmpl, text) {
			return true
		}
	}
	return false
}

// docReasonCLICommandOps maps docs/cli.md's reference-table command column
// to the requireGate op name it documents. A row whose command is not in
// this map fails the test by name (docReasonCheckTable), the same
// discipline gate_structural_test.go's allowlist uses: a new gated CLI
// command earns a mapping entry, on purpose, rather than being silently
// skipped by a guard that looks green either way.
var docReasonCLICommandOps = map[string]string{
	"credential mint":   "credential.mint",
	"credential revoke": "credential.revoke",
	"enrol create":      "enrolment.create",
	"enrol sign":        "enrolment.sign",
	"enrol update":      "enrolment.update",
	"enrol revoke":      "enrolment.revoke",
	"login enrol":       "login.bootstrap.mint",
	"login revoke":      "login.passkey.revoke",
	"eve enrol":         "eve.enrolment.open",
	"eve revoke":        "eve.passkey.revoke",
	"mcp register":      "mcp.register",
	"service register":  "service.register",
}

var docReasonTableRowRE = regexp.MustCompile("^\\| `([^`]+)` \\| (.+) \\|$")
var docReasonBacktickSpanRE = regexp.MustCompile("`([^`]+)`")

// docReasonCheckTable validates docs/cli.md's "Every gated command has its
// own version of this reason string" table -- the single document an
// operator is told to use as the reference for what a real dialog should
// say, and so the highest-value target this guard has. Two rows
// (mcp register, service register) carry two backtick spans each and get
// bespoke reconstruction; every other mapped row must carry exactly one
// span that fully matches one of its op's known templates.
func docReasonCheckTable(t *testing.T, ops map[string][]string) (rowsChecked int) {
	t.Helper()
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "cli.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	seen := make(map[string]bool)
	for _, line := range lines {
		m := docReasonTableRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cmd, cell := m[1], m[2]
		op, known := docReasonCLICommandOps[cmd]
		if !known {
			// Not every backtick-first-column table row in cli.md is this
			// reason-string table (the quick-reference table above it has
			// the same "| `cmd` | ... |" shape). Only rows this map claims
			// are checked; anything that looks like it belongs but is
			// unmapped would be a false negative worth knowing about, so
			// rows that plausibly ARE reason quotes but unmapped are
			// caught by the prose-anchor scan instead, not silently here.
			continue
		}
		seen[cmd] = true
		rowsChecked++
		spans := docReasonBacktickSpanRE.FindAllStringSubmatch(cell, -1)
		switch cmd {
		case "mcp register":
			if len(spans) != 2 {
				t.Errorf("docs/cli.md: %q row has %d backtick spans, want 2 (full form + HTTP fragment): %q", cmd, len(spans), cell)
				continue
			}
			full, frag := spans[0][1], spans[1][1]
			if !docReasonAnyFullMatch(full, ops[op]) {
				t.Errorf("docs/cli.md: %q row's primary form %q does not match any known %s template:\n%s", cmd, full, op, strings.Join(ops[op], "\n"))
			}
			idx := strings.Index(full, "that runs")
			if idx < 0 {
				t.Errorf("docs/cli.md: %q row's primary form %q has no \"that runs\" to splice the HTTP fragment onto", cmd, full)
				continue
			}
			reconstructed := full[:idx] + frag
			if !docReasonAnyFullMatch(reconstructed, ops[op]) {
				t.Errorf("docs/cli.md: %q row's HTTP form (reconstructed as %q) does not match any known %s template:\n%s", cmd, reconstructed, op, strings.Join(ops[op], "\n"))
			}
		case "service register":
			if len(spans) != 2 {
				t.Errorf("docs/cli.md: %q row has %d backtick spans, want 2 (register form + update form): %q", cmd, len(spans), cell)
				continue
			}
			for _, span := range spans {
				if !docReasonAnyFullMatch(span[1], ops[op]) {
					t.Errorf("docs/cli.md: %q row's form %q does not match any known %s template:\n%s", cmd, span[1], op, strings.Join(ops[op], "\n"))
				}
			}
		default:
			if len(spans) != 1 {
				t.Errorf("docs/cli.md: %q row has %d backtick spans, want 1: %q", cmd, len(spans), cell)
				continue
			}
			if !docReasonAnyFullMatch(spans[0][1], ops[op]) {
				t.Errorf("docs/cli.md: %q row's form %q does not match any known %s template:\n%s", cmd, spans[0][1], op, strings.Join(ops[op], "\n"))
			}
		}
	}
	for cmd := range docReasonCLICommandOps {
		if !seen[cmd] {
			t.Errorf("docs/cli.md: expected a reason-table row for %q, found none -- either the table dropped it or docReasonCLICommandOps is stale", cmd)
		}
	}
	return rowsChecked
}

// docReasonDocsRoot lists every doc this guard reads prose from: docs/ in
// full except docs/decisions/, which docs/decisions/000-readme.md declares
// immutable once accepted -- a stale illustrative reason-string example in
// an accepted ADR is a fact about history, not a bug this file should be
// fixing by editing the ADR.
func docReasonProseDocs(t *testing.T, root string) []string {
	t.Helper()
	docsDir := filepath.Join(root, "docs")
	var out []string
	err := filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "decisions" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", docsDir, err)
	}
	if len(out) == 0 {
		t.Fatalf("doc_reason_strings_test.go: found zero non-ADR markdown docs under %s", docsDir)
	}
	sort.Strings(out)
	return out
}

var docReasonWhitespaceRE = regexp.MustCompile(`\s+`)
var docReasonBlockquoteRE = regexp.MustCompile(`(?m)^\s{0,3}>+\s?`)

// docReasonNormalizeProse strips markdown blockquote markers line by line
// (a wrapped `> "Relay is trying to ...` quote otherwise gets a stray "> "
// spliced into the middle of the sentence once newlines are collapsed) and
// then collapses all whitespace, including the newlines themselves, to
// single spaces -- the reason strings this guard checks are one sentence
// each, wrapped across source lines for the editor's benefit, not the
// reader's.
func docReasonNormalizeProse(raw string) string {
	stripped := docReasonBlockquoteRE.ReplaceAllString(raw, "")
	return docReasonWhitespaceRE.ReplaceAllString(stripped, " ")
}

// docReasonTemplateAnchor is the literal text of tmpl up to its first
// wildcard marker -- the fixed opening words a prose quote of this exact
// template would start with ("register the MCP ", "revoke the enrolment
// ", ...). Anchors under 12 runes are rejected by the caller as too likely
// to coincide with unrelated prose.
func docReasonTemplateAnchor(tmpl string) string {
	if i := strings.IndexAny(tmpl, docReasonQ+docReasonS+docReasonD); i >= 0 {
		return tmpl[:i]
	}
	return tmpl
}

// docReasonAnchorNeedsQuote reports whether tmpl's very next thing after
// anchor is a %q slot. When every variant sharing an anchor needs one, an
// occurrence of the anchor words with no nearby quote character is not a
// stale reason-string quote to flag -- it is ordinary prose that happens
// to open the same way ("...see #23 -- revoke the enrolment first." is not
// a mangled copy of `revoke the enrolment %q`, it is a sentence). A %s- or
// %d-first template (the enrolment.sign-via-Approve composite starts with
// the remote address, no quotes) gets no such exemption: those anchors are
// long and distinctive enough on their own.
func docReasonAnchorNeedsQuote(tmpl, anchor string) bool {
	return strings.HasPrefix(tmpl[len(anchor):], docReasonQ)
}

// docReasonCheckProseAnchors is the second half of the documentation side:
// every non-ADR doc, searched for the literal opening words of each known
// reason template. Where an anchor is found, the full template's regex
// must match somewhere in a bounded window after it -- tolerant of line
// wrapping (whitespace is collapsed first), an ellipsis, or trailing prose
// (the search is unanchored on the right), but not of the connecting words
// between arguments changing, which is exactly the class of drift this
// guard exists to catch.
func docReasonCheckProseAnchors(t *testing.T, ops map[string][]string) (matches int) {
	t.Helper()
	root := repoRoot(t)
	docs := docReasonProseDocs(t, root)

	type variant struct {
		op, tmpl string
		re       *regexp.Regexp
	}
	// Grouped by anchor text, not kept as one list per template: several
	// branches of the same reason function share an opening phrase
	// ("create an enrolment for client " precedes both the empty- and
	// non-empty-grant-list forms), and a doc quoting one branch must not
	// be flagged for failing to also look like the other.
	byAnchor := make(map[string][]variant)
	for op, tmpls := range ops {
		for _, tmpl := range tmpls {
			a := docReasonTemplateAnchor(tmpl)
			if len([]rune(a)) < 12 {
				continue
			}
			byAnchor[a] = append(byAnchor[a], variant{op: op, tmpl: tmpl, re: docReasonBuildRegex(tmpl)})
		}
	}
	if len(byAnchor) == 0 {
		t.Fatalf("doc_reason_strings_test.go: zero templates produced an anchor long enough to search for -- the extraction or anchor logic is broken")
	}

	const window = 600
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		normalized := docReasonNormalizeProse(string(raw))
		for anchor, variants := range byAnchor {
			allNeedQuote := true
			for _, v := range variants {
				if !docReasonAnchorNeedsQuote(v.tmpl, anchor) {
					allNeedQuote = false
					break
				}
			}
			start := 0
			for {
				i := strings.Index(normalized[start:], anchor)
				if i < 0 {
					break
				}
				pos := start + i
				end := pos + window
				if end > len(normalized) {
					end = len(normalized)
				}
				segment := normalized[pos:end]
				start = pos + len(anchor)

				if allNeedQuote {
					lookahead := segment[len(anchor):]
					if len(lookahead) > 6 {
						lookahead = lookahead[:6]
					}
					if !strings.ContainsRune(lookahead, '"') {
						// No quote follows the anchor within a few
						// characters, and every candidate template
						// requires one immediately: this occurrence is
						// ordinary prose that happens to open with the
						// same words, not a reason-string quote gone
						// stale. Skip it rather than flag a false drift.
						continue
					}
				}

				matched := false
				for _, v := range variants {
					if v.re.MatchString(segment) {
						matched = true
						break
					}
				}
				if matched {
					matches++
				} else {
					snippet := segment
					if len(snippet) > 160 {
						snippet = snippet[:160] + "…"
					}
					var shapes []string
					for _, v := range variants {
						shapes = append(shapes, v.tmpl)
					}
					t.Errorf("%s: found the opening words of a %s reason string (%q) but the rest does not match any current format string for it.\n  expected one of:\n    %s\n  found:\n    %s",
						path, variants[0].op, anchor, strings.Join(shapes, "\n    "), snippet)
				}
			}
		}
	}
	return matches
}

// TestDocReasonStrings_MatchGoSource is the guard itself. It reads
// presence-prompt (and sibling requireGate) reason strings out of the Go
// source, then checks docs/cli.md's reference table and every other
// non-ADR doc's prose for a quote that still matches. Both checks assert a
// nonzero count of things actually compared -- an extraction bug that
// silently found nothing to check must fail this test, not pass it.
func TestDocReasonStrings_MatchGoSource(t *testing.T) {
	ops := docReasonOpTemplates(t)

	rows := docReasonCheckTable(t, ops)
	if rows == 0 {
		t.Fatalf("docs/cli.md: checked zero reason-table rows; the table parser or docReasonCLICommandOps is broken, not merely empty")
	}
	t.Logf("checked %d docs/cli.md reason-table rows against their Go source templates", rows)

	matches := docReasonCheckProseAnchors(t, ops)
	if matches == 0 {
		t.Fatalf("scanned every non-ADR doc for a reason-string quote and found zero matches; either the docs stopped quoting reason strings in prose (lower this expectation deliberately) or the scan itself is broken -- either way this must not pass silently")
	}
	t.Logf("matched %d reason-string quotes in prose across non-ADR docs", matches)
}
