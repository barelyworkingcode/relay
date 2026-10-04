//go:build !windows

package mcpbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
)

func TestCarriesTrace(t *testing.T) {
	http := func(u string) *config.ExternalMcp {
		return &config.ExternalMcp{ID: "m", Transport: "http", URL: u}
	}
	cases := []struct {
		name string
		cfg  *config.ExternalMcp
		want bool
	}{
		{"stdio", &config.ExternalMcp{ID: "m", Transport: "stdio", Command: "x"}, true},
		{"localhost", http("http://localhost:8080/mcp"), true},
		{"127.0.0.1", http("http://127.0.0.1:8080/mcp"), true},
		{"127.8.9.10", http("http://127.8.9.10/mcp"), true},
		{"ipv6 loopback", http("http://[::1]:8080/mcp"), true},
		{"example.com", http("https://example.com/mcp"), false},
		{"private 10.0.0.1", http("http://10.0.0.1/mcp"), false},
		{"localhost.example.com", http("http://localhost.example.com/mcp"), false},
		{"unparseable URL", http("http://%zz"), false},
		{"empty URL", http(""), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CarriesTrace(c.cfg); got != c.want {
				t.Errorf("CarriesTrace = %v, want %v", got, c.want)
			}
		})
	}
}

func envSeenByChild(t *testing.T, m *Manager, id string) string {
	t.Helper()
	conn := m.ConnectionForTest(id)
	if conn == nil {
		t.Fatalf("testmcp %q did not come up", id)
	}
	res, err := conn.SendRequest(context.Background(), "env", nil)
	if err != nil {
		t.Fatalf("env request: %v", err)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("decode %q: %v", res, err)
	}
	return out.Value
}

func TestStdioSpawnSetsTraceFromContext(t *testing.T) {
	const id = "abcd1234efgh5678"
	bin := buildTestMcpBinary(t)
	t.Setenv("RELAY_TRACE_ID", "inherited-trace-id")
	cfg := func() *config.ExternalMcp {
		return &config.ExternalMcp{
			ID: "tm", DisplayName: "tm", Transport: "stdio", Command: bin,
			Env: map[string]config.Secret{"RELAY_TRACE_ID": config.NewSecret("configured-trace-id")},
		}
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"trace in ctx reaches the child", logging.ContextWithTrace(context.Background(), id), id},
		{"no trace: configured and inherited values are scrubbed", context.Background(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewManager(nil)
			t.Cleanup(m.StopAll)
			if err := m.Reload(c.ctx, "tm", cfg()); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			if got := envSeenByChild(t, m, "tm"); got != c.want {
				t.Errorf("child RELAY_TRACE_ID = %q, want %q", got, c.want)
			}
		})
	}
}
