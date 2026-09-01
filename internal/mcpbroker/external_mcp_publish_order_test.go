//go:build !windows

package mcpbroker

// Subtle: project.ParseContextSchema(nil, 0) is not a narrow schema, it is no
// schema — checkScopePresence finds no field to require and passes every
// tool, and project.FilterKnownContextFields strips every stored context key. A
// connection reachable before its schema is published is a call answered as
// though the grant were empty.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Deliberate: a respawn onto a known id replaces the schema, not merges with
// it — the respawn path doesn't go through Stop, so a child that stopped
// declaring a schema must not leave relay enforcing its predecessor's.
func TestPublishOrder_SchemaTracksTheLiveConnection(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	declaring := stdioMcp("mcp-schema", bin)
	declaring.Env = config.SecretMapFromPlain(map[string]string{"RELAY_TESTMCP_CONTEXT": "v2"})
	if err := m.startOne(ctx, &declaring); err != nil {
		t.Fatalf("startOne (declaring): %v", err)
	}
	if s := m.McpSurfaceFor("mcp-schema"); len(s.Schema) == 0 || s.SchemaVersion != 2 {
		t.Fatalf("surface after the declaring start = %+v", s)
	}

	silent := stdioMcp("mcp-schema", bin)
	if err := m.startOne(ctx, &silent); err != nil {
		t.Fatalf("startOne (silent): %v", err)
	}

	s := m.McpSurfaceFor("mcp-schema")
	if len(s.Schema) != 0 || s.SchemaVersion != 0 {
		t.Errorf("surface = %+v, want empty: relay is holding a schema the live child never declared", s)
	}
}

// Deliberate: publication and schema write happen in one critical section —
// a reader holding the manager's lock cannot observe a connection without the
// declaration that governs it.
func TestPublishOrder_NoConnectionIsReachableBeforeItsSchema(t *testing.T) {
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	ctx := context.Background()

	const starts = 40
	for i := 0; i < starts; i++ {
		id := fmt.Sprintf("mcp-order-%d", i)
		cfg := stdioMcp(id, bin)
		cfg.Env = config.SecretMapFromPlain(map[string]string{"RELAY_TESTMCP_CONTEXT": "v2"})

		stop := make(chan struct{})
		bad := make(chan string, 1)
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if !m.IsConnected(id) {
					continue
				}
				if s := m.McpSurfaceFor(id); len(s.Schema) == 0 {
					select {
					case bad <- "reachable with no context schema":
					default:
					}
					return
				} else if len(s.Tools) == 0 {
					select {
					case bad <- "reachable with no tools":
					default:
					}
					return
				}
				return
			}
		}()

		if err := m.startOne(ctx, &cfg); err != nil {
			close(stop)
			t.Fatalf("startOne %s: %v", id, err)
		}
		// Let the reader finish its observation before tearing it down.
		time.Sleep(time.Millisecond)
		close(stop)
		select {
		case why := <-bad:
			t.Fatalf("%s was %s", id, why)
		default:
		}
	}
}
