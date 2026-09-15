package modelbroker

import (
	"encoding/json"
	"testing"
)

func TestErrorBodies_AreValidJSON(t *testing.T) {
	ctors := []func(Shape) ErrorBody{
		UnauthorizedError,
		RouteNotFoundError,
		RemoteProjectError,
		HostUnavailableError,
		TooManyRequestsError,
	}
	for _, ctor := range ctors {
		for _, shape := range []Shape{ShapeOpenAI, ShapeAnthropic} {
			eb := ctor(shape)
			var v map[string]any
			if err := json.Unmarshal(eb.Body, &v); err != nil {
				t.Fatalf("body not valid JSON: %s: %v", eb.Body, err)
			}
		}
	}
}

func TestModelNotFoundError_IdenticalBodyDistinctReason(t *testing.T) {
	for _, shape := range []Shape{ShapeOpenAI, ShapeAnthropic} {
		denied := ModelNotFoundError(shape, ReasonDenied)
		notFound := ModelNotFoundError(shape, ReasonNotFound)
		if denied.Status != 404 || notFound.Status != 404 {
			t.Fatalf("expected both 404, got %d / %d", denied.Status, notFound.Status)
		}
		if string(denied.Body) != string(notFound.Body) {
			t.Fatalf("wire bodies differ: %s vs %s", denied.Body, notFound.Body)
		}
		if denied.Reason == notFound.Reason {
			t.Fatalf("expected distinct internal reasons, got %q for both", denied.Reason)
		}
	}
}

func TestUnauthorizedError_Shapes(t *testing.T) {
	openai := UnauthorizedError(ShapeOpenAI)
	anthropic := UnauthorizedError(ShapeAnthropic)
	if openai.Status != 401 || anthropic.Status != 401 {
		t.Fatalf("expected 401s, got %d / %d", openai.Status, anthropic.Status)
	}
	if string(openai.Body) == string(anthropic.Body) {
		t.Fatalf("expected shape-specific bodies, got identical bodies")
	}
	var oa struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(openai.Body, &oa); err != nil || oa.Error.Type != "authentication_error" {
		t.Fatalf("openai body malformed: %s (%v)", openai.Body, err)
	}
	var an struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(anthropic.Body, &an); err != nil || an.Type != "error" || an.Error.Type != "authentication_error" {
		t.Fatalf("anthropic body malformed: %s (%v)", anthropic.Body, err)
	}
}
