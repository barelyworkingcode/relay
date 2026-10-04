package logging_test

import (
	"context"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
)

func TestRelaySessionsImportsNoStdlibLog(t *testing.T) {
	files, err := filepath.Glob("../../cmd/relaysessions/*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("scan found no files in cmd/relaysessions")
	}
	fset := token.NewFileSet()
	for _, f := range files {
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"log"` {
				t.Errorf("%s imports stdlib log", filepath.Base(f))
			}
		}
	}
}

func TestRejectedTraceValueNeverLogged(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	buf := &lockedBuf{}
	logging.Install(buf, logging.Options{
		DefaultService: testService,
		Getenv:         envOf(map[string]string{logging.EnvLogLevel: "debug"}),
	})
	const canary = "CANARY-TRACE with spaces"
	logging.TraceIDOrNew(canary)
	logging.ValidTraceID(canary)
	logging.ContextWithTrace(context.Background(), canary)
	if strings.Contains(buf.String(), "CANARY") {
		t.Errorf("rejected trace value reached the log: %q", buf.String())
	}
}
