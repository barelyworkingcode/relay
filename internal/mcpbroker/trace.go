package mcpbroker

import (
	"net/url"

	"github.com/barelyworkingcode/relay/internal/config"
)

// CarriesTrace reports whether relay may put its trace ID on a call to this
// MCP. A stdio child is relay's own process; an HTTP MCP receives it only when
// it is on this machine, so a trace ID never leaves for a third-party server.
func CarriesTrace(cfg *config.ExternalMcp) bool {
	if cfg == nil {
		return false
	}
	if !cfg.IsHTTP() {
		return true
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}
