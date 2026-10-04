package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

func TestOpenMcpStderrLogWritesUnderTheLogDirWithPrivateModes(t *testing.T) {
	mkSandboxRelayHome(t)
	w, err := openMcpStderrLog("acme-mcp.v1")
	if err != nil {
		t.Fatalf("openMcpStderrLog: %v", err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	dir := filepath.Join(bridge.ConfigDir(), "logs", "mcp")
	path := filepath.Join(dir, "acme-mcp.v1.log")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("log file %s = %q, %v", path, data, err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0700 {
		t.Errorf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestOpenMcpStderrLogRefusesIdsThatAreNotSafeFileNames(t *testing.T) {
	home := mkSandboxRelayHome(t)
	bad := []string{"", "../escape", "a/b", ".hidden", "-lead", "has space", "nul\x00byte", "a" + strings.Repeat("b", 128)}
	for _, id := range bad {
		if w, err := openMcpStderrLog(id); err == nil {
			_ = w.Close()
			t.Errorf("id %q was accepted", id)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "logs", "escape.log")); err == nil {
		t.Error("a traversal id created a file outside the mcp log dir")
	}
	w, err := openMcpStderrLog("a" + strings.Repeat("b", 127))
	if err != nil {
		t.Fatalf("128-character id refused: %v", err)
	}
	_ = w.Close()
}
