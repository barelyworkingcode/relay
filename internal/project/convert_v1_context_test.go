package project

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func v1AndPlainSurfaces() McpSurfaces {
	return McpSurfaces{
		"fsmcp":    {Schema: json.RawMessage(`{"allowed_dirs":{"type":"array"}}`)},
		"notesmcp": {},
	}
}

func localGrantingV1AndPlain(t *testing.T) (*config.Settings, string) {
	t.Helper()
	s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "fsmcp"}, {ID: "notesmcp"}}}
	created, err := ApplyCreate(s, CreateFields{
		Name: "Acme", Path: t.TempDir(), AllowedMcpIDs: []string{"fsmcp", "notesmcp"},
	}, v1AndPlainSurfaces())
	assertNoErr(t, err, "create local project")
	stored, _ := config.FindProjectByID(s, created.ID)
	if _, ok := ContextValues(stored.Context["fsmcp"])[V1AllowedDirsField]; !ok {
		t.Fatalf("precondition: %s was not derived onto the local project: %s", V1AllowedDirsField, stored.Context["fsmcp"])
	}
	return s, created.ID
}

// formConversion is the Settings form's complete save flipping kind to
// remote: every permission map named, context as given.
func formConversion(ctx map[string]json.RawMessage) UpdateFields {
	f := toRemote()
	f.Access = &map[string]string{}
	f.AllowExternal = &map[string]bool{}
	f.AllowedTools = &map[string][]string{}
	f.Context = &ctx
	return f
}

func TestApplyUpdate_ConvertingV1GrantToRemote(t *testing.T) {
	refused := func(t *testing.T, s *config.Settings, id string, f UpdateFields) error {
		t.Helper()
		before := storedJSON(t, s, id)
		_, _, err := ApplyUpdate(s, id, f, surfacesOf(v1AndPlainSurfaces()))
		if err == nil {
			t.Fatal("converting a project that still grants a filesystem-scoped MCP was not refused")
		}
		if after := storedJSON(t, s, id); !bytes.Equal(before, after) {
			t.Errorf("a refused conversion changed the record:\nbefore %s\nafter  %s", before, after)
		}
		return err
	}

	t.Run("keeping the grant with the stored blob echoed is refused by the grant check", func(t *testing.T) {
		s, id := localGrantingV1AndPlain(t)
		stored, _ := config.FindProjectByID(s, id)

		err := refused(t, s, id, formConversion(maps.Clone(stored.Context)))
		if !strings.Contains(err.Error(), "filesystem-scoped") || strings.Contains(err.Error(), "v1 context schema") {
			t.Errorf("refusal must come from the grant check, not the v1 context check: %v", err)
		}
	})

	t.Run("keeping the grant with a different allowed_dirs is refused", func(t *testing.T) {
		s, id := localGrantingV1AndPlain(t)
		stored, _ := config.FindProjectByID(s, id)
		ctx := maps.Clone(stored.Context)
		ctx["fsmcp"] = json.RawMessage(`{"allowed_dirs":["/acme/elsewhere"]}`)

		err := refused(t, s, id, formConversion(ctx))
		if !strings.Contains(err.Error(), "v1 context schema") {
			t.Errorf("a changed allowed_dirs must be refused by the v1 context check: %v", err)
		}
	})

	t.Run("an operator value under a v1 schema relay derives nothing for is refused", func(t *testing.T) {
		folder := McpSurfaces{"foldermcp": {Schema: json.RawMessage(`{"folder":{"type":"string"}}`)}}
		s := &config.Settings{ExternalMcps: []config.ExternalMcp{{ID: "foldermcp"}}}
		created, err := ApplyCreate(s, CreateFields{
			Name: "Acme", Path: t.TempDir(), AllowedMcpIDs: []string{"foldermcp"},
		}, folder)
		assertNoErr(t, err, "create local project")
		stored, _ := config.FindProjectByID(s, created.ID)
		// Written past the form, which cannot set a field under a v1 schema.
		stored.Context = map[string]json.RawMessage{"foldermcp": json.RawMessage(`{"folder":"/acme"}`)}
		before := storedJSON(t, s, created.ID)

		_, _, err = ApplyUpdate(s, created.ID, formConversion(maps.Clone(stored.Context)), surfacesOf(folder))
		if err == nil || !strings.Contains(err.Error(), "v1 context schema") {
			t.Errorf("conversion echoing a v1 value relay did not derive must be refused by the v1 context check: %v", err)
		}
		if after := storedJSON(t, s, created.ID); !bytes.Equal(before, after) {
			t.Errorf("a refused conversion changed the record:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("dropping the grant converts and leaves no fsmcp context", func(t *testing.T) {
		s, id := localGrantingV1AndPlain(t)
		f := toRemote()
		f.AllowedMcpIDs = &[]string{"notesmcp"}
		f.Access = &map[string]string{}

		_, _, err := ApplyUpdate(s, id, f, surfacesOf(v1AndPlainSurfaces()))
		assertNoErr(t, err, "convert to remote without fsmcp")

		after, _ := config.FindProjectByID(s, id)
		if !after.IsRemote() {
			t.Fatalf("kind = %q, want remote", after.Kind)
		}
		if raw, ok := after.Context["fsmcp"]; ok {
			t.Errorf("the remote record still holds a fsmcp context entry: %s", raw)
		}
	})
}
