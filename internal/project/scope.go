package project

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
)

func updateProjectMcps(s *config.Settings, id string, mcpIDs []string, surfaces McpSurfaces) {
	proj, _ := config.FindProjectByID(s, id)
	if proj == nil {
		return
	}
	proj.AllowedMcpIDs = mcpIDs
	syncProjectToken(s, proj, surfaces)
}

func updateProjectPath(s *config.Settings, id string, path string, surfaces McpSurfaces) {
	proj, _ := config.FindProjectByID(s, id)
	// Belt-and-braces: the real refusal is ValidateShape at the call
	// site, but this function is reachable from anywhere in this package
	// without passing through that guard. A remote project has no
	// filesystem scope, so silently refuse rather than let one acquire a
	// path no validation pass approved. Clearing an already-remote
	// project's path back to "" is a no-op and stays allowed.
	if proj == nil || (proj.IsRemote() && path != "") {
		return
	}
	proj.Path = path
	syncProjectToken(s, proj, surfaces)
}

// updateProjectKind keeps a profile enrolled until its enrolments have been
// explicitly removed; a remote-to-local conversion must never widen a grant.
func updateProjectKind(s *config.Settings, id string, kind config.ProjectKind) {
	proj, _ := config.FindProjectByID(s, id)
	if proj == nil {
		return
	}
	kind = config.NormalizeProjectKind(kind)
	// Belt-and-braces, exactly as updateProjectPath does it: the real
	// refusal is enrolment.ValidateProjectConversion at the call site, but this
	// function is reachable from anywhere in this package without passing
	// through that guard. A remote→local conversion under a live enrolment
	// strands that enrolment on a project whose shape it was never
	// validated against — a silent widening of what a remote client
	// reaches, rather than a loud error. Refuse silently here so a bypass
	// of the validated path cannot produce it (ADR-010 decision 3).
	if !kind.IsRemote() && proj.IsRemote() && len(enrolment.GrantingProject(s, id)) > 0 {
		return
	}
	proj.Kind = kind
}

// updateProjectContext replaces a project's per-MCP context values — the
// resource scope an operator sets (ADR-011 decisions 4 and 6) — and then
// runs syncProjectToken so every source: "project_path" field is
// re-derived on top of it (mergeContextField puts each back without
// touching the operator's). That ordering is what makes a wholesale
// replace safe: the operator cannot set a derived field directly
// (validateProjectPermissions refuses it), so a plain replace would
// otherwise silently delete one. surfaces is what that re-derivation
// needs; nil means no derivation.
func updateProjectContext(s *config.Settings, id string, values map[string]json.RawMessage, surfaces McpSurfaces) {
	proj, _ := config.FindProjectByID(s, id)
	if proj == nil {
		return
	}
	cleaned := make(map[string]json.RawMessage, len(values))
	for mcpID, blob := range values {
		if !config.IsWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		if len(ContextValues(blob)) == 0 {
			continue
		}
		cleaned[mcpID] = blob
	}
	proj.Context = cleaned
	syncProjectToken(s, proj, surfaces)
}

// syncProjectToken derives the project's disabled tools and context from
// its current allowedMcpIDs and path, driven by the SCHEMA rather than by a
// field name relay knows: it writes the project's path into every field
// declaring source: "project_path" because the schema asked it to (ADR-011
// decision 5). surfaces maps MCP IDs to what relay knows at runtime; a nil
// map skips derivation for a caller with no live MCP manager wired.
//
// Deliberately NOT done here: pruning a stored field the live schema no
// longer declares. A merely-down MCP reports no schema at all, which is
// indistinguishable from "this schema now declares zero fields", so
// pruning on that signal would delete an operator's values because a
// process happened to be down at sync time — and it isn't needed for
// safety anyway, since a stale key is inert (FilterKnownContextFields
// drops it at call time) and cannot be operator-set in the first place
// (validateProjectContextForMcp refuses unknown names at write time).
func syncProjectToken(s *config.Settings, proj *config.Project, surfaces McpSurfaces) {
	if proj.Context == nil {
		proj.Context = make(map[string]json.RawMessage)
	}
	if proj.DisabledTools == nil {
		proj.DisabledTools = make(map[string][]string)
	}
	mcpIDs := proj.AllowedMcpIDs
	if config.IsWildcard(mcpIDs) {
		mcpIDs = s.AllExternalMcpIDs()
	}
	allowed := make(map[string]bool, len(mcpIDs))
	for _, id := range mcpIDs {
		allowed[id] = true
	}
	for id := range proj.Context {
		if !allowed[id] {
			delete(proj.Context, id)
		}
	}
	for id := range proj.DisabledTools {
		if !allowed[id] {
			delete(proj.DisabledTools, id)
		}
	}
	for id := range proj.Access {
		if !allowed[id] {
			delete(proj.Access, id)
		}
	}
	for id := range proj.AllowedTools {
		if !allowed[id] {
			delete(proj.AllowedTools, id)
		}
	}
	for id := range proj.AllowExternal {
		if !allowed[id] {
			delete(proj.AllowExternal, id)
		}
	}
	// Defence in depth: a remote project has no Path, and both ways to
	// handle that are unsafe (an empty-string root, or omitting the field
	// and letting the MCP fall back to its own possibly-unrestricted
	// default), so remote projects skip derivation entirely, unconditionally,
	// rather than only when ValidateGrants happens to catch it —
	// this guard is what keeps a bypass of that check from silently
	// widening scope instead of failing loudly (ADR-011 decision 5).
	if proj.IsRemote() {
		return
	}
	for _, mcpID := range mcpIDs {
		surface := surfaces[mcpID]
		schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)
		if schema.V2() {
			derived := 0
			for _, f := range schema.ProjectPathFields() {
				value, err := json.Marshal(projectPathValue(f, proj.Path))
				if err != nil {
					continue
				}
				proj.Context[mcpID] = mergeContextField(proj.Context[mcpID], f.Name, value)
				derived++
			}
			// DEFERRED (ADR-011): fs_bash auto-disable stays a hardcoded tool
			// name here, keyed off "this MCP scopes something to the project
			// path" rather than off a field name — the most domain-blind
			// form available without a schema change to carry it.
			if derived > 0 {
				disableToolByDefault(proj, mcpID, V1FsBashTool)
			}
			continue
		}
		// v1 compatibility: the last place in relay that knows a field name
		// directly; see V1AllowedDirsField.
		if schemaHasField(surface.Schema, V1AllowedDirsField) {
			ctx, _ := json.Marshal(map[string]interface{}{V1AllowedDirsField: []string{proj.Path}})
			proj.Context[mcpID] = ctx
			disableToolByDefault(proj, mcpID, V1FsBashTool)
		}
	}
}

// projectPathValue: a string-typed field gets the bare path; anything else
// (including array-typed) gets a one-element list, which is the only shape
// that can carry more than one root later.
func projectPathValue(f ContextField, path string) interface{} {
	if f.Type == "string" {
		return path
	}
	return []string{path}
}

// mergeContextField sets one field inside an MCP's context blob, leaving
// every other field alone — replacing the whole blob would destroy any
// field an operator can set beside a derived one (ADR-011 decision 6).
func mergeContextField(base json.RawMessage, name string, value json.RawMessage) json.RawMessage {
	m := ContextValues(base)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m[name] = value
	out, err := json.Marshal(m)
	if err != nil {
		return base
	}
	return out
}

func disableToolByDefault(proj *config.Project, mcpID, tool string) {
	if !slices.Contains(proj.DisabledTools[mcpID], tool) {
		proj.DisabledTools[mcpID] = append(proj.DisabledTools[mcpID], tool)
	}
}

// ValidateGrants refuses a grant that would leave an MCP with no
// usable tools (ADR-011 decision 5): a remote-kind record has no Path, so
// a source: "project_path" field cannot be supplied, and by ADR-011
// decision 4 every tool that field governs then refuses. If the field's
// applies_to covers every tool the MCP exposes, the grant buys nothing and
// is refused; if it covers only some, the grant is permitted and precisely
// those tools lose out.
//
// This is a coherence check an operator sees at edit time, not the
// security boundary — that is syncProjectToken (above) and CallTool's
// presence re-check — so where the MCP's tool list is unknown (relay has
// never connected to it), this permits, and the call-time check still
// denies. Local projects are exempt: a path-scoped MCP granted to a local
// project is the expected case.
func ValidateGrants(proj *config.Project, surfaces McpSurfaces) error {
	if !proj.IsRemote() {
		return nil
	}
	for _, mcpID := range proj.AllowedMcpIDs {
		surface := surfaces[mcpID]
		schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)
		if schema.V2() {
			granted := grantedToolNames(proj, mcpID, surface.Tools)
			for _, f := range schema.ProjectPathFields() {
				if !f.GovernsAll(granted) {
					continue
				}
				return fmt.Errorf("remote project cannot be granted %q: its %q scope is derived from the project's path, it governs every tool this grant names, and a remote project has no path — the grant would leave no usable tools", mcpID, f.Name)
			}
			continue
		}
		// v1 compatibility: an MCP that declares allowed_dirs and no
		// version is refused outright, without consulting a tool list it
		// has no way to qualify.
		if schemaHasField(surface.Schema, V1AllowedDirsField) {
			return fmt.Errorf("remote project cannot be granted %q: it is a filesystem-scoped MCP (declares %s) and a remote project has no path to scope it to", mcpID, V1AllowedDirsField)
		}
	}
	return nil
}

// grantedToolNames narrows an MCP's live tool list to the ones this record
// may actually call (the same allowlist StoredToken.ToolAllowed applies at
// the chokepoint), because asking ValidateGrants's question against
// the MCP's WHOLE surface is the wrong set: macMCP declares file_dirs
// governing mail_save_attachment alone, so the answer over 47 tools is
// always no, even for a grant of exactly that one tool that can call
// nothing.
//
// A grant naming NO tool of this MCP falls back to the whole surface — the
// fail-closed direction, not a shortcut, since an incomplete profile
// (decision 9b) is the ordinary order of work and must not read as
// vacuously safe.
func grantedToolNames(proj *config.Project, mcpID string, all []string) []string {
	tok := config.StoredToken{ProjectKind: proj.Kind, AllowedTools: proj.AllowedTools}
	out := make([]string, 0, len(all))
	for _, name := range all {
		if tok.ToolAllowed(mcpID, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// schemaHasField is the V1 PATH ONLY — a v2 schema (contextSchemaVersion
// >= 2) never reaches here. It checks both the flat shape fsMCP ships and
// the JSON-Schema shape nested under "properties", because checking only
// one made the answer depend on how an MCP happened to spell the same
// declaration. Getting that wrong fails OPEN: a false negative silently
// permits a grant to an MCP that reads an absent allowlist as
// "unrestricted" (ADR-009).
func schemaHasField(schema json.RawMessage, field string) bool {
	if len(schema) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return false
	}
	if _, ok := fields[field]; ok {
		return true
	}
	// Checked after the flat lookup so a schema that genuinely declares a
	// field called "properties" is still matched by the flat rule first.
	props, ok := fields["properties"]
	if !ok {
		return false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(props, &nested); err != nil {
		return false
	}
	_, ok = nested[field]
	return ok
}
