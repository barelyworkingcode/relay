package modelbroker

import "testing"

func TestMatchRoute_Allowlisted(t *testing.T) {
	cases := []struct {
		method, path string
		shape        Shape
		source       ModelSource
	}{
		{"GET", "/v1/models", ShapeOpenAI, ModelSourceNone},
		{"GET", "/models", ShapeOpenAI, ModelSourceNone},
		{"POST", "/v1/chat/completions", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/chat/completions", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/v1/completions", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/completions", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/v1/embeddings", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/embeddings", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/v1/responses", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/responses", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/v1/audio/transcriptions", ShapeOpenAI, ModelSourceMultipart},
		{"POST", "/v1/audio/speech", ShapeOpenAI, ModelSourceJSONBody},
		{"POST", "/v1/messages", ShapeAnthropic, ModelSourceJSONBody},
		{"POST", "/v1/messages/count_tokens", ShapeAnthropic, ModelSourceJSONBody},
		{"GET", "/health", ShapeOpenAI, ModelSourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			route, ok := MatchRoute(tc.method, tc.path)
			if !ok {
				t.Fatalf("MatchRoute(%q, %q) refused, want allowed", tc.method, tc.path)
			}
			if route.Shape != tc.shape || route.Source != tc.source {
				t.Fatalf("MatchRoute(%q, %q) = %+v, want shape %v source %v", tc.method, tc.path, route, tc.shape, tc.source)
			}
		})
	}
}

func TestMatchRoute_RefusesNonRoutes(t *testing.T) {
	cases := []struct{ method, path string }{
		{"POST", "/api/claude_cli/bootstrap"},
		{"POST", "/openai/v1/responses"},
		{"POST", "/models/load"},
		{"POST", "/models/unload"},
		{"GET", "/api/anything"},
		{"POST", "/anthropic/v1/messages"},
		{"PUT", "/v1/chat/completions"},   // right path, wrong method
		{"GET", "/v1/chat/completions"},   // right path, wrong method
		{"POST", "/v1/chat/completions/"}, // trailing slash is a different path
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if _, ok := MatchRoute(tc.method, tc.path); ok {
				t.Fatalf("MatchRoute(%q, %q) allowed, want refused", tc.method, tc.path)
			}
		})
	}
}
