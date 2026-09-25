package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	stderrWarnLineLimit    = 20
	stderrLineMaxBytes     = 1024
	msgProviderStderr      = "provider stderr"
	msgProviderStderrLimit = "provider stderr: warn limit reached, later lines at debug"

	// stderrLineKeepBytes bounds the memory one line may hold. It sits well
	// above stderrLineMaxBytes so a secret straddling the logged cut is still
	// whole when redaction runs; bytes past it are read and discarded.
	stderrLineKeepBytes = 64 * 1024

	redactedPlaceholder = "[redacted]"
)

var (
	stderrModelKeyPattern = regexp.MustCompile(`rmk_[0-9a-f]{64}`)
	stderrSKTokenPattern  = regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)
	stderrBearerPattern   = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

// newStderrPipe returns an os.Pipe whose write end becomes cmd.Stderr.
// cmd.StderrPipe is deliberately not used: Wait closes that pipe's read end,
// racing the reader, and the line lost to that race is the one naming a
// launch failure. The caller closes w after Start and hands r to
// logProviderStderr, which reads to EOF.
func newStderrPipe(cmd *exec.Cmd) (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = w
	return r, w, nil
}

// logProviderStderr logs each non-empty stderr line of one provider spawn.
// Lines arriving before the spawn's first stdout line log at Warn, up to
// stderrWarnLineLimit of them; the rest log at Debug. It reads r to EOF and
// then closes it.
func logProviderStderr(r io.ReadCloser, sessionID, kind string, sawStdout *atomic.Bool, secrets ...string) {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, 4096)
	warned := 0
	limitLogged := false
	for {
		raw, err := readBoundedLine(br)
		if line := strings.TrimSuffix(string(raw), "\r"); line != "" {
			text, truncated := truncateOnRune(redactStderrLine(line, secrets), stderrLineMaxBytes)
			attrs := []any{"session", sessionID, "kind", kind, "text", text}
			if truncated {
				attrs = append(attrs, "truncated", true)
			}
			switch {
			case sawStdout.Load():
				slog.Debug(msgProviderStderr, attrs...)
			case warned < stderrWarnLineLimit:
				warned++
				slog.Warn(msgProviderStderr, attrs...)
			default:
				if !limitLogged {
					limitLogged = true
					slog.Warn(msgProviderStderrLimit, "session", sessionID, "kind", kind, "limit", stderrWarnLineLimit)
				}
				slog.Debug(msgProviderStderr, attrs...)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				slog.Debug("provider stderr read error", "session", sessionID, "kind", kind, "error", err)
			}
			return
		}
	}
}

// readBoundedLine returns the next line without its '\n', keeping at most
// stderrLineKeepBytes of it and discarding the rest so the child never
// blocks on a full pipe.
func readBoundedLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if room := stderrLineKeepBytes - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\n")), err
	}
}

func truncateOnRune(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// redactStderrLine masks the exact secrets given, then anything shaped like
// a relay model key, an sk- API key or a bearer token. Empty secrets are
// skipped: replacing "" would splice the placeholder between every byte.
func redactStderrLine(line string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			line = strings.ReplaceAll(line, s, redactedPlaceholder)
		}
	}
	line = stderrModelKeyPattern.ReplaceAllLiteralString(line, "rmk_"+redactedPlaceholder)
	line = stderrSKTokenPattern.ReplaceAllLiteralString(line, "sk-"+redactedPlaceholder)
	return stderrBearerPattern.ReplaceAllString(line, "${1}"+redactedPlaceholder)
}
