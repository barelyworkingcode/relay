package modelbroker

import "strings"

// Passthrough is the model endpoint's second branch (plans/client-model-
// routing.md): a request for a provider's own model, forwarded with the
// client's own credential instead of being served by the broker. Everything
// here is the pure decision of which branch a request takes; the forwarding
// and the credential rules live with the handler.

// passthroughPrefixes is the fixed path table for the routes that are
// forwarded by path alone, never by the request's model: relayLLM mounts the
// same three (/api/ under router.anthropic, /chatgpt/ and /openai/ under
// router.passthrough). Relay owns this table rather than reading relayLLM's
// configuration, so what reaches a cloud provider is decided here and cannot
// change under a config edit relay never saw. A trailing slash is part of the
// prefix: "/openai" alone is not a route.
var passthroughPrefixes = []string{"/api/", "/chatgpt/", "/openai/"}

// MatchPassthrough reports whether path is one of the passthrough routes and
// names it ("api", "chatgpt" or "openai"). A path that is not already in
// clean form (a ".." or "." element, an empty element) never matches: it
// would name a different route once anything downstream cleaned it, and
// routing must decide on exactly the path that is forwarded.
func MatchPassthrough(path string) (name string, ok bool) {
	if !cleanRequestPath(path) {
		return "", false
	}
	for _, prefix := range passthroughPrefixes {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return strings.Trim(prefix, "/"), true
		}
	}
	return "", false
}

// cleanRequestPath reports whether path has no "." or ".." element and no
// empty element other than a final trailing slash.
func cleanRequestPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	elems := strings.Split(path[1:], "/")
	for i, e := range elems {
		switch e {
		case ".", "..":
			return false
		case "":
			if i != len(elems)-1 {
				return false
			}
		}
	}
	return true
}

// IsMessagesRoute reports whether path is one of the two Anthropic Messages
// routes, the only routes whose branch depends on the request's model: a
// Claude model relay does not manage is forwarded to Anthropic, everything
// else is served or refused by the broker.
func IsMessagesRoute(path string) bool {
	return path == "/v1/messages" || path == "/v1/messages/count_tokens"
}

// MessagesBranch is what a /v1/messages request's model says about where it
// goes.
type MessagesBranch int

const (
	// MessagesLocal: the model is a modelMap key in relay's catalog. Served by
	// the broker, scoped to the caller's grant.
	MessagesLocal MessagesBranch = iota
	// MessagesAnthropic: a Claude model relay does not manage. Forwarded to
	// Anthropic with the client's own credential.
	MessagesAnthropic
	// MessagesUnknown: neither. A 404 — including a catalog model that is not
	// a modelMap key, which has no Anthropic-shaped way to be served and must
	// not be sent to Anthropic.
	MessagesUnknown
)

// ClassifyMessagesModel decides the branch for a /v1/messages request naming
// requested, against rows (a catalog snapshot that has been resolved for this
// id, so "not in the catalog" is a fresh answer and never a stale one). The
// catalog is checked before the id's shape: a modelMap key that itself looks
// like a Claude id ("claude-haiku-4-5" mapped to a local model) is relay's to
// serve, not Anthropic's.
//
// A model that is in the catalog but is not a modelMap row (a bare managed
// alias, an endpoint model) is MessagesUnknown. relayLLM only translates a
// Messages request for a modelMap key; anything else on that route would fall
// through to its real-Anthropic passthrough, which is exactly where a typo'd
// or mis-routed local model name must never go.
//
// A model that is neither in the catalog nor Claude-shaped is also
// MessagesUnknown: a mistyped local name must not send its prompt to a cloud
// provider.
func ClassifyMessagesModel(requested string, rows []Row) MessagesBranch {
	if _, row, ok := canonicalize(requested, rows); ok {
		if classify(row) == groupModelMap {
			return MessagesLocal
		}
		return MessagesUnknown
	}
	if IsClaudeModelID(requested) {
		return MessagesAnthropic
	}
	return MessagesUnknown
}

// IsClaudeModelID reports whether id has the shape of an Anthropic model id:
// "claude-" followed by anything, compared case-insensitively. Claude Code
// sends the full id ("claude-opus-5"); the short aliases it accepts on its
// command line (opus, sonnet) are resolved before a request is made.
func IsClaudeModelID(id string) bool {
	const prefix = "claude-"
	return len(id) > len(prefix) && strings.EqualFold(id[:len(prefix)], prefix)
}
