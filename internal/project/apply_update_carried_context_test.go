package project

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

const (
	carriedPath     = "/work/acme"
	carriedNextPath = "/work/acme-next"
)

func carriedV1FsSurface() McpSurface {
	return McpSurface{Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)}
}

func carriedSettings(t *testing.T, f CreateFields, surfaces McpSurfaces) (*config.Settings, config.Project) {
	t.Helper()
	s := &config.Settings{Version: 1}
	ids := make([]string, 0, len(surfaces))
	for id := range surfaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{ID: id, DisplayName: id})
	}
	created, err := ApplyCreate(s, f, surfaces)
	if err != nil {
		t.Fatalf("ApplyCreate: %v", err)
	}
	return s, created
}

func carriedDecoded(t *testing.T, s *config.Settings, id, mcpID string) any {
	t.Helper()
	p, _ := config.FindProjectByID(s, id)
	if p == nil {
		t.Fatalf("project %s missing", id)
	}
	var v any
	if err := json.Unmarshal(p.Context[mcpID], &v); err != nil {
		t.Fatalf("stored context[%s] = %s does not decode: %v", mcpID, p.Context[mcpID], err)
	}
	return v
}

func carriedWant(t *testing.T, blob string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(blob), &v); err != nil {
		t.Fatalf("want %s: %v", blob, err)
	}
	return v
}

func TestApplyUpdate_PermissionOnlyPatchCarriesDerivedContext(t *testing.T) {
	kinds := []struct {
		name, mcpID, tool string
		surfaces          McpSurfaces
		context           map[string]json.RawMessage
		want              func(path string) string
	}{
		{"v2 derived field", "macmcp", "mail_search", McpSurfaces{"macmcp": macmcpSurface()},
			blobs("macmcp", `{"mail_accounts":["Alice"]}`),
			func(p string) string { return `{"mail_accounts":["Alice"],"file_dirs":["` + p + `"]}` }},
		{"v1 blob", "fsmcp", "fs_read", McpSurfaces{"fsmcp": carriedV1FsSurface()}, nil,
			func(p string) string { return `{"allowed_dirs":["` + p + `"]}` }},
	}
	for _, k := range kinds {
		patches := map[string]func() UpdateFields{
			"access":         func() UpdateFields { return UpdateFields{Access: &map[string]string{k.mcpID: config.AccessRead}} },
			"allowed_tools":  func() UpdateFields { return UpdateFields{AllowedTools: &map[string][]string{k.mcpID: {k.tool}}} },
			"allow_external": func() UpdateFields { return UpdateFields{AllowExternal: &map[string]bool{k.mcpID: true}} },
		}
		for field, patch := range patches {
			for _, path := range []string{carriedPath, carriedNextPath} {
				t.Run(k.name+"/"+field+"/path "+path, func(t *testing.T) {
					s, created := carriedSettings(t, CreateFields{
						Name: "Acme", Kind: config.ProjectKindLocal, Path: carriedPath,
						AllowedMcpIDs: []string{k.mcpID}, Context: k.context,
					}, k.surfaces)
					f := patch()
					if path != carriedPath {
						f.Path = &path
					}
					if _, _, err := ApplyUpdate(s, created.ID, f, func() McpSurfaces { return k.surfaces }); err != nil {
						t.Fatalf("a %s patch without context was refused: %v", field, err)
					}
					if got, want := carriedDecoded(t, s, created.ID, k.mcpID), carriedWant(t, k.want(path)); !reflect.DeepEqual(got, want) {
						t.Fatalf("stored context[%s] = %v, want %v", k.mcpID, got, want)
					}
				})
			}
		}
	}
}

func TestApplyUpdate_DerivedFieldStillRefusedWhenNamedOrConverted(t *testing.T) {
	remote := config.ProjectKindRemote
	empty := ""
	rows := []struct {
		name string
		f    UpdateFields
	}{
		{"request context carries the derived field", UpdateFields{
			Context: &map[string]json.RawMessage{"macmcp": json.RawMessage(`{"mail_accounts":["Alice"],"file_dirs":["` + carriedPath + `"]}`)},
		}},
		{"conversion to remote with access while file_dirs is stored", UpdateFields{
			Kind: &remote, Path: &empty, DisabledTools: &map[string][]string{},
			Access: &map[string]string{"macmcp": config.AccessRead},
		}},
	}
	surfaces := McpSurfaces{"macmcp": macmcpSurface()}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s, created := carriedSettings(t, CreateFields{
				Name: "Acme", Kind: config.ProjectKindLocal, Path: carriedPath, AllowedMcpIDs: []string{"macmcp"},
				Context: blobs("macmcp", `{"mail_accounts":["Alice"]}`),
			}, surfaces)
			before := cloneBlobs(s.Projects[0].Context)
			_, _, err := ApplyUpdate(s, created.ID, r.f, func() McpSurfaces { return surfaces })
			if err == nil || !strings.Contains(err.Error(), "file_dirs") {
				t.Fatalf("err = %v, want a refusal naming file_dirs", err)
			}
			if s.Projects[0].IsRemote() || !reflect.DeepEqual(s.Projects[0].Context, before) {
				t.Fatalf("refused update changed the record: kind %q context %s", s.Projects[0].Kind, s.Projects[0].Context)
			}
		})
	}
}

func TestApplyUpdate_V1EchoAcceptedExactlyWhenTheGateSeesNoChange(t *testing.T) {
	surfaces := McpSurfaces{"fsmcp": carriedV1FsSurface()}
	stored := `{"allowed_dirs":["` + carriedPath + `"]}`
	rows := []struct {
		name, stored, requested, path string
		accept                        bool
	}{
		{"exact echo", stored, stored, "", true},
		{"echo with whitespace", stored, ` { "allowed_dirs" : [ "` + carriedPath + `" ] } `, "", true},
		{"echo with reordered keys", `{"allowed_dirs":["` + carriedPath + `"],"mode":"ro"}`,
			`{"mode":"ro","allowed_dirs":["` + carriedPath + `"]}`, "", true},
		{"echo with a path change", stored, stored, carriedNextPath, true},
		{"changed value", stored, `{"allowed_dirs":["/work/other"]}`, "", false},
		{"value the new path would derive", stored, `{"allowed_dirs":["` + carriedNextPath + `"]}`, carriedNextPath, false},
		{"new blob", "", stored, "", false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s, created := carriedSettings(t, CreateFields{
				Name: "Acme", Kind: config.ProjectKindLocal, Path: carriedPath, AllowedMcpIDs: []string{"fsmcp"},
			}, surfaces)
			if r.stored == "" {
				delete(s.Projects[0].Context, "fsmcp")
			} else {
				s.Projects[0].Context["fsmcp"] = json.RawMessage(r.stored)
			}
			snapshot := s.Projects[0]
			snapshot.Context = cloneBlobs(s.Projects[0].Context)
			before := cloneBlobs(s.Projects[0].Context)

			f := UpdateFields{Context: &map[string]json.RawMessage{"fsmcp": json.RawMessage(r.requested)}}
			if r.path != "" {
				f.Path = &r.path
			}
			gateUnchanged := !slices.Contains(UpdateWidensGrant(snapshot, f, surfaces), "context")
			_, _, err := ApplyUpdate(s, created.ID, f, func() McpSurfaces { return surfaces })

			if (err == nil) != r.accept {
				t.Fatalf("ApplyUpdate err = %v, want accepted=%v", err, r.accept)
			}
			if (err == nil) != gateUnchanged {
				t.Fatalf("validation accepted=%v but the gate reports context unchanged=%v", err == nil, gateUnchanged)
			}
			if !r.accept {
				if !reflect.DeepEqual(s.Projects[0].Context, before) {
					t.Fatalf("refused update changed the stored context to %s", s.Projects[0].Context)
				}
				return
			}
			if r.path != "" {
				want := carriedWant(t, `{"allowed_dirs":["`+r.path+`"]}`)
				if got := carriedDecoded(t, s, created.ID, "fsmcp"); !reflect.DeepEqual(got, want) {
					t.Fatalf("stored fsmcp = %v, want it re-derived as %v", got, want)
				}
			}
		})
	}
}

func TestV1Context_RefusedOnCreateAndOnARemoteProfile(t *testing.T) {
	blob := `{"allowed_dirs":["` + carriedPath + `"]}`
	t.Run("create", func(t *testing.T) {
		s := &config.Settings{Version: 1, ExternalMcps: []config.ExternalMcp{{ID: "fsmcp", DisplayName: "fsmcp"}}}
		_, err := ApplyCreate(s, CreateFields{
			Name: "Acme", Kind: config.ProjectKindLocal, Path: carriedPath, AllowedMcpIDs: []string{"fsmcp"},
			Context: blobs("fsmcp", blob),
		}, McpSurfaces{"fsmcp": carriedV1FsSurface()})
		if err == nil || !strings.Contains(err.Error(), "fsmcp") {
			t.Fatalf("err = %v, want a refusal naming fsmcp", err)
		}
		if len(s.Projects) != 0 {
			t.Fatalf("refused create persisted %d projects", len(s.Projects))
		}
	})
	t.Run("remote echo", func(t *testing.T) {
		surfaces := McpSurfaces{"notesmcp": {Schema: json.RawMessage(`{"rules":{"type":"array"}}`)}}
		s, created := carriedSettings(t, CreateFields{
			Name: "Acme", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"notesmcp"},
		}, surfaces)
		s.Projects[0].Context = blobs("notesmcp", `{"rules":["a"]}`)
		before := cloneBlobs(s.Projects[0].Context)
		echo := blobs("notesmcp", `{"rules":["a"]}`)
		_, _, err := ApplyUpdate(s, created.ID, UpdateFields{Context: &echo}, func() McpSurfaces { return surfaces })
		if err == nil || !strings.Contains(err.Error(), "notesmcp") {
			t.Fatalf("err = %v, want a refusal naming notesmcp", err)
		}
		if !reflect.DeepEqual(s.Projects[0].Context, before) {
			t.Fatalf("refused update changed the stored context to %s", s.Projects[0].Context)
		}
	})
}

func TestApplyUpdate_V1DuplicateKeyRefusedBeforeTheEcho(t *testing.T) {
	surfaces := McpSurfaces{"fsmcp": carriedV1FsSurface()}
	s, created := carriedSettings(t, CreateFields{
		Name: "Acme", Kind: config.ProjectKindLocal, Path: carriedPath, AllowedMcpIDs: []string{"fsmcp"},
	}, surfaces)
	requested := blobs("fsmcp", `{"allowed_dirs":["/"],"allowed_dirs":["`+carriedPath+`"]}`)
	_, _, err := ApplyUpdate(s, created.ID, UpdateFields{Context: &requested}, func() McpSurfaces { return surfaces })
	if err == nil || !strings.Contains(err.Error(), `repeats the key "allowed_dirs"`) {
		t.Fatalf("err = %v, want the duplicate-key refusal", err)
	}
}
