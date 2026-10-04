package logging_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestTraceIDValidation(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"abcdefg", false},
		{"abcdefgh", true},
		{"ab_cd-EF9", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"abc.defgh", false},
		{"abc defgh", false},
		{"abcdefgh\n", false},
		{"abcdéfgh", false},
		{"", false},
	}
	for _, c := range cases {
		if got := logging.ValidTraceID(c.id); got != c.ok {
			t.Errorf("ValidTraceID(%q) = %v, want %v", c.id, got, c.ok)
		}
		out := logging.TraceIDOrNew(c.id)
		if c.ok && out != c.id {
			t.Errorf("TraceIDOrNew(%q) = %q, want kept", c.id, out)
		}
		if !c.ok && (out == c.id || !hex32.MatchString(out)) {
			t.Errorf("TraceIDOrNew(%q) = %q, want fresh 32 hex", c.id, out)
		}
	}
}

func TestNewTraceID(t *testing.T) {
	a, b := logging.NewTraceID(), logging.NewTraceID()
	if !hex32.MatchString(a) || !hex32.MatchString(b) || a == b {
		t.Errorf("NewTraceID = %q, %q", a, b)
	}
	if !logging.ValidTraceID(a) {
		t.Error("generated id fails own validation")
	}
}

func TestTraceContext(t *testing.T) {
	ctx := context.Background()
	if got := logging.TraceFromContext(ctx); got != "" {
		t.Errorf("empty context trace = %q", got)
	}
	withID := logging.ContextWithTrace(ctx, "abcd1234")
	if got := logging.TraceFromContext(withID); got != "abcd1234" {
		t.Errorf("trace = %q", got)
	}
	if got := logging.TraceFromContext(logging.ContextWithTrace(ctx, "short")); got != "" {
		t.Errorf("invalid id stored: %q", got)
	}
	if got := logging.TraceFromContext(logging.ContextWithTrace(withID, "bad id!!!!")); got != "abcd1234" {
		t.Errorf("invalid id replaced existing trace: %q", got)
	}
}
