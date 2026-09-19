package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestIpcListTemplates_EmitsResolvedList(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *config.Settings) {
		s.TerminalTemplates = []config.TerminalTemplate{
			{ID: "shell", Name: "Custom Shell", Command: "/bin/bash"},
		}
	}); err != nil {
		t.Fatalf("With: %v", err)
	}

	ui := &recordingUI{}
	ctx := &IPCContext{Ctx: context.Background(), Store: store, UI: ui}

	ipcListTemplates(ctx, json.RawMessage(`{"type":"list_templates"}`))

	args, ok := findEvent(ui, "onTemplatesListed")
	if !ok {
		t.Fatal("expected onTemplatesListed to be emitted")
	}
	raw, ok := args[0].(json.RawMessage)
	if !ok {
		t.Fatalf("onTemplatesListed arg was %T, want json.RawMessage", args[0])
	}
	var got []config.TerminalTemplate
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal emitted templates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d templates, want only the one in settings.json", len(got))
	}
	found := false
	for _, tmpl := range got {
		if tmpl.ID == "shell" {
			found = true
			if tmpl.Name != "Custom Shell" {
				t.Fatalf("expected the overridden shell template, got %+v", tmpl)
			}
		}
	}
	if !found {
		t.Fatal("shell template missing from emitted list")
	}
}
