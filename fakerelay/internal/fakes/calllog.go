package fakes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CallLog appends one JSON line per call to DIR/fakes/<name>.jsonl.
type CallLog struct {
	dir string
	mu  sync.Mutex
}

func NewCallLog(dir string) *CallLog { return &CallLog{dir: filepath.Join(dir, "fakes")} }

// Record writes {"ts","fake","call","args"}. A failed write is dropped: the
// log is evidence for a test, never part of an answer.
func (l *CallLog) Record(name, call string, args any) {
	if l == nil {
		return
	}
	b, err := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "fake": name, "call": call, "args": args})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if os.MkdirAll(l.dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
