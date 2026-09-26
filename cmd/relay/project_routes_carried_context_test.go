package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/project"
)

// carriedRoutesServer seeds a local project reaching macmcp (a v2 derived
// field) and fsmcp (a v1 blob) and serves the project routes over it, with a
// gate that allows and records every prompt.
func carriedRoutesServer(t *testing.T) (*httptest.Server, config.SettingsStore, config.Project, *presencetest.Recording) {
	t.Helper()
	surfaces := v2Surfaces()
	store, stored := ctxGateSeed(t, project.CreateFields{
		Name: "Acme", Kind: config.ProjectKindLocal, Path: t.TempDir(), AllowedMcpIDs: []string{"macmcp", "fsmcp"},
		Context: ctxMap("macmcp", `{"mail_accounts":["Alice"]}`),
	}, surfaces)
	rec := presencetest.NewRecording(nil)
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket},
		store, ctxGateOps(t, store, rec), schemaProviderFunc(func() project.McpSurfaces { return surfaces }), nil, nil, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, stored, rec
}

func carriedPut(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	assertNoErr(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "PUT %s", url)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	assertNoErr(t, err, "read body")
	return resp.StatusCode, string(out)
}

func carriedPromptFields(t *testing.T, rec *presencetest.Recording) [][]string {
	t.Helper()
	var out [][]string
	for _, r := range rec.Reasons() {
		lp, rp := strings.LastIndex(r, "("), strings.LastIndex(r, ")")
		if lp < 0 || rp < lp {
			t.Fatalf("reason %q carries no field list", r)
		}
		out = append(out, strings.Split(r[lp+1:rp], ", "))
	}
	return out
}

func TestProjectRoutes_PermissionOnlyPutOnDerivedContext(t *testing.T) {
	rows := []struct {
		name, body string
		prompts    [][]string
	}{
		{"access", `{"access":{"macmcp":"read","fsmcp":"read"}}`, nil},
		{"allowed_tools", `{"allowed_tools":{"macmcp":["mail_search"]}}`, [][]string{{"allowed_tools"}}},
		{"allow_external", `{"allow_external":{"macmcp":true}}`, [][]string{{"allow_external"}}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			srv, store, stored, rec := carriedRoutesServer(t)
			before := decodedContext(t, store, stored.ID)
			if code, body := carriedPut(t, srv.URL+"/api/projects/"+stored.ID, r.body); code != http.StatusOK {
				t.Fatalf("PUT %s = %d %s, want 200", r.body, code, body)
			}
			if got := carriedPromptFields(t, rec); !reflect.DeepEqual(got, r.prompts) {
				t.Fatalf("prompts named %v, want %v", got, r.prompts)
			}
			if after := decodedContext(t, store, stored.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("stored context changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestProjectRoutes_V1EchoWithAPathChange(t *testing.T) {
	srv, store, stored, rec := carriedRoutesServer(t)
	next := t.TempDir()
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	body := `{"path":` + quote(next) + `,"context":{"macmcp":{"mail_accounts":["Alice"]},` +
		`"fsmcp": { "allowed_dirs" : [ ` + quote(stored.Path) + ` ] } }}`
	if code, resp := carriedPut(t, srv.URL+"/api/projects/"+stored.ID, body); code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", code, resp)
	}
	if got := carriedPromptFields(t, rec); !reflect.DeepEqual(got, [][]string{{"path"}}) {
		t.Fatalf("prompts named %v, want one naming only path", got)
	}
	want := map[string]any{
		"macmcp": map[string]any{"mail_accounts": []any{"Alice"}, "file_dirs": []any{next}},
		"fsmcp":  map[string]any{"allowed_dirs": []any{next}},
	}
	if got := decodedContext(t, store, stored.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored context = %v, want %v", got, want)
	}
}
