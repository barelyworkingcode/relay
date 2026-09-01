package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

// fsmcpV1Schema is fsMCP as shipped: the flat declaration, no
// contextSchemaVersion, and therefore no keywords at all. The whole of what
// relay can read off it is that the field is there.
const fsmcpV1Schema = `{"allowed_dirs": {"type": "array", "items": {"type": "string"}}}`

func fsTools() []mcp.Tool { return simpleTools("fs_read", "fs_write", "fs_list") }

// fsProfile builds a grant on a v1 filesystem MCP with every other layer wide
// open, so the only thing that can refuse is the one under test.
func fsProfile(t *testing.T, kind config.ProjectKind, values map[string]json.RawMessage) *appRouter {
	t.Helper()
	return newProfileRouter(t, profileOpts{
		kind:          kind,
		tools:         fsTools(),
		allowedTools:  map[string][]string{"macmcp": {"fs_*"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: values,
		schema:        fsmcpV1Schema,
		schemaVersion: 0,
	})
}

func TestCallTool_AProfileCannotForgeAV1FilesystemScope(t *testing.T) {
	r := fsProfile(t, config.ProjectKindRemote, map[string]json.RawMessage{
		v1AllowedDirsField: json.RawMessage(`["/Users/admin/.ssh"]`),
	})
	for _, tool := range []string{"fs_read", "fs_write", "fs_list"} {
		_, err := r.CallTool(context.Background(), tool, json.RawMessage(`{}`), testToken)
		if err == nil {
			t.Fatalf("%s ran under a hand-written filesystem scope on an access profile", tool)
		}
		if !strings.Contains(err.Error(), v1AllowedDirsField) {
			t.Errorf("%s: refusal does not name the field: %v", tool, err)
		}
	}
}

// TestCallTool_TheV1RefusalDoesNotDependOnAValueBeingThere is the other half of
// why this is a refusal rather than a strip: for a v1 filesystem MCP an ABSENT
// allowed_dirs is precisely what fsMCP reads as unrestricted, so removing the
// forged value and letting the call through would turn a confinement relay
// disbelieves into no confinement at all.
func TestCallTool_TheV1RefusalDoesNotDependOnAValueBeingThere(t *testing.T) {
	r := fsProfile(t, config.ProjectKindRemote, nil)
	if _, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("a profile reached a v1 filesystem MCP with no scope at all")
	}
}

// TestCallTool_AProfileCannotForgeAV2ProjectPathScope is the same forgery under
// v2, where the presence check WOULD have been satisfied by the forged value —
// it is present and non-empty, which is all that check asks.
func TestCallTool_AProfileCannotForgeAV2ProjectPathScope(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindRemote,
		allowedTools:  map[string][]string{"macmcp": {"mail_*"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
		contextValues: map[string]json.RawMessage{
			"mail_accounts":  json.RawMessage(`["Bob"]`),
			"mail_mailboxes": json.RawMessage(`["INBOX"]`),
			"file_dirs":      json.RawMessage(`["/Users/admin/.ssh"]`),
		},
		schema:        macmcpSchema,
		schemaVersion: 2,
	})
	if _, err := r.CallTool(context.Background(), "mail_save_attachment", json.RawMessage(`{}`), testToken); err == nil {
		t.Fatal("mail_save_attachment ran under a hand-written file_dirs on an access profile")
	} else if !strings.Contains(err.Error(), "file_dirs") {
		t.Errorf("refusal does not name the field: %v", err)
	}
	// And the tools that field does not govern are untouched: this is a
	// per-field refusal, not a per-MCP one.
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("an ungoverned tool was refused: %v", err)
	}
}

func TestCallTool_ALocalProjectStillUsesItsV1Scope(t *testing.T) {
	r := fsProfile(t, config.ProjectKindLocal, map[string]json.RawMessage{
		v1AllowedDirsField: json.RawMessage(`["/tmp/test"]`),
	})
	if _, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("a local project was refused its own derived filesystem scope: %v", err)
	}
}

func TestAudit_AV1CallRecordsTheScopeItWasConfinedBy(t *testing.T) {
	r := fsProfile(t, config.ProjectKindLocal, map[string]json.RawMessage{
		v1AllowedDirsField: json.RawMessage(`["/tmp/test"]`),
	})
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got, ok := events[0].Scope[v1AllowedDirsField]
	if !ok {
		t.Fatalf("a v1 call recorded scope %v; the value relay injected is missing", events[0].Scope)
	}
	if string(got) != `["/tmp/test"]` {
		t.Errorf("recorded scope = %s, want the injected value", got)
	}
}

// TestAudit_AMcpWithNoScopeConceptStillRecordsNone keeps the other half of the
// distinction: `null` has to go on meaning "this MCP scopes nothing", or the
// field answers no question at all.
func TestAudit_AMcpWithNoScopeConceptStillRecordsNone(t *testing.T) {
	r := newProfileRouter(t, profileOpts{
		kind:          config.ProjectKindLocal,
		tools:         fsTools(),
		allowedTools:  map[string][]string{"macmcp": {"fs_*"}},
		access:        map[string]string{"macmcp": config.AccessWrite},
		allowExternal: map[string]bool{"macmcp": true},
	})
	rec := newTestAudit(t, nil)
	r.audit = rec

	if _, err := r.CallTool(context.Background(), "fs_read", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	events := readLoggedEvents(t, rec)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Scope != nil {
		t.Errorf("an MCP declaring no schema recorded scope %v, want none", events[0].Scope)
	}
}
