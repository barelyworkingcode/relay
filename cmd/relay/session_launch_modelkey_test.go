package main

// Minting a model key for a template and delivering it are separate acts:
// relay mints and hands the key to relay-sessions, which expands the
// template's ${MODEL_KEY} mapping at spawn (plans/client-model-routing.md).

import (
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// A template's ${MODEL_KEY} mapping is carried to relay-sessions verbatim and
// expanded there (internal/sessions/terminal): relay mints the key and puts it
// in the spec's model_key, and neither writes it into the env nor invents a
// mapping of its own.
func TestAuthorizeLaunch_ModelKeyIsMintedForTheTemplateAndNeverInjectedByRelay(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		s.TerminalTemplates = append(s.TerminalTemplates,
			config.TerminalTemplate{
				ID: "mapped", Name: "Mapped", Command: "/bin/sh", ModelKey: true,
				Env: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "X-Relay-Key: ${MODEL_KEY}"},
			},
			config.TerminalTemplate{ID: "optin-nomap", Name: "Opted in, unmapped", Command: "/bin/sh", ModelKey: true},
			config.TerminalTemplate{ID: "plain", Name: "Plain", Command: "/bin/sh"},
		)
	}), "seed templates")

	caller := LaunchCaller{Credential: &config.APICredential{ID: "tpl-exec", Classes: []control.CapabilityClass{control.ClassExecute}}}
	launch := func(id string) *LaunchResult {
		t.Helper()
		res, refusal := AuthorizeLaunch(f.store, f.deps.modelKeys, f.deps.sessions,
			LaunchRequest{Caller: caller, ProjectID: f.proj.ID, Kind: KindPTY, TemplateID: id})
		if refusal != nil {
			t.Fatalf("%s: AuthorizeLaunch refused: %+v", id, refusal)
		}
		return res
	}

	mapped := launch("mapped")
	if mapped.Spec.ModelKey == "" {
		t.Fatal("a model_key template minted no key")
	}
	if got := mapped.Spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Relay-Key: ${MODEL_KEY}" {
		t.Fatalf("relay expanded or altered the mapping: %q (relay-sessions expands it at spawn)", got)
	}

	unmapped := launch("optin-nomap")
	if unmapped.Spec.ModelKey == "" {
		t.Fatal("an opted-in template minted no key")
	}
	for k, v := range unmapped.Spec.Env {
		if strings.Contains(v, unmapped.Spec.ModelKey) || strings.Contains(k, "MODEL_KEY") {
			t.Fatalf("relay injected the key into a template with no mapping: %s=%s", k, v)
		}
	}

	if plain := launch("plain"); plain.Spec.ModelKey != "" {
		t.Fatal("a template without model_key: true was minted a key")
	}
}
