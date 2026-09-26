package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

// An MCP declaring one operator restrict field of each kind the form's text
// round trip treats differently, plus one field relay derives.
const untouchedAcmeSchema = `{
  "acme_accounts": {"type": "array", "items": {"type": "string"}, "description": "Accounts", "scope": "restrict", "source": "operator"},
  "acme_owner": {"type": "string", "description": "Owner", "scope": "restrict", "source": "operator"},
  "acme_limit": {"description": "Limit", "scope": "restrict", "source": "operator"},
  "acme_dirs": {"type": "array", "items": {"type": "string"}, "description": "Dirs", "scope": "restrict", "source": "project_path", "applies_to": ["acme_write"]}
}`

const untouchedAcmeValid = `{"acme_accounts":["Alice"],"acme_owner":"Carol","acme_limit":"7"}`

func untouchedAcmeSurfaces() project.McpSurfaces {
	return project.McpSurfaces{"acmemcp": {
		Schema: json.RawMessage(untouchedAcmeSchema), SchemaVersion: 2, Tools: []string{"acme_read", "acme_write"},
	}}
}

// untouchedSeed creates a valid project, then overwrites what is stored the
// way a hand edit or an older relay could have left it.
func untouchedSeed(t *testing.T, f project.CreateFields, surfaces project.McpSurfaces, mutate func(p *config.Project)) (config.SettingsStore, config.Project) {
	t.Helper()
	store, created := ctxGateSeed(t, f, surfaces)
	if err := store.With(func(s *config.Settings) {
		p, _ := config.FindProjectByID(s, created.ID)
		mutate(p)
	}); err != nil {
		t.Fatalf("mutate stored project: %v", err)
	}
	p, _ := config.FindProjectByID(store.Get(), created.ID)
	return store, *p
}

func untouchedLocalAcme(t *testing.T, stored string) (config.SettingsStore, config.Project) {
	t.Helper()
	return untouchedSeed(t, project.CreateFields{
		Name: "Acme", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"acmemcp"},
		Context: ctxMap("acmemcp", untouchedAcmeValid),
	}, untouchedAcmeSurfaces(), func(p *config.Project) {
		values := project.ContextValues(p.Context["acmemcp"])
		for k, v := range project.ContextValues(json.RawMessage(stored)) {
			values[k] = v
		}
		b, _ := json.Marshal(values)
		p.Context["acmemcp"] = b
	})
}

func untouchedDecode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func untouchedHarvestedAcme(t *testing.T, msg ipcUpdateProjectMsg) map[string]any {
	t.Helper()
	if msg.Context == nil {
		t.Fatal("harvest omitted context")
	}
	return untouchedDecode(t, (*msg.Context)["acmemcp"])
}

func untouchedOperatorValues(values map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range values {
		if k != "acme_dirs" {
			out[k] = v
		}
	}
	return out
}

func TestSettingsForm_UntouchedFieldsAreSentAsStored(t *testing.T) {
	rows := []struct{ name, stored string }{
		{"padded and JSON-looking values", `{"acme_accounts":[" Alice "],"acme_owner":"  Carol ","acme_limit":"123"}`},
		{"null in a restrict field", `{"acme_accounts":null}`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			store, stored := untouchedLocalAcme(t, r.stored)
			msg := ctxGateHarvest(t, store, stored, untouchedAcmeSurfaces(), "")
			want := untouchedOperatorValues(untouchedDecode(t, stored.Context["acmemcp"]))
			if got := untouchedHarvestedAcme(t, msg); !reflect.DeepEqual(got, want) {
				t.Fatalf("harvested acmemcp = %v, want as stored %v", got, want)
			}
		})
	}
}

func TestSettingsForm_TouchingAFieldNormalisesOnlyThatField(t *testing.T) {
	stored := `{"acme_accounts":[" Alice "],"acme_owner":"  Carol ","acme_limit":"123"}`
	rows := []struct {
		field, text string
		want        any
	}{
		{"acme_accounts", ` Dave \n\n Erin `, []any{"Dave", "Erin"}},
		{"acme_owner", `  Dave  `, "Dave"},
		{"acme_limit", ` 42 `, float64(42)},
	}
	for _, r := range rows {
		t.Run(r.field, func(t *testing.T) {
			store, p := untouchedLocalAcme(t, stored)
			msg := ctxGateHarvest(t, store, p, untouchedAcmeSurfaces(),
				`window.setProjScopeText('acmemcp', '`+r.field+`', '`+r.text+`');`)
			want := untouchedOperatorValues(untouchedDecode(t, p.Context["acmemcp"]))
			want[r.field] = r.want
			if got := untouchedHarvestedAcme(t, msg); !reflect.DeepEqual(got, want) {
				t.Fatalf("harvested acmemcp = %v, want %v", got, want)
			}
		})
	}
}

func TestSettingsForm_UntouchedRemoteAllowedToolsAreSentAsStored(t *testing.T) {
	surfaces := project.McpSurfaces{"macmcp": macmcpSurface()}
	store, stored := untouchedSeed(t, project.CreateFields{
		Name: "Acme inbox", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
		AllowedTools: map[string][]string{"macmcp": {"mail_*"}},
		Access:       map[string]string{"macmcp": "read"},
		Context:      ctxMap("macmcp", `{"mail_accounts":["Bob"]}`),
	}, surfaces, func(p *config.Project) {
		p.AllowedTools = map[string][]string{"macmcp": {" mail_search ", "mail_send"}}
	})
	msg := ctxGateHarvest(t, store, stored, surfaces, "")
	if msg.AllowedTools == nil {
		t.Fatal("harvest omitted allowed_tools")
	}
	if got := *msg.AllowedTools; !reflect.DeepEqual(got, stored.AllowedTools) {
		t.Fatalf("harvested allowed_tools = %q, want as stored %q", got, stored.AllowedTools)
	}
}

func TestSettingsForm_ContextAlwaysSentWithoutDerivedFields(t *testing.T) {
	rows := []struct{ name, edit string }{
		{"untouched", ""},
		{"touched", `window.setProjScopeText('acmemcp', 'acme_owner', 'Dave');`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			store, stored := untouchedLocalAcme(t, untouchedAcmeValid)
			if _, ok := untouchedDecode(t, stored.Context["acmemcp"])["acme_dirs"]; !ok {
				t.Fatalf("seed did not derive acme_dirs: %s", stored.Context["acmemcp"])
			}
			msg := ctxGateHarvest(t, store, stored, untouchedAcmeSurfaces(), r.edit)
			if _, ok := untouchedHarvestedAcme(t, msg)["acme_dirs"]; ok {
				t.Fatalf("harvest sent the derived field acme_dirs: %s", (*msg.Context)["acmemcp"])
			}
		})
	}
}
