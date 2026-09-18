package modelbroker

// Shape picks which API family's error and usage shapes apply to a route.
type Shape int

const (
	ShapeOpenAI Shape = iota
	ShapeAnthropic
)

// ModelSource says where a route's model id comes from, so the caller knows
// which extractor to run before this package's normalisation.
type ModelSource int

const (
	// ModelSourceNone means the route names no model (listing, health).
	ModelSourceNone ModelSource = iota
	// ModelSourceJSONBody means the model is the body's top-level "model".
	ModelSourceJSONBody
	// ModelSourceMultipart means the model is the "model" form field of a
	// multipart/form-data body.
	ModelSourceMultipart
)

// Route is one entry of the broker's explicit allowlist (spec §2.2).
type Route struct {
	Method string
	Path   string
	Shape  Shape
	Source ModelSource
}

// allowedRoutes is exactly spec §2.2's table, including the bare-path forms
// relayLLM's own openAIRoutesWithoutV1 accepts (router.go) for llama.cpp-
// style clients that post to a bare base URL. Anything not listed here is
// refused with the "route not found" 404 (spec §2.4), except the fixed
// passthrough paths MatchPassthrough recognises, which the handler forwards
// as the client's own request instead of brokering them. /models/load and
// /models/unload are deliberately absent, not merely unlisted by omission:
// they would let a caller reach a control-plane action through what is meant
// to be a data plane.
var allowedRoutes = []Route{
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

// MatchRoute reports whether method+path is on the broker's allowlist, and
// if so, which route entry matched. Matching is on method and exact path
// only — no prefix globbing, no trailing-slash tolerance, matching the
// spec's "explicit allowlist; anything else returns 404".
func MatchRoute(method, path string) (Route, bool) {
	for _, r := range allowedRoutes {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return Route{}, false
}
