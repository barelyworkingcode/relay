package hook

import (
	"testing"
	"time"
)

// The host waits 60s for a decision and Claude Code kills the hook at 120s.
// A hook killed first is "no decision", which Claude Code refuses for MCP
// tools, so the client timeout must sit strictly between the two.
func TestClientTimeout_SitsBetweenHostWaitAndClaudeHookTimeout(t *testing.T) {
	const hostWait, claudeHookTimeout = 60 * time.Second, 120 * time.Second
	if clientTimeout <= hostWait || clientTimeout >= claudeHookTimeout {
		t.Fatalf("clientTimeout = %v, want strictly between %v and %v", clientTimeout, hostWait, claudeHookTimeout)
	}
}
