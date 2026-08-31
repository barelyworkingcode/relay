package main

// Issue #91: the wire semantics of NarrowGrant's three per-MCP maps
// (allowed_tools, access, allow_external) is whole-map REPLACE, not merge —
// applyProjectUpdate's candidate.AllowedTools = *f.AllowedTools (and the
// same for Access/AllowExternal) discards any key the request did not
// name. narrowUpdateFields built that request map from only the ids the
// caller named, so narrowing one MCP of a multi-MCP grant silently
// stripped every other MCP down to zero tools, read-default access and
// external-default — an unrequested narrowing of a grant the caller never
// mentioned. These tests pin the fix: a key the request does not touch
// keeps its stored value.

import (
	"context"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// TestNarrowForEnrolment_UntouchedMcpKeepsItsStoredGrant is RED against the
// pre-fix code: narrowing macmcp alone must leave "other"'s allowed_tools,
// access and allow_external exactly as stored, not wiped to empty by a
// whole-map replace built from only the id the request named.
func TestNarrowForEnrolment_UntouchedMcpKeepsItsStoredGrant(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	assertNoErr(t, store.With(func(s *Settings) {
		p, _ := s.findProjectByID(mail.ID)
		if p == nil {
			t.Fatal("seeded project vanished")
		}
		p.AllowedMcpIDs = []string{"macmcp", "other"}
		p.AllowedTools = map[string][]string{
			"macmcp": {"mail_search", "mail_send"},
			"other":  {"other_tool"},
		}
		p.Access = map[string]string{"macmcp": AccessWrite, "other": AccessWrite}
		p.AllowExternal = map[string]bool{"macmcp": true, "other": true}
	}), "widen behind the guards")

	ops := &ProjectOps{Store: store, Issuance: pgwWithIssuance(t), OnChange: func() {}}
	surfaces := func() McpSurfaces { return McpSurfaces{"macmcp": macmcpSurface()} }
	caller := bridge.RemoteCaller{ClientID: "hermes-mail", Fingerprint: "sha256:" + strings.Repeat("a", 64)}

	tools := map[string][]string{"macmcp": {"mail_search"}}
	access := map[string]string{"macmcp": AccessRead}
	external := map[string]bool{"macmcp": false}
	_, changed, err := ops.NarrowForEnrolment(context.Background(), mail.ID,
		remoteNarrowFields{AllowedTools: &tools, Access: &access, AllowExternal: &external},
		caller, surfaces)
	assertNoErr(t, err, "narrow macmcp only")
	if len(changed) == 0 {
		t.Fatal("expected a genuine narrowing to report changed fields")
	}

	proj, _ := store.Get().findProjectByID(mail.ID)
	if proj == nil {
		t.Fatal("the project vanished")
	}

	if got := proj.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_search" {
		t.Errorf("stored allowed_tools[macmcp] = %v, want [mail_search]", got)
	}
	if got := proj.AllowedTools["other"]; len(got) != 1 || got[0] != "other_tool" {
		t.Errorf("stored allowed_tools[other] = %v, want [other_tool] unchanged — narrowing macmcp must not touch an MCP the request never named", got)
	}

	if got := proj.Access["macmcp"]; got != AccessRead {
		t.Errorf("stored access[macmcp] = %q, want %q", got, AccessRead)
	}
	if got := proj.Access["other"]; got != AccessWrite {
		t.Errorf("stored access[other] = %q, want %q unchanged", got, AccessWrite)
	}

	if got := proj.AllowExternal["macmcp"]; got != false {
		t.Errorf("stored allow_external[macmcp] = %v, want false", got)
	}
	if got := proj.AllowExternal["other"]; got != true {
		t.Errorf("stored allow_external[other] = %v, want true unchanged", got)
	}
}

// TestNarrowForEnrolment_ReplaySafeUnderMergeSemantics re-establishes the
// replay-safety property NarrowGrant depends on (it is marked replay-safe
// in presence.GatedOps' surrounding docs) now that narrowUpdateFields
// merges per key instead of replacing the whole map: narrowingIsNoop must
// compare the MERGED result against stored, not the bare request map,
// or a request that only names one already-narrowed MCP of a two-MCP
// grant would never read back as a no-op (the request map alone never
// equals the stored map, which also holds the untouched MCP's entries) —
// resending it would rewrite settings.json and re-append a config_change
// every time.
func TestNarrowForEnrolment_ReplaySafeUnderMergeSemantics(t *testing.T) {
	dir, store := newEnrolmentSandbox(t)
	mail := mkStoreProject(t, store, ProjectKindRemote, "Mail", "")

	assertNoErr(t, store.With(func(s *Settings) {
		p, _ := s.findProjectByID(mail.ID)
		if p == nil {
			t.Fatal("seeded project vanished")
		}
		p.AllowedMcpIDs = []string{"macmcp", "other"}
		p.AllowedTools = map[string][]string{
			"macmcp": {"mail_search", "mail_send"},
			"other":  {"other_tool"},
		}
		p.Access = map[string]string{"macmcp": AccessWrite, "other": AccessWrite}
		p.AllowExternal = map[string]bool{"macmcp": true, "other": true}
	}), "widen behind the guards")

	rec := enabledIssuanceRecorder(t)
	ops := &ProjectOps{Store: store, Issuance: issuanceAuditorOrNil(rec), OnChange: func() {}}
	surfaces := func() McpSurfaces { return McpSurfaces{"macmcp": macmcpSurface()} }
	caller := bridge.RemoteCaller{ClientID: "hermes-mail", Fingerprint: "sha256:" + strings.Repeat("a", 64)}

	narrow := func() (Project, []string, error) {
		tools := map[string][]string{"macmcp": {"mail_search"}}
		access := map[string]string{"macmcp": AccessRead}
		external := map[string]bool{"macmcp": false}
		return ops.NarrowForEnrolment(context.Background(), mail.ID,
			remoteNarrowFields{AllowedTools: &tools, Access: &access, AllowExternal: &external},
			caller, surfaces)
	}

	_, changed, err := narrow()
	assertNoErr(t, err, "first narrowing")
	if len(changed) == 0 {
		t.Fatal("first narrowing reported no changed fields, want at least one")
	}

	countConfigChanges := func() int {
		n := 0
		for _, e := range readLoggedEvents(t, rec) {
			if e.Event == AuditEventConfigChange && e.Credential == auditCredentialProjectGrant {
				n++
			}
		}
		return n
	}
	if got := countConfigChanges(); got != 1 {
		t.Fatalf("config_change count after the first narrowing = %d, want 1", got)
	}

	before := odwSnap(t, dir)

	for i := 0; i < 5; i++ {
		_, changed, err := narrow()
		assertNoErr(t, err, "replay #%d", i)
		if len(changed) != 0 {
			t.Errorf("replay #%d reported changed = %v, want none — this must read back as a no-op", i, changed)
		}
	}

	before.assertUntouched(t, dir, "five identical resends of an already-applied narrowing, under merge semantics")
	if got := countConfigChanges(); got != 1 {
		t.Fatalf("config_change count after five identical resends = %d, want still 1", got)
	}
}
