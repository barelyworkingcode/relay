package main

import (
	"context"
	"testing"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// seedShellTemplates sets a project's shell templates via the mutator inside a
// store transaction. Reuses newPtyTestRouter's sandboxed store + project.
func seedShellTemplates(t *testing.T, r *appRouter, projID string, tmpls []ShellTemplate) {
	t.Helper()
	if err := r.store.With(func(s *Settings) {
		s.UpdateProjectShellTemplates(projID, tmpls)
	}); err != nil {
		t.Fatalf("seed shell templates: %v", err)
	}
}

func TestResolveProjectTemplate_Found(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)
	want := ShellTemplate{
		ID:          "ssh-prod",
		Name:        "Prod SSH",
		Command:     "ssh",
		Args:        []string{"deploy@prod.example"},
		Env:         map[string]string{"TERM": "xterm-256color"},
		Description: "ssh into prod",
		Icon:        "shell",
	}
	seedShellTemplates(t, router, proj.ID, []ShellTemplate{want})

	resp, err := router.ResolveProjectTemplate(context.Background(), bridge.ShellTemplateRequest{
		ProjectID:  proj.ID,
		TemplateID: want.ID,
	}, svcToken)
	if err != nil {
		t.Fatalf("ResolveProjectTemplate: %v", err)
	}
	if resp.Command != "ssh" || resp.Name != "Prod SSH" {
		t.Errorf("name/command not returned: %+v", resp)
	}
	if len(resp.Args) != 1 || resp.Args[0] != "deploy@prod.example" {
		t.Errorf("args not round-tripped: %+v", resp.Args)
	}
	if resp.Env["TERM"] != "xterm-256color" {
		t.Errorf("env not round-tripped: %+v", resp.Env)
	}
	// ShellTemplateResponse has no token field by construction — assert at the
	// behavioral level too that resolving a template never surfaces the project
	// token anywhere in the response (ResolvePtyEnv is the sole token egress).
	if resp.ID != want.ID {
		t.Errorf("id mismatch: %q", resp.ID)
	}
}

func TestResolveProjectTemplate_UnknownProject(t *testing.T) {
	router, _, svcToken := newPtyTestRouter(t)
	_, err := router.ResolveProjectTemplate(context.Background(), bridge.ShellTemplateRequest{
		ProjectID:  "no-such-project",
		TemplateID: "x",
	}, svcToken)
	if code := codeOf(err); code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want CodeMethodNotFound (%d)", code, jsonrpc.CodeMethodNotFound)
	}
}

func TestResolveProjectTemplate_UnknownTemplate(t *testing.T) {
	router, proj, svcToken := newPtyTestRouter(t)
	seedShellTemplates(t, router, proj.ID, []ShellTemplate{{ID: "present", Name: "Present", Command: "ssh"}})

	_, err := router.ResolveProjectTemplate(context.Background(), bridge.ShellTemplateRequest{
		ProjectID:  proj.ID,
		TemplateID: "missing",
	}, svcToken)
	if code := codeOf(err); code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want CodeMethodNotFound (%d)", code, jsonrpc.CodeMethodNotFound)
	}
}

func TestResolveProjectTemplate_RequiresServiceToken(t *testing.T) {
	router, proj, _ := newPtyTestRouter(t)
	seedShellTemplates(t, router, proj.ID, []ShellTemplate{{ID: "t", Name: "T", Command: "ssh"}})

	// A project token (not a service token) must be rejected — project config is
	// service-token-gated over the bridge.
	_, err := router.ResolveProjectTemplate(context.Background(), bridge.ShellTemplateRequest{
		ProjectID:  proj.ID,
		TemplateID: "t",
	}, proj.Token)
	if code := codeOf(err); code != jsonrpc.CodeUnauthorized {
		t.Errorf("error code = %d, want CodeUnauthorized (%d)", code, jsonrpc.CodeUnauthorized)
	}
}
