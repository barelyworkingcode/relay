package project

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func contextValidationFixture() (*config.Settings, McpSurfaces, CreateFields) {
	s := &config.Settings{Version: 1}
	surfaces := McpSurfaces{"macmcp": macmcpSurface()}
	f := CreateFields{Name: "Acme", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp", "notesmcp"}}
	return s, surfaces, f
}

func wantDuplicateKeyRefusal(t *testing.T, err error, mcpID, key string) {
	t.Helper()
	want := fmt.Sprintf("context for %q repeats the key %q", mcpID, key)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want one containing %s", err, want)
	}
}

func TestProjectContext_DuplicateKeyIsRefusedOnSave(t *testing.T) {
	rows := []struct {
		name, mcpID, blob, key string
	}{
		{"top-level repeat under a v2 schema", "macmcp", `{"mail_accounts":["Alice"],"mail_accounts":["*"]}`, "mail_accounts"},
		{"top-level repeat with no schema", "notesmcp", `{"rules":["a"],"rules":["b"]}`, "rules"},
		{"repeat inside an object in an array", "notesmcp", `{"rules":[{"account":"Alice"},{"account":"Bob","account":"*"}]}`, "account"},
	}
	for _, r := range rows {
		requested := blobs(r.mcpID, r.blob)
		t.Run("create: "+r.name, func(t *testing.T) {
			s, surfaces, f := contextValidationFixture()
			f.Context = requested
			_, err := ApplyCreate(s, f, surfaces)
			wantDuplicateKeyRefusal(t, err, r.mcpID, r.key)
			if len(s.Projects) != 0 {
				t.Fatalf("refused create persisted %d projects", len(s.Projects))
			}
		})
		t.Run("update: "+r.name, func(t *testing.T) {
			s, surfaces, f := contextValidationFixture()
			created, err := ApplyCreate(s, f, surfaces)
			if err != nil {
				t.Fatalf("ApplyCreate: %v", err)
			}
			before := cloneBlobs(s.Projects[0].Context)
			_, found, err := ApplyUpdate(s, created.ID, UpdateFields{Context: &requested}, func() McpSurfaces { return surfaces })
			if !found {
				t.Fatal("ApplyUpdate did not find the project")
			}
			wantDuplicateKeyRefusal(t, err, r.mcpID, r.key)
			if !reflect.DeepEqual(s.Projects[0].Context, before) {
				t.Fatalf("refused update changed the stored context to %s", s.Projects[0].Context)
			}
		})
	}
}

func TestProjectContext_DistinctKeysSave(t *testing.T) {
	rows := []struct {
		name, mcpID, blob string
		fields            []string
	}{
		{"v2 schema", "macmcp", `{"mail_accounts":["Alice"],"mail_mailboxes":["INBOX"]}`, []string{"mail_accounts", "mail_mailboxes"}},
		{"one key in sibling objects and at another depth", "notesmcp",
			`{"account":["Alice"],"rules":[{"account":"Alice"},{"account":"Bob"}]}`, []string{"account", "rules"}},
	}
	for _, r := range rows {
		requested := blobs(r.mcpID, r.blob)
		assertSaved := func(t *testing.T, s *config.Settings) {
			t.Helper()
			values := ContextValues(s.Projects[0].Context[r.mcpID])
			for _, field := range r.fields {
				if !HasScopeValue(values, field) {
					t.Fatalf("field %q not saved; stored %s", field, s.Projects[0].Context[r.mcpID])
				}
			}
		}
		t.Run("create: "+r.name, func(t *testing.T) {
			s, surfaces, f := contextValidationFixture()
			f.Context = requested
			if _, err := ApplyCreate(s, f, surfaces); err != nil {
				t.Fatalf("ApplyCreate: %v", err)
			}
			assertSaved(t, s)
		})
		t.Run("update: "+r.name, func(t *testing.T) {
			s, surfaces, f := contextValidationFixture()
			created, err := ApplyCreate(s, f, surfaces)
			if err != nil {
				t.Fatalf("ApplyCreate: %v", err)
			}
			if _, _, err := ApplyUpdate(s, created.ID, UpdateFields{Context: &requested}, func() McpSurfaces { return surfaces }); err != nil {
				t.Fatalf("ApplyUpdate: %v", err)
			}
			assertSaved(t, s)
		})
	}
}
