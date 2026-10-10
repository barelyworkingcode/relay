// Package calllog is the call-log writer shared by the fakes. A line is one
// JSON object written with a single write(2) on an O_APPEND descriptor, and
// callers append before they reply, so a reader that has seen the reply needs
// no wait to see the line.
package calllog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Writer appends JSON lines to one file.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

// Open creates the file (0600) if needed and opens it for append.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open call log %q: %w", path, err)
	}
	return &Writer{f: f}, nil
}

// Append marshals v and writes it as one line. The mutex only keeps the order
// of concurrent callers stable; atomicity of a line comes from the single
// write.
func (w *Writer) Append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal call log line: %w", err)
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(b); err != nil {
		return fmt.Errorf("write call log: %w", err)
	}
	return nil
}

// Close closes the file.
func (w *Writer) Close() error { return w.f.Close() }

// Call is one request seen by a fake MCP server or the fake model host.
type Call struct {
	TS           string          `json:"ts"`
	Transport    string          `json:"transport"`
	Method       string          `json:"method"`
	ID           json.RawMessage `json:"id,omitempty"`
	Params       json.RawMessage `json:"params,omitempty"`
	ParamsSHA256 string          `json:"params_sha256"`
	Auth         string          `json:"auth"`
}

// NewCall stamps a Call. raw is the exact byte range the hash covers; params
// is kept only when it is valid JSON, so a form body or a query string never
// lands in the log verbatim.
func NewCall(transport, method string, id json.RawMessage, params []byte, raw []byte, auth string) Call {
	c := Call{
		TS:           time.Now().UTC().Format(time.RFC3339Nano),
		Transport:    transport,
		Method:       method,
		ID:           id,
		ParamsSHA256: SHA256Hex(raw),
		Auth:         auth,
	}
	if len(params) > 0 && json.Valid(params) {
		c.Params = append(json.RawMessage(nil), params...)
	}
	return c
}

// SHA256Hex is the lower-case hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AuthLabel turns an Authorization header into the label the log keeps: the
// token itself never appears, only its hash.
func AuthLabel(header string) string {
	const scheme = "bearer "
	if len(header) > len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return "bearer:" + SHA256Hex([]byte(strings.TrimSpace(header[len(scheme):])))
	}
	return "none"
}
