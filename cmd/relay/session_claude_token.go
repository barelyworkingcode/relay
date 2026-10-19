package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/config"
)

// maxClaudeTokenBytes bounds the token file. It stays under a pipe buffer so
// the host's write of the token never blocks.
const maxClaudeTokenBytes = 4096

// claudeTokenPath resolves claude_token_file: "" when unset, "~/" expanded
// with the home directory, an absolute path cleaned. Any other value is an
// error naming the key and value.
func claudeTokenPath(settings *config.Settings) (string, error) {
	raw := settings.ClaudeTokenFile
	if raw == "" {
		return "", nil
	}
	var p string
	switch {
	case strings.HasPrefix(raw, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("claude_token_file %q: resolve home directory: %w", raw, err)
		}
		p = filepath.Join(home, raw[2:])
	case filepath.IsAbs(raw):
		p = filepath.Clean(raw)
	default:
		return "", fmt.Errorf("claude_token_file %q: must be an absolute path or start with ~/", raw)
	}
	return p, nil
}

// loadClaudeToken returns the token for one launch, or "" when the setting is
// empty.
func loadClaudeToken(settings *config.Settings) (string, error) {
	path, err := claudeTokenPath(settings)
	if err != nil || path == "" {
		return "", err
	}
	return readClaudeToken(path)
}

// readClaudeToken opens the file O_NONBLOCK, so a FIFO cannot hold a launch,
// refuses a non-regular file by fstat, and reads at most one byte past the
// cap. Every error names the path and the reason, never the bytes: the reason
// is the syscall error or a fixed phrase.
func readClaudeToken(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", claudeTokenError(path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", claudeTokenError(path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("claude_token_file %s: not a regular file", path)
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxClaudeTokenBytes+1))
	if err != nil {
		return "", claudeTokenError(path, err)
	}
	if len(buf) > maxClaudeTokenBytes {
		return "", fmt.Errorf("claude_token_file %s: larger than %d bytes", path, maxClaudeTokenBytes)
	}
	token := string(bytes.Trim(buf, " \t\r\n\v\f"))
	if token == "" {
		return "", fmt.Errorf("claude_token_file %s: empty", path)
	}
	return token, nil
}

// claudeTokenError reports the underlying errno text only; a PathError's own
// text already carries the path, which this wraps once.
func claudeTokenError(path string, err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("claude_token_file %s: %s", path, errno.Error())
	}
	return fmt.Errorf("claude_token_file %s: read failed", path)
}
