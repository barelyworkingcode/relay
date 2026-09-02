package login

import (
	"bytes"
	"sync"
)

// loginSyncBuffer is this package's copy of cmd/relay's lrSyncBuffer
// (login_routes_test.go): a slog target safe for a test that reads what it
// captured while a concurrent handler may still be writing to it. The two
// packages do not share a test helper module, so each keeps the shape it
// needs.
type loginSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *loginSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *loginSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// boolPtr is this package's copy of cmd/relay's own helper
// (audit_cmd_test.go), needed here for the same reason loginSyncBuffer is.
func boolPtr(b bool) *bool { return &b }
