package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/project"
)

// ctxGateSeed creates a project through project.ApplyCreate against a store
// whose ExternalMcps match surfaces, so any project_path field is derived
// exactly as it is for a real record.
func ctxGateSeed(t *testing.T, f project.CreateFields, surfaces project.McpSurfaces) (config.SettingsStore, config.Project) {
	t.Helper()
	_, store := pgwSandbox(t)
	ids := make([]string, 0, len(surfaces))
	for id := range surfaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var created config.Project
	var createErr error
	if err := store.With(func(s *config.Settings) {
		for _, id := range ids {
			s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{ID: id, DisplayName: id})
		}
		created, createErr = project.ApplyCreate(s, f, surfaces)
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if createErr != nil {
		t.Fatalf("ApplyCreate: %v", createErr)
	}
	return store, created
}

// ctxGateHarvest opens stored in the Settings form, runs edit (may be empty),
// and returns the harvested payload decoded as the IPC update message.
func ctxGateHarvest(t *testing.T, store config.SettingsStore, stored config.Project, surfaces project.McpSurfaces, edit string) ipcUpdateProjectMsg {
	t.Helper()
	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	vm := newAppVM(t)
	raw := evalString(t, vm, `(function(){
		window.webkit = { messageHandlers: { ipc: { postMessage: function(){} } } };
		window.state.page = 'projects';
		window.state.externalMcps = `+mustJSON(externalMcpsToNativeView(store.Get().ExternalMcps))+`;
		window.state.mcpScopeFields = `+mustJSON(surfaces.ScopeFields())+`;
		window.state.projects = [`+mustJSON(projectToNativeView(stored))+`];
		window.editProject('`+stored.ID+`');
		`+edit+`
		return JSON.stringify(window.harvestProjectForm());
	})()`)
	var msg ipcUpdateProjectMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("harvest did not decode as update_project: %v\n%s", err, raw)
	}
	if msg.Context == nil {
		t.Fatalf("harvest omitted context, so the save would not exercise the gate:\n%s", raw)
	}
	msg.ID = stored.ID
	return msg
}

func ctxGateOps(t *testing.T, store config.SettingsStore, provider presence.Provider) *ProjectOps {
	t.Helper()
	gate, err := presence.NewGate(provider)
	assertNoErr(t, err, "NewGate")
	return &ProjectOps{Store: store, Gate: gate, Issuance: pgwWithIssuance(t)}
}

func decodedContext(t *testing.T, store config.SettingsStore, id string) map[string]any {
	t.Helper()
	p, _ := config.FindProjectByID(store.Get(), id)
	if p == nil {
		t.Fatalf("project %s missing", id)
	}
	out := map[string]any{}
	for mcp, raw := range p.Context {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("stored context[%s] does not decode: %v", mcp, err)
		}
		out[mcp] = v
	}
	return out
}

func ctxMap(mcp, blob string) map[string]json.RawMessage {
	return map[string]json.RawMessage{mcp: json.RawMessage(blob)}
}

func TestSettingsForm_UnchangedSaveDoesNotPrompt(t *testing.T) {
	cases := []struct {
		name     string
		fields   project.CreateFields
		surfaces project.McpSurfaces
	}{
		{"local macmcp with mail_accounts", project.CreateFields{
			Name: "Mail", Kind: config.ProjectKindLocal, AllowedMcpIDs: []string{"macmcp"},
			Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
		}, project.McpSurfaces{"macmcp": macmcpSurface()}},
		{"local fsmcp v2", project.CreateFields{
			Name: "Files", Kind: config.ProjectKindLocal, AllowedMcpIDs: []string{"fsmcp"},
		}, project.McpSurfaces{"fsmcp": fsmcpSurface()}},
		{"local wildcard with both MCPs", project.CreateFields{
			Name: "Everything", Kind: config.ProjectKindLocal, AllowedMcpIDs: []string{"*"},
		}, project.McpSurfaces{"macmcp": macmcpSurface(), "fsmcp": fsmcpSurface()}},
		{"local MCP without a schema", project.CreateFields{
			Name: "Quiet", Kind: config.ProjectKindLocal, AllowedMcpIDs: []string{"quietmcp"},
		}, project.McpSurfaces{"quietmcp": {Tools: []string{"quiet_ping"}}}},
		{"remote profile with operator fields", project.CreateFields{
			Name: "Acme inbox", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
			AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
			Access:       map[string]string{"macmcp": "read"},
			Context:      ctxMap("macmcp", `{"mail_accounts":["Bob"],"mail_mailboxes":["INBOX"]}`),
		}, project.McpSurfaces{"macmcp": macmcpSurface()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.fields.Kind == config.ProjectKindLocal {
				c.fields.Path = t.TempDir()
			}
			store, stored := ctxGateSeed(t, c.fields, c.surfaces)
			before := decodedContext(t, store, stored.ID)
			msg := ctxGateHarvest(t, store, stored, c.surfaces, "")

			_, _, err := ctxGateOps(t, store, presencetest.Deny()).Update(context.Background(), msg.ID, msg.UpdateFields,
				func() project.McpSurfaces { return c.surfaces }, auditViaIPC, "")
			if err != nil {
				t.Fatalf("an unchanged Settings save must not reach the gate: %v", err)
			}
			if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("stored context changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestSettingsForm_EditedScopeValueStillPrompts(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Mail", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	msg := ctxGateHarvest(t, store, stored, surfaces, `window.setProjScopeText('macmcp', 'mail_accounts', 'Alice\nBob');`)

	rec := presencetest.NewRecording(nil)
	if _, _, err := ctxGateOps(t, store, rec).Update(context.Background(), msg.ID, msg.UpdateFields,
		func() project.McpSurfaces { return surfaces }, auditViaIPC, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reasons := rec.Reasons()
	if len(reasons) != 1 {
		t.Fatalf("expected exactly one presence prompt, got %v", reasons)
	}
	r := reasons[0]
	lp, rp := strings.LastIndex(r, "("), strings.LastIndex(r, ")")
	if lp < 0 || rp < lp {
		t.Fatalf("reason %q carries no field list", r)
	}
	if named := strings.Split(r[lp+1:rp], ", "); !reflect.DeepEqual(named, []string{"context"}) {
		t.Fatalf("reason %q names %v, want only context", r, named)
	}
}

func TestProjectOps_SchemaLostBeforeWriteRefusesContextResend(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Mail", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	before := decodedContext(t, store, stored.ID)
	msg := ctxGateHarvest(t, store, stored, surfaces, "")

	calls := 0
	losing := func() project.McpSurfaces {
		calls++
		if calls == 1 {
			return surfaces
		}
		return nil
	}
	_, _, err := ctxGateOps(t, store, presencetest.Deny()).Update(context.Background(), msg.ID, msg.UpdateFields, losing, auditViaIPC, "")
	if !errors.Is(err, errProjectChangedDuringApproval) {
		t.Fatalf("schema lost before the write: err = %v, want errProjectChangedDuringApproval", err)
	}
	if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused update still wrote context:\nbefore %v\nafter  %v", before, after)
	}
}

func TestProjectOps_RecheckAndWriteShareOneSurfacesFetch(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Mail", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	before := decodedContext(t, store, stored.ID)
	if _, ok := before["macmcp"].(map[string]any)["file_dirs"]; !ok {
		t.Fatalf("seed did not derive file_dirs: %v", before)
	}
	msg := ctxGateHarvest(t, store, stored, surfaces, "")

	calls := 0
	twice := func() project.McpSurfaces {
		calls++
		if calls <= 2 {
			return surfaces
		}
		return nil
	}
	if _, _, err := ctxGateOps(t, store, presencetest.Deny()).Update(context.Background(), msg.ID, msg.UpdateFields, twice, auditViaIPC, ""); err != nil {
		t.Fatalf("an unchanged save must succeed when the schema holds for the gate and the queued step: %v", err)
	}
	after := decodedContext(t, store, stored.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stored context changed, so the write saw a different schema than the recheck:\nbefore %v\nafter  %v", before, after)
	}
}

func TestProjectOps_UpdateWithoutContextFetchesNoSurfaces(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Mail", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)

	calls := 0
	counting := func() project.McpSurfaces { calls++; return surfaces }
	name := "Renamed"
	if _, _, err := ctxGateOps(t, store, presencetest.Deny()).Update(context.Background(), stored.ID,
		project.UpdateFields{Name: &name}, counting, auditViaIPC, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 0 {
		t.Fatalf("a name-only update fetched surfaces %d times, want 0", calls)
	}
}

func TestProjectOps_ApprovedContextEditSurvivesSchemaLossWithoutDerivedField(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Mail", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	if _, ok := decodedContext(t, store, stored.ID)["macmcp"].(map[string]any)["file_dirs"]; !ok {
		t.Fatalf("seed did not derive file_dirs")
	}
	msg := ctxGateHarvest(t, store, stored, surfaces, `window.setProjScopeText('macmcp', 'mail_accounts', 'Alice\nBob');`)

	calls := 0
	gateOnly := func() project.McpSurfaces {
		calls++
		if calls == 1 {
			return surfaces
		}
		return nil
	}
	rec := presencetest.NewRecording(nil)
	if _, _, err := ctxGateOps(t, store, rec).Update(context.Background(), msg.ID, msg.UpdateFields, gateOnly, auditViaIPC, ""); err != nil {
		t.Fatalf("an approved context edit must be written after the schema is lost: %v", err)
	}
	if rec.Calls() != 1 {
		t.Fatalf("expected the edit to be approved through one prompt, got %d", rec.Calls())
	}
	if calls < 2 {
		t.Fatalf("surfaces fetched %d times, so the queued step never saw the lost schema", calls)
	}
	mac, _ := decodedContext(t, store, stored.ID)["macmcp"].(map[string]any)
	if got := mac["mail_accounts"]; !reflect.DeepEqual(got, []any{"Alice", "Bob"}) {
		t.Fatalf("stored mail_accounts = %v, want [Alice Bob]", got)
	}
	if v, ok := mac["file_dirs"]; ok {
		t.Fatalf("stored context kept derived file_dirs = %v without a schema to derive it", v)
	}
}
