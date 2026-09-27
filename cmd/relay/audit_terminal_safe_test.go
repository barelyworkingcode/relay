package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

func TestTerminalSafe(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain ascii", "read_file", "read_file"},
		{"utf-8 passes through", "café → done", "café → done"},
		{"backslash passes through", `C:\Users\p1`, `C:\Users\p1`},
		{"nul", "a\x00b", `a\x00b`},
		{"tab", "a\tb", `a\x09b`},
		{"newline", "a\nb", `a\x0ab`},
		{"carriage return", "a\rb", `a\x0db`},
		{"bel", "a\ab", `a\x07b`},
		{"esc sequence", "\x1b[2K", `\x1b[2K`},
		{"unit separator", "a\x1fb", `a\x1fb`},
		{"del", "a\x7fb", `a\x7fb`},
		{"c1 first", "a\u0080b", `a\u0080b`},
		{"c1 csi", "a\u009bb", `a\u009bb`},
		{"c1 last", "a\u009fb", `a\u009fb`},
		{"invalid utf-8 byte", "a\x9bb", `a\x9bb`},
		{"first rune past c1", "a\u00a0b", "a\u00a0b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalSafe(tc.in); got != tc.want {
				t.Errorf("terminalSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteAuditTable_EscapesControlInEveryStringCell(t *testing.T) {
	events := []audit.AuditEvent{{
		Outcome: "error\x1b[31m",
		Actor:   audit.AuditActor{ProjectName: "Acme\x1b[2K", ClientID: "devbox\x1b[1A"},
		McpID:   "fsmcp\x1b]0;t\a",
		Tool:    "read\tfile\x1b[2J",
		Error:   "failed \x1b[1A\x1b[2K",
		Access:  config.AccessRead,
		McpRoot: "/srv/p1\x1b[2K",
	}}
	var buf bytes.Buffer
	writeAuditTable(&buf, events, true)
	out := buf.String()

	if strings.Contains(out, "\x1b") {
		t.Errorf("rendered audit table contains a raw ESC:\n%q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + row + authority line):\n%q", len(lines), out)
	}
	if cells := strings.Split(lines[1], "\t"); len(cells) != 8 {
		t.Errorf("row split into %d cells, want 8; a raw tab in TOOL forged a column: %q", len(cells), lines[1])
	}
}
