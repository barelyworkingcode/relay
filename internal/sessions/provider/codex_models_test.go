package provider

import (
	"context"
	"os"
	"reflect"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// The picker lists the models codex marks "list", as codex/<slug> rows in the
// Codex group; hidden ones stay out.
func TestFetchCodexModels_ListsVisibleModelsFromDebugModels(t *testing.T) {
	bin := buildTestCodexBinary(t)
	raw, err := os.ReadFile("testdata/codex/models.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTCODEX_MODELS", string(raw))

	got := FetchCodexModels(context.Background(), bin)

	want := []sessionstypes.ModelInfo{
		{Label: "GPT-6 Luna", Value: "codex/gpt-6-luna", Group: "Codex", Provider: "codex"},
		{Label: "GPT-6 Sol", Value: "codex/gpt-6-sol", Group: "Codex", Provider: "codex"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v, want %+v", got, want)
	}
}

func TestFetchCodexModels_MissingBinaryReturnsNil(t *testing.T) {
	if got := FetchCodexModels(context.Background(), "/nonexistent/codex/binary"); got != nil {
		t.Fatalf("expected nil for a missing binary, got %+v", got)
	}
}
