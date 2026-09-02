package mcpbroker

import (
	"encoding/json"
	"testing"
)

// The connection table and the runtime schema table are unexported because
// nothing outside this package may install a connection relay did not
// handshake, or assert a context schema no MCP declared. Several of
// cmd/relay's tests legitimately need both: the router, the audit
// instrumentation and the project-scoping code all take a *Manager, and
// their tests have to stand a fake MCP behind it without spawning one.
//
// These three are that seam, and they are the whole of it. Each panics
// outside a test binary, for the reason config.NewSettingsStoreWithCache
// does: a constructor that puts the manager into a state no handshake could
// produce is a weakening, and a weakening introduced for a test is the one
// most likely to survive into production (ADR-016 decision 8). Reflection
// and unsafe are not an alternative here — they would remove the panic
// along with the boundary.

// SetConnectionForTest installs conn as the live connection for id. It does
// NOT run a handshake, so the connection carries whatever tools the caller
// gave it and no context schema at all — use SetContextSchemaForTest to
// supply one.
func (m *Manager) SetConnectionForTest(id string, conn Connection) {
	if !testing.Testing() {
		panic("mcpbroker: SetConnectionForTest is a test seam and must not be reached in a shipped binary")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[id] = conn
}

// ConnectionForTest returns the live connection for id, or nil. It is the
// read half: a test that has to drive a real stdio child off-protocol (kill
// it, say) needs the connection object, and IsConnected only answers yes/no.
func (m *Manager) ConnectionForTest(id string) Connection {
	if !testing.Testing() {
		panic("mcpbroker: ConnectionForTest is a test seam and must not be reached in a shipped binary")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[id]
}

// SetContextSchemaForTest publishes a runtime context schema and its version
// for id, as a handshake would. Version and schema are set together on
// purpose: a version with no schema behind it is the state McpSurfaceFor
// exists to make unrepresentable.
func (m *Manager) SetContextSchemaForTest(id string, schema json.RawMessage, version int) {
	if !testing.Testing() {
		panic("mcpbroker: SetContextSchemaForTest is a test seam and must not be reached in a shipped binary")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schemas[id] = schema
	m.schemaVersions[id] = version
}
