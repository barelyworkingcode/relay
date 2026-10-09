package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Audit appends JSON lines to logs/audit/toolcalls.jsonl.
type Audit struct {
	path string
	mu   sync.Mutex
}

// NewAudit creates DIR/logs/audit.
func NewAudit(dir string) (*Audit, error) {
	p := filepath.Join(dir, "logs", "audit")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return nil, err
	}
	return &Audit{path: filepath.Join(p, "toolcalls.jsonl")}, nil
}

// Path is the audit file.
func (a *Audit) Path() string { return a.path }

// Append writes one line; the caller sees the failure, as relay's callers do.
func (a *Audit) Append(rec any) error {
	var b []byte
	switch r := rec.(type) {
	case json.RawMessage:
		var buf bytes.Buffer
		if err := json.Compact(&buf, r); err != nil {
			return err
		}
		b = buf.Bytes()
	default:
		var err error
		if b, err = json.Marshal(rec); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(append([]byte(nil), b...), '\n'))
	return err
}
