package modelbroker

import "testing"

func TestMatchPassthrough(t *testing.T) {
	cases := []struct {
		path     string
		wantName string
		wantOK   bool
	}{
		{"/api/claude_cli/bootstrap", "api", true},
		{"/api/hello", "api", true},
		{"/chatgpt/codex/responses", "chatgpt", true},
		{"/openai/v1/models", "openai", true},
		{"/openai/v1/", "openai", true}, // a trailing slash is the route's own

		// Not routes: the prefix itself, other names, model routes.
		{"/api/", "", false},
		{"/openai", "", false},
		{"/openai/", "", false},
		{"/apix/foo", "", false},
		{"/v1/models", "", false},
		{"/v1/chat/completions", "", false},
		{"/models/load", "", false},
		{"/health", "", false},
		{"openai/v1/models", "", false},

		// A path that would name another route once cleaned is never a route.
		{"/openai/../v1/chat/completions", "", false},
		{"/openai/./v1/models", "", false},
		{"/openai//v1/models", "", false},
		{"/api/../v1/models", "", false},
	}
	for _, tc := range cases {
		name, ok := MatchPassthrough(tc.path)
		if ok != tc.wantOK || name != tc.wantName {
			t.Errorf("MatchPassthrough(%q) = %q, %v; want %q, %v", tc.path, name, ok, tc.wantName, tc.wantOK)
		}
	}
}

func TestIsMessagesRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/messages": true, "/v1/messages/count_tokens": true,
		"/v1/chat/completions": false, "/messages": false, "/v1/messages/": false,
	} {
		if got := IsMessagesRoute(path); got != want {
			t.Errorf("IsMessagesRoute(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsClaudeModelID(t *testing.T) {
	for id, want := range map[string]bool{
		"claude-opus-5": true, "claude-3-5-sonnet-20241022": true, "Claude-Haiku-4-5": true,
		"claude-": false, "claude": false, "sonnet": false, "gpt-5": false, "": false,
		"anthropic/claude-opus-5": false, "vCode": false,
	} {
		if got := IsClaudeModelID(id); got != want {
			t.Errorf("IsClaudeModelID(%q) = %v, want %v", id, got, want)
		}
	}
}

// classification table for a /v1/messages request against a catalog holding a
// managed alias, an endpoint model, a virtual model, a modelMap key that does
// not look like a Claude id and one that does.
func TestClassifyMessagesModel(t *testing.T) {
	rows := []Row{
		{ID: "qwen3-8b", OwnedBy: "llama.cpp"},
		{ID: "omlx/Chat", OwnedBy: "mlx-omni"},
		{ID: "vCode", OwnedBy: "virtual"},
		{ID: "relay/coder", OwnedBy: "anthropic-map", Target: "qwen3-8b"},
		{ID: "claude-haiku-4-5", OwnedBy: "anthropic-map", Target: "vCode"},
	}
	cases := []struct {
		name string
		id   string
		want MessagesBranch
	}{
		{"modelMap key -> local", "relay/coder", MessagesLocal},
		{"modelMap key shaped like a Claude id -> local, not Anthropic", "claude-haiku-4-5", MessagesLocal},
		{"Claude model not in the catalog -> Anthropic", "claude-opus-5", MessagesAnthropic},
		{"Claude id, any case -> Anthropic", "Claude-Sonnet-5", MessagesAnthropic},
		{"managed alias is not reachable through Messages -> unknown", "qwen3-8b", MessagesUnknown},
		{"llama/ spelling of a managed alias -> unknown", "llama/qwen3-8b", MessagesUnknown},
		{"endpoint model -> unknown", "omlx/Chat", MessagesUnknown},
		{"virtual model -> unknown", "vCode", MessagesUnknown},
		{"typo of a local name -> unknown, never Anthropic", "qwen3-8", MessagesUnknown},
		{"typo of a modelMap key -> unknown", "relay/codr", MessagesUnknown},
		{"another provider's model -> unknown", "gpt-5", MessagesUnknown},
		{"empty -> unknown", "", MessagesUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyMessagesModel(tc.id, rows); got != tc.want {
			t.Errorf("%s: ClassifyMessagesModel(%q) = %v, want %v", tc.name, tc.id, got, tc.want)
		}
	}
}
