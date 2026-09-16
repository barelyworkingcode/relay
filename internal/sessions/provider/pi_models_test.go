package provider

import (
	"context"
	"testing"
)

func TestParsePiListModels_FixedWidthTable(t *testing.T) {
	table := "provider   model               context  max-out  thinking  images\n" +
		"anthropic  claude-sonnet-4     200K     16K      yes       yes\n" +
		"llama-cpp  Qwen3.6 27B Q4      128K     16.4K    no        no\n"

	models := parsePiListModels([]byte(table))
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(models), models)
	}
	if models[0].Value != "pi/anthropic/claude-sonnet-4" {
		t.Errorf("models[0].Value = %q", models[0].Value)
	}
	if models[1].Value != "pi/llama-cpp/Qwen3.6 27B Q4" {
		t.Errorf("models[1].Value = %q", models[1].Value)
	}
}

func TestParsePiListModels_MissingHeaderReturnsNil(t *testing.T) {
	if got := parsePiListModels([]byte("not a table\n")); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestFetchPiModels_MissingBinaryReturnsNil(t *testing.T) {
	got := FetchPiModels(context.Background(), "/nonexistent/pi/binary")
	if got != nil {
		t.Fatalf("expected nil for a missing binary, got %+v", got)
	}
}
