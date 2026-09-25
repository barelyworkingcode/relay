package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/modelbroker"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func catalogHostRows() []sessiontypes.ModelInfo {
	return []sessiontypes.ModelInfo{
		{Value: "haiku", Label: "Haiku", Group: "Claude", Provider: "claude"},
		{Value: "Chat", Label: "Chat", Group: "Model Broker", Provider: "chat"},
		{Value: "p1/local-model", Label: "local-model", Group: "pi · p1", Provider: "pi"},
		{Value: "omlx/Chat", Label: "omlx/Chat", Group: "Model Broker", Provider: "chat"},
		{Value: "virt", Label: "virt", Group: "Model Broker", Provider: "chat"},
		{Value: "Speak", Label: "Speak", Group: "Model Broker", Provider: "chat"},
		{Value: "unmatched", Label: "unmatched", Group: "Model Broker", Provider: "chat"},
	}
}

func catalogBrokerRows() []modelbroker.Row {
	return []modelbroker.Row{
		{ID: "haiku", OwnedBy: "anthropic-map", Target: "omlx/Chat"},
		{ID: "Chat", OwnedBy: "anthropic-map", Target: "omlx/Chat"},
		{ID: "omlx/Chat", OwnedBy: "omlx"},
		{ID: "virt", OwnedBy: "virtual"},
		{ID: "Speak", OwnedBy: "anthropic-map", Target: "omlx/kokoro-v1"},
	}
}

func catalogByID(view ModelCatalogView) map[string]ModelEntryView {
	out := make(map[string]ModelEntryView, len(view.Models))
	for _, m := range view.Models {
		out[m.ID] = m
	}
	return out
}

func TestBuildModelCatalogView_EveryHostRowOnceInOrder(t *testing.T) {
	host := catalogHostRows()
	view := buildModelCatalogView(host, catalogBrokerRows())
	var got, want []string
	for _, m := range view.Models {
		got = append(got, m.ID)
	}
	for _, h := range host {
		want = append(want, h.Value)
	}
	if view.Status != "ok" || !reflect.DeepEqual(got, want) {
		t.Fatalf("status=%q ids=%v, want ok %v", view.Status, got, want)
	}
}

func TestBuildModelCatalogView_GroupsLabelsAndTargets(t *testing.T) {
	byID := catalogByID(buildModelCatalogView(catalogHostRows(), catalogBrokerRows()))
	cases := []struct{ id, group, label, target, kind string }{
		{"haiku", "Claude", "Haiku", "", "chat"}, // non-chat provider is never matched
		{"Chat", "Model broker · aliases", "Chat → omlx/Chat", "omlx/Chat", "chat"},
		{"p1/local-model", "pi · p1", "local-model", "", "chat"},
		{"omlx/Chat", "Model broker · omlx", "omlx/Chat", "", "chat"},
		{"virt", "Model broker · virtual", "virt", "", "chat"},
		{"Speak", "Model broker · aliases", "Speak → omlx/kokoro-v1", "omlx/kokoro-v1", "other"},
		{"unmatched", "Model Broker", "unmatched", "", "chat"},
	}
	for _, c := range cases {
		m := byID[c.id]
		if m.Group != c.group || m.Label != c.label || m.Target != c.target || m.Kind != c.kind {
			t.Errorf("%s = %+v, want group=%q label=%q target=%q kind=%q", c.id, m, c.group, c.label, c.target, c.kind)
		}
	}
}

func TestModelKind(t *testing.T) {
	other := strings.Fields("tts asr stt speech whisper parakeet kokoro orpheus dia codec snac vocoder embed embedding embeddings rerank reranker")
	for _, tok := range other {
		for _, id := range []string{tok, "acme/Model-" + strings.ToUpper(tok) + "-v1"} {
			if got := modelKind(id); got != "other" {
				t.Errorf("modelKind(%q) = %q, want other", id, got)
			}
		}
	}
	for _, id := range []string{"haiku", "Chat", "omlx/Chat", "acme/audio-chat-8b", "tts/plain-chat", "diamond", "whisperer", "codecs", "speech2text"} {
		if got := modelKind(id); got != "chat" {
			t.Errorf("modelKind(%q) = %q, want chat", id, got)
		}
	}
}

func TestModelCatalogOps_List(t *testing.T) {
	hostErr := errors.New("host down")
	brokerErr := errors.New("broker down")
	cases := []struct {
		name         string
		hostErr      error
		brokerErr    error
		wantStatus   string
		wantError    string
		wantWarnings []string
		wantCalls    []string
		wantChat     string // group of the Chat row
	}{
		{"both ok", nil, nil, "ok", "", nil, []string{"host", "broker"}, "Model broker · aliases"},
		{"broker fails", nil, brokerErr, "ok", "", []string{"model broker unavailable"}, []string{"host", "broker"}, "Model Broker"},
		{"host fails", hostErr, nil, "unavailable", "session host unavailable", nil, []string{"host"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			ops := &ModelCatalogOps{
				HostModels: func(context.Context) ([]sessiontypes.ModelInfo, error) {
					calls = append(calls, "host")
					if c.hostErr != nil {
						return nil, c.hostErr
					}
					return catalogHostRows(), nil
				},
				BrokerRows: func(context.Context) ([]modelbroker.Row, error) {
					calls = append(calls, "broker")
					if c.brokerErr != nil {
						return nil, c.brokerErr
					}
					return catalogBrokerRows(), nil
				},
			}
			view := ops.List(context.Background())
			if view.Status != c.wantStatus || view.Error != c.wantError || !reflect.DeepEqual(view.Warnings, c.wantWarnings) {
				t.Fatalf("view = %+v, want status=%q error=%q warnings=%v", view, c.wantStatus, c.wantError, c.wantWarnings)
			}
			if !reflect.DeepEqual(calls, c.wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, c.wantCalls)
			}
			if c.wantChat == "" {
				assertUnavailableCatalogJSON(t, view)
				return
			}
			if got := catalogByID(view)["Chat"].Group; got != c.wantChat {
				t.Fatalf("Chat group = %q, want %q", got, c.wantChat)
			}
		})
	}
}

func TestModelCatalogOps_NilSafe(t *testing.T) {
	var nilOps *ModelCatalogOps
	for name, ops := range map[string]*ModelCatalogOps{"nil receiver": nilOps, "nil funcs": {}} {
		view := ops.List(context.Background())
		if view.Status != "unavailable" || view.Error != "session host unavailable" {
			t.Errorf("%s: view = %+v, want unavailable", name, view)
		}
		assertUnavailableCatalogJSON(t, view)
	}

	hostOnly := &ModelCatalogOps{HostModels: func(context.Context) ([]sessiontypes.ModelInfo, error) {
		return catalogHostRows(), nil
	}}
	view := hostOnly.List(context.Background())
	if view.Status != "ok" || !reflect.DeepEqual(view.Warnings, []string{"model broker unavailable"}) || len(view.Models) != len(catalogHostRows()) {
		t.Fatalf("nil BrokerRows: view = %+v, want ok with broker warning and every host row", view)
	}
}

func assertUnavailableCatalogJSON(t *testing.T, view ModelCatalogView) {
	t.Helper()
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"models":[]`) || strings.Contains(string(data), `"warnings"`) {
		t.Fatalf("unavailable view JSON = %s, want models:[] and no warnings", data)
	}
}
