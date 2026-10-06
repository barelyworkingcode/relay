package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func codexConsoleTemplate() config.TerminalTemplate {
	return config.TerminalTemplate{
		ID: "codex", Name: "Codex", Command: "codex", Sandbox: ptr(true),
		ReadWrite: []string{"~/.codex", "~/.cache"}, Read: []string{"/opt/homebrew"},
	}
}

// POST /api/sessions with a codex/ model reaches the host as a codex launch
// only when the project allows the codex template.
func TestSessionRoutes_CreateCodexSession_GateAndLaunchSpec(t *testing.T) {
	t.Run("template not allowed", func(t *testing.T) {
		f := newSessionRoutesFixture(t)
		fs := f.registerChatHost(t)
		assertNoErr(t, f.store.With(func(s *config.Settings) {
			s.TerminalTemplates = append(s.TerminalTemplates, codexConsoleTemplate())
			s.Projects[0].AllowedTemplates = []string{"shell"}
		}), "seed")

		rec := f.postSession(t, f.mux, "/api/sessions", "codex/gpt-6-luna")

		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "codex") {
			t.Fatalf("status = %d body = %s, want 403 naming the codex template", rec.Code, rec.Body.String())
		}
		if n := len(fs.Requests()); n != 0 {
			t.Fatalf("a refused launch reached the host: %d requests", n)
		}
	})

	t.Run("allowed", func(t *testing.T) {
		f := newSessionRoutesFixture(t)
		fs := f.registerChatHost(t)
		assertNoErr(t, f.store.With(func(s *config.Settings) {
			s.TerminalTemplates = append(s.TerminalTemplates, codexConsoleTemplate())
		}), "seed")

		rec := f.postSession(t, f.mux, "/api/sessions", "codex/gpt-6-luna")

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body.String())
		}
		var spec hostapi.LaunchRequest
		assertNoErr(t, json.Unmarshal(fs.LastRequest().Body, &spec), "decode launch request")
		if spec.Kind != "codex" {
			t.Errorf("kind = %q, want codex", spec.Kind)
		}
		if spec.Sandbox == nil || spec.Sandbox.ProfilePath == "" {
			t.Errorf("sandbox = %+v, want a profile path", spec.Sandbox)
		}
		if spec.ModelKey != "" {
			t.Errorf("model_key = %q, want none: codex brings its own login", spec.ModelKey)
		}
	})
}

// A hosted project may run codex only when its host has a valid codex
// template, and that template's command becomes the host's codex path.
func TestAuthorizeLaunch_HostedCodexGate(t *testing.T) {
	cases := map[string]struct {
		templates []config.TerminalTemplate
		wantPath  string
		wantCode  string // "" means allowed
	}{
		"host has codex template": {[]config.TerminalTemplate{{ID: "codex", Name: "Codex", Command: "/opt/acme/bin/codex"}}, "/opt/acme/bin/codex", ""},
		"host has only shell":     {[]config.TerminalTemplate{{ID: "shell", Name: "Shell"}}, "", "template_not_allowed"},
		"codex template invalid":  {[]config.TerminalTemplate{{ID: "codex", Name: "Codex", Command: "/opt/acme/bin/codex", Sandbox: ptr(true)}}, "", "template_not_allowed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := newLaunchTestStore(t)
			sessions := newLaunchTestLedger(t)
			proj := addLaunchTestHostedProject(t, store, c.templates)
			req := LaunchRequest{Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindCodex, Model: "codex/gpt-6-luna"}

			result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), sessions, req)

			if c.wantCode != "" {
				if refusal == nil || refusal.Code != c.wantCode {
					t.Fatalf("refusal = %+v, want %s", refusal, c.wantCode)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refused: %+v", refusal)
			}
			var host sessiontypes.HostSpec
			assertNoErr(t, json.Unmarshal(result.Spec.Host, &host), "decode host spec")
			if host.CodexPath != c.wantPath {
				t.Errorf("codex_path = %q, want %q", host.CodexPath, c.wantPath)
			}
			if result.Spec.Sandbox != nil || result.Spec.ModelKey != "" || result.Spec.Identity != nil {
				t.Errorf("hosted codex got sandbox=%v model_key=%q identity=%v, want none of them", result.Spec.Sandbox, result.Spec.ModelKey, result.Spec.Identity)
			}
		})
	}
}
