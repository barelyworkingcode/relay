package project

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// localWithDerivedContext creates a console project granting mcpID while
// surface is connected, so every project_path field is really derived.
func localWithDerivedContext(t *testing.T, mcpID string, surface McpSurface, operator map[string]json.RawMessage, derivedField string) (*config.Settings, string) {
	t.Helper()
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: mcpID}}}
	created, err := ApplyCreate(s, CreateFields{
		Name: "Acme", Path: t.TempDir(), AllowedMcpIDs: []string{mcpID}, Context: operator,
	}, McpSurfaces{mcpID: surface})
	assertNoErr(t, err, "create local project")
	stored, _ := config.FindProjectByID(s, created.ID)
	if _, ok := ContextValues(stored.Context[mcpID])[derivedField]; !ok {
		t.Fatalf("precondition: %s was not derived onto the local project: %s", derivedField, stored.Context[mcpID])
	}
	return s, created.ID
}

func toRemote() UpdateFields {
	remote, empty := config.ProjectKindRemote, ""
	return UpdateFields{Kind: &remote, Path: &empty, DisabledTools: &map[string][]string{}}
}

func surfacesOf(sc McpSurfaces) func() McpSurfaces { return func() McpSurfaces { return sc } }

// storedJSON snapshots the fields a conversion touches as bytes, because a
// struct copy shares Context and DisabledTools with the stored record.
func storedJSON(t *testing.T, s *config.Settings, id string) []byte {
	t.Helper()
	proj, _ := config.FindProjectByID(s, id)
	out, err := json.Marshal(struct {
		Kind          config.ProjectKind
		Path          string
		AllowedMcpIDs []string
		Context       map[string]json.RawMessage
		DisabledTools map[string][]string
	}{proj.Kind, proj.Path, proj.AllowedMcpIDs, proj.Context, proj.DisabledTools})
	assertNoErr(t, err, "marshal stored project")
	return out
}

func aliceContext() map[string]json.RawMessage {
	return map[string]json.RawMessage{"macmcp": json.RawMessage(`{"mail_accounts":["Alice"]}`)}
}

func TestApplyUpdate_LocalToRemoteDropsDerivedField(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator map[string]json.RawMessage
	}{
		{"with an operator field", aliceContext()},
		{"derived only", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := localWithDerivedContext(t, "macmcp", macmcpSurface(), tc.operator, "file_dirs")

			_, _, err := ApplyUpdate(s, id, toRemote(), surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
			assertNoErr(t, err, "convert to remote")

			after, _ := config.FindProjectByID(s, id)
			if !after.IsRemote() {
				t.Fatal("project did not convert to remote")
			}
			values := ContextValues(after.Context["macmcp"])
			if raw, ok := values["file_dirs"]; ok {
				t.Fatalf("stored context still holds file_dirs after conversion: %s", raw)
			}
			if tc.operator == nil {
				if raw, ok := after.Context["macmcp"]; ok {
					t.Errorf("an entry left with no fields was kept: %s", raw)
				}
				return
			}
			if string(values["mail_accounts"]) != `["Alice"]` {
				t.Errorf("the operator field did not survive: %s", after.Context["macmcp"])
			}
		})
	}
}

func TestApplyUpdate_LocalToRemoteRefusedWhileCarriedContextHasNoSchema(t *testing.T) {
	v1 := McpSurface{Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}

	t.Run("refused naming the MCP, nothing changes", func(t *testing.T) {
		for _, tc := range []struct {
			mcpID, derived string
			surface        McpSurface
			operator       map[string]json.RawMessage
		}{
			{"macmcp", "file_dirs", macmcpSurface(), aliceContext()},
			{"fsmcp", V1AllowedDirsField, v1, nil},
		} {
			t.Run(tc.mcpID, func(t *testing.T) {
				s, id := localWithDerivedContext(t, tc.mcpID, tc.surface, tc.operator, tc.derived)
				before := storedJSON(t, s, id)

				_, found, err := ApplyUpdate(s, id, toRemote(), surfacesOf(McpSurfaces{}))
				if err == nil {
					t.Fatalf("conversion was not refused while %s is not connected", tc.mcpID)
				}
				if !found || !strings.Contains(err.Error(), tc.mcpID) || !strings.Contains(err.Error(), "not connected") {
					t.Errorf("refusal must name %q and say it is not connected (found=%v): %v", tc.mcpID, found, err)
				}
				if after := storedJSON(t, s, id); !bytes.Equal(before, after) {
					t.Errorf("a refused conversion changed the record:\nbefore %s\nafter  %s", before, after)
				}
			})
		}
	})

	t.Run("a connected MCP's derived field survives another MCP's refusal", func(t *testing.T) {
		s, id := localWithDerivedContext(t, "macmcp", macmcpSurface(), aliceContext(), "file_dirs")
		stored, _ := config.FindProjectByID(s, id)
		s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{ID: "notesmcp"})
		stored.AllowedMcpIDs = append(stored.AllowedMcpIDs, "notesmcp")
		stored.Context["notesmcp"] = json.RawMessage(`{"notebooks":["Acme"]}`)
		before := storedJSON(t, s, id)

		_, found, err := ApplyUpdate(s, id, toRemote(), surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
		if err == nil {
			t.Fatal("conversion was not refused while notesmcp is not connected")
		}
		if !found || !strings.Contains(err.Error(), "notesmcp") || !strings.Contains(err.Error(), "not connected") {
			t.Errorf("refusal must name notesmcp and say it is not connected (found=%v): %v", found, err)
		}
		if after := storedJSON(t, s, id); !bytes.Equal(before, after) {
			t.Errorf("a refused conversion changed the record:\nbefore %s\nafter  %s", before, after)
		}
	})

	for _, tc := range []struct {
		name   string
		escape func(*UpdateFields)
		want   func(t *testing.T, ctx map[string]json.RawMessage)
	}{
		{
			name:   "MCP removed from allowed_mcp_ids",
			escape: func(f *UpdateFields) { f.AllowedMcpIDs = &[]string{} },
			want: func(t *testing.T, ctx map[string]json.RawMessage) {
				if len(ctx) != 0 {
					t.Errorf("context for an ungranted MCP survived: %v", ctx)
				}
			},
		},
		{
			name: "context sent in the same request",
			escape: func(f *UpdateFields) {
				sent := map[string]json.RawMessage{"macmcp": json.RawMessage(`{"mail_accounts":["Bob"]}`)}
				f.Context = &sent
			},
			want: func(t *testing.T, ctx map[string]json.RawMessage) {
				if got := string(ctx["macmcp"]); got != `{"mail_accounts":["Bob"]}` {
					t.Errorf("stored context is not the value sent: %s", got)
				}
			},
		},
	} {
		t.Run("escape: "+tc.name, func(t *testing.T) {
			s, id := localWithDerivedContext(t, "macmcp", macmcpSurface(), aliceContext(), "file_dirs")
			f := toRemote()
			tc.escape(&f)

			_, _, err := ApplyUpdate(s, id, f, surfacesOf(McpSurfaces{}))
			assertNoErr(t, err, "convert with the escape applied")

			after, _ := config.FindProjectByID(s, id)
			if !after.IsRemote() {
				t.Fatal("project did not convert to remote")
			}
			tc.want(t, after.Context)
		})
	}
}

func TestApplyUpdate_ConnectedMcpWithoutSchemaDoesNotBlockConversion(t *testing.T) {
	s := &config.Settings{
		ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}},
		Projects: []config.Project{{
			ID: "p1", Name: "Acme", Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp"},
			Context: aliceContext(),
		}},
	}
	connectedNoSchema := McpSurfaces{"macmcp": {Tools: macmcpSurface().Tools}}

	_, _, err := ApplyUpdate(s, "p1", toRemote(), surfacesOf(connectedNoSchema))
	assertNoErr(t, err, "convert with a connected MCP that declares no schema")

	after, _ := config.FindProjectByID(s, "p1")
	if !after.IsRemote() {
		t.Fatal("project did not convert to remote")
	}
	if got := string(ContextValues(after.Context["macmcp"])["mail_accounts"]); got != `["Alice"]` {
		t.Errorf("the operator field did not survive: %s", after.Context["macmcp"])
	}
}

func TestApplyUpdate_RemoteOrHostedRecordDropsStaleDerivedFieldOnAnyEdit(t *testing.T) {
	staleRemoteGranting := func(mcpID string, ctx map[string]json.RawMessage) *config.Settings {
		return &config.Settings{
			ExternalMcps: []config.ExternalMcp{{ID: mcpID}},
			Projects: []config.Project{{
				ID: "p1", Name: "Acme", Kind: config.ProjectKindRemote,
				AllowedMcpIDs: []string{mcpID}, Context: ctx,
			}},
		}
	}
	staleRemote := func(ctx map[string]json.RawMessage) *config.Settings { return staleRemoteGranting("macmcp", ctx) }
	stale := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"macmcp": json.RawMessage(`{"file_dirs":["/srv/acme"],"mail_accounts":["Alice"]}`),
		}
	}
	rename := "Acme renamed"

	staleHosted := func() *config.Settings {
		return &config.Settings{
			ExternalMcps: []config.ExternalMcp{{ID: "macmcp"}},
			Hosts:        []config.Host{{ID: "h_win", Name: "winhost"}},
			Projects: []config.Project{{
				ID: "p1", Name: "Acme", HostID: "h_win", Path: "/home/acme/work", Context: stale(),
			}},
		}
	}

	for _, tc := range []struct {
		kind   string
		record func() *config.Settings
	}{
		{"remote", func() *config.Settings { return staleRemote(stale()) }},
		{"hosted", staleHosted},
	} {
		t.Run("schema known: the rename drops the field: "+tc.kind, func(t *testing.T) {
			s := tc.record()
			_, _, err := ApplyUpdate(s, "p1", UpdateFields{Name: &rename}, surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
			assertNoErr(t, err, "rename")

			after, _ := config.FindProjectByID(s, "p1")
			values := ContextValues(after.Context["macmcp"])
			if raw, ok := values["file_dirs"]; ok {
				t.Fatalf("a %s record still holds file_dirs after an edit: %s", tc.kind, raw)
			}
			if string(values["mail_accounts"]) != `["Alice"]` {
				t.Errorf("the operator field did not survive: %s", after.Context["macmcp"])
			}
		})

		t.Run("schema known: a permissions edit drops the field: "+tc.kind, func(t *testing.T) {
			s := tc.record()
			access := map[string]string{"macmcp": "write"}
			_, _, err := ApplyUpdate(s, "p1", UpdateFields{Access: &access}, surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
			assertNoErr(t, err, "access edit")

			after, _ := config.FindProjectByID(s, "p1")
			// A hosted project cannot be granted macmcp, so its access entry
			// is pruned on store whatever happens to the context.
			if tc.kind == "remote" && after.Access["macmcp"] != "write" {
				t.Errorf("access not applied: %v", after.Access)
			}
			values := ContextValues(after.Context["macmcp"])
			if raw, ok := values["file_dirs"]; ok {
				t.Fatalf("a %s record still holds file_dirs after a permissions edit: %s", tc.kind, raw)
			}
			if string(values["mail_accounts"]) != `["Alice"]` {
				t.Errorf("the operator field did not survive: %s", after.Context["macmcp"])
			}
		})
	}

	t.Run("v1 schema known: the rename drops allowed_dirs", func(t *testing.T) {
		s := staleRemoteGranting("fsmcp", map[string]json.RawMessage{
			"fsmcp": json.RawMessage(`{"allowed_dirs":["/srv/acme"]}`),
		})
		v1 := McpSurface{Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}
		_, _, err := ApplyUpdate(s, "p1", UpdateFields{Name: &rename}, surfacesOf(McpSurfaces{"fsmcp": v1}))
		assertNoErr(t, err, "rename")

		after, _ := config.FindProjectByID(s, "p1")
		if raw, ok := ContextValues(after.Context["fsmcp"])[V1AllowedDirsField]; ok {
			t.Fatalf("a remote record still holds allowed_dirs after an edit: %s", raw)
		}
	})

	t.Run("schema known: a refused permissions edit leaves context untouched", func(t *testing.T) {
		s := staleRemote(stale())
		before := append(json.RawMessage(nil), s.Projects[0].Context["macmcp"]...)

		access := map[string]string{"macmcp": "wrIte"}
		_, _, err := ApplyUpdate(s, "p1", UpdateFields{Access: &access}, surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
		if err == nil {
			t.Fatal("an access edit with an invalid level was accepted")
		}

		after, _ := config.FindProjectByID(s, "p1")
		if !bytes.Equal(before, after.Context["macmcp"]) {
			t.Errorf("a refused edit changed the stored context:\nbefore %s\nafter  %s", before, after.Context["macmcp"])
		}
	})

	t.Run("schema unknown: the rename leaves context untouched", func(t *testing.T) {
		s := staleRemote(stale())
		before := append(json.RawMessage(nil), s.Projects[0].Context["macmcp"]...)

		_, _, err := ApplyUpdate(s, "p1", UpdateFields{Name: &rename}, surfacesOf(McpSurfaces{}))
		assertNoErr(t, err, "rename while macmcp is not connected")

		after, _ := config.FindProjectByID(s, "p1")
		if after.Name != rename {
			t.Errorf("rename not applied: %q", after.Name)
		}
		if !bytes.Equal(before, after.Context["macmcp"]) {
			t.Errorf("context changed with no schema to judge it by:\nbefore %s\nafter  %s", before, after.Context["macmcp"])
		}
	})

	t.Run("no context: no surface fetch", func(t *testing.T) {
		s := staleRemote(nil)
		fetches := 0
		_, _, err := ApplyUpdate(s, "p1", UpdateFields{Name: &rename}, func() McpSurfaces {
			fetches++
			return McpSurfaces{"macmcp": macmcpSurface()}
		})
		assertNoErr(t, err, "rename")
		if fetches != 0 {
			t.Errorf("a rename of a record with no context fetched surfaces %d times", fetches)
		}
	})
}

func TestApplyUpdate_ConsoleToHostCarriesNoContext(t *testing.T) {
	s, id := localWithDerivedContext(t, "macmcp", macmcpSurface(), aliceContext(), "file_dirs")
	s.Hosts = []config.Host{{ID: "h_win", Name: "winhost"}}
	host := "h_win"

	_, _, err := ApplyUpdate(s, id, UpdateFields{
		HostID: &host, AllowedMcpIDs: &[]string{}, DisabledTools: &map[string][]string{},
	}, surfacesOf(McpSurfaces{"macmcp": macmcpSurface()}))
	assertNoErr(t, err, "move to host")

	after, _ := config.FindProjectByID(s, id)
	if !after.IsHosted() {
		t.Fatal("project did not move to the host")
	}
	if len(after.Context) != 0 {
		t.Errorf("a host project carries context: %v", after.Context)
	}
}
