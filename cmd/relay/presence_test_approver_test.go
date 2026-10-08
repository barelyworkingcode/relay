package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/testapprover"
	"github.com/barelyworkingcode/relay/internal/project"
)

func taGate(t *testing.T) *presence.Gate {
	t.Helper()
	return prGate(t, testapprover.New())
}

func TestRequireGate_TestApproverAuditsEveryGatedOp(t *testing.T) {
	for _, op := range presence.GatedOps {
		t.Run(op, func(t *testing.T) {
			rec := enabledIssuanceRecorder(t)
			attempt := presenceAttempt{auditor: issuanceAuditorOrNil(rec), via: auditViaCLI, subject: "p1"}
			grant, err := requireGate(taGate(t), context.Background(), op, singleStringDigest(op, "id", "p1"), "r", attempt)
			row := onlyPresenceRefusalRow(t, rec).ev

			if op == "project.grant" {
				assertNoErr(t, err, "requireGate project.grant")
				if row.Outcome != audit.AuditOutcomeOK || row.Method != op || row.PresenceApprover != "testapprover" ||
					row.PresenceID == "" || row.PresenceID != grant.ID() || row.Error != "" {
					t.Errorf("approval row = %+v, want ok, approver testapprover, presence_id %q", row, grant.ID())
				}
				return
			}
			if err == nil || grant.Valid() {
				t.Fatalf("requireGate(%s) = %v, grant valid %v; want a refusal", op, err, grant.Valid())
			}
			if row.Outcome != audit.AuditOutcomeDenied || row.Method != op || row.PresenceApprover != "testapprover" ||
				row.Error != testapprover.ErrNotAllowed.Error() || row.PresenceID != "" {
				t.Errorf("denial row = %+v, want denied by testapprover with %q", row, testapprover.ErrNotAllowed)
			}
		})
	}
}

func TestRequireGate_RefusalsBeforeTheProviderCarryNoApprover(t *testing.T) {
	op := "project.grant"
	d := singleStringDigest(op, "id", "p1")
	cases := []struct {
		name string
		gate func(t *testing.T) *presence.Gate
		ctx  context.Context
	}{
		{"no session", taGate, presence.WithCallerSession(context.Background(), presence.CallerSession{GraphicAccess: false})},
		{"nil gate", func(*testing.T) *presence.Gate { return nil }, context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := enabledIssuanceRecorder(t)
			attempt := presenceAttempt{auditor: issuanceAuditorOrNil(rec), via: auditViaCLI, subject: "p1"}
			if _, err := requireGate(tc.gate(t), tc.ctx, op, d, "r", attempt); err == nil {
				t.Fatal("requireGate succeeded, want a refusal")
			}
			row := onlyPresenceRefusalRow(t, rec)
			if _, has := row.raw["presence_approver"]; has || row.ev.Outcome != audit.AuditOutcomeDenied {
				t.Errorf("row = %s; want a denial with no presence_approver", row.line)
			}
		})
	}
}

func TestProjectOpsCreate_BrokenSinkRefusesAnApprovedGrant(t *testing.T) {
	store := newCLISandboxStore(t)
	rec := enabledIssuanceRecorder(t)
	assertNoErr(t, rec.CloseWriterForTest(), "CloseWriterForTest")
	ops := &ProjectOps{Store: store, Gate: taGate(t), Issuance: issuanceAuditorOrNil(rec)}

	_, err := ops.Create(context.Background(), project.CreateFields{Name: "acme", Path: t.TempDir()}, project.McpSurfaces{}, auditViaHTTP, "")
	if err == nil {
		t.Fatal("Create succeeded with an approval that could not be recorded")
	}
	if n := len(store.Get().Projects); n != 0 {
		t.Fatalf("project count = %d, want 0: an unrecorded approval must not act", n)
	}
}

func TestPostProjects_TestApproverRowsShareOnePresenceID(t *testing.T) {
	dir, store := aiHome(t)
	rec := aiRecorderAt(t, filepath.Join(dir, "rec.jsonl"), nil)
	var bearer string
	assertNoErr(t, store.With(func(s *config.Settings) {
		var err error
		_, bearer, err = mintAPICredentialForever(s, "ta-operator", []control.CapabilityClass{control.ClassRead, control.ClassConfigure})
		assertNoErr(t, err, "mint")
	}), "store.With")

	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket,
		Authz: NewCredentialAuthorizer(store), Auditor: audit.ControlAuditorOrNil(rec)}
	RegisterProjectRoutes(rr, store, &ProjectOps{Store: store, Gate: taGate(t), Issuance: issuanceAuditorOrNil(rec)}, mcpbroker.NewManager(nil), nil, nil, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, body := doJSONAuth(t, "POST", srv.URL+"/api/projects", map[string]any{"name": "verify-1a2b", "path": t.TempDir()}, bearer)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("POST /api/projects: status %d, body %s", resp.StatusCode, body)
	}

	var route, approval, change *audit.AuditEvent
	events := readLoggedEvents(t, rec)
	for i := range events {
		ev := &events[i]
		switch {
		case ev.Event == audit.AuditEventControlDecision && ev.Path == "/api/projects":
			route = ev
		case ev.Event == audit.AuditEventControlDecision && ev.Via != "":
			approval = ev
		case ev.Event == audit.AuditEventConfigChange:
			change = ev
		}
	}
	if route == nil || approval == nil || change == nil {
		t.Fatalf("want a route row, an approval row and a config_change; got %+v", events)
	}
	if approval.Outcome != audit.AuditOutcomeOK || approval.Method != "project.grant" || approval.Via != "http" ||
		approval.PresenceApprover != "testapprover" || approval.PresenceID == "" {
		t.Errorf("approval row = %+v", *approval)
	}
	if change.PresenceID != approval.PresenceID {
		t.Errorf("config_change presence_id = %q, approval presence_id = %q; want equal", change.PresenceID, approval.PresenceID)
	}
}

func TestAuditDetail_ShowsTheApproverWhenSet(t *testing.T) {
	denied := audit.AuditEvent{Event: audit.AuditEventControlDecision, Outcome: audit.AuditOutcomeDenied,
		Method: "credential.mint", Via: "cli", Error: "presence was refused by the test approver"}
	if got, want := auditDetail(denied), "credential.mint  via=cli  presence was refused by the test approver"; got != want {
		t.Errorf("detail without approver = %q, want %q byte for byte", got, want)
	}
	denied.PresenceApprover = "testapprover"
	if got, want := auditDetail(denied), "credential.mint  via=cli  presence was refused by the test approver  approver=testapprover"; got != want {
		t.Errorf("detail with approver = %q, want %q", got, want)
	}

	approved := audit.AuditEvent{Event: audit.AuditEventControlDecision, Outcome: audit.AuditOutcomeOK,
		Method: "project.grant", Subject: "verify-1a2b", Via: "http", PresenceApprover: "testapprover"}
	if got, want := auditDetail(approved), "project.grant  verify-1a2b  via=http  approver=testapprover"; got != want {
		t.Errorf("approval detail = %q, want %q", got, want)
	}
}
