package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// The id becomes a file name, so it is validated rather than sanitised.
var mcpLogIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// openMcpStderrLog opens <log dir>/mcp/<id>.log for an external MCP's stderr.
func openMcpStderrLog(id string) (io.WriteCloser, error) {
	if !mcpLogIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid MCP id for log file")
	}
	dir, err := serviceLogDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "mcp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create MCP log directory: %w", err)
	}
	return openRotatingLog(filepath.Join(dir, id+".log"))
}
