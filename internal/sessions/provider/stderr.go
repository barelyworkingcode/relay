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
	"time"
	"unicode/utf8"
)

const (
	stderrWarnLineLimit    = 20
	stderrTailLines        = 10
	stderrLineMaxBytes     = 1024
	msgProviderStderr      = "provider stderr"
	msgProviderStderrLimit = "provider stderr: warn limit reached, later lines at debug"

	// stderrLineKeepBytes bounds the memory one line may hold. It sits well
	// above stderrLineMaxBytes so a secret straddling the logged cut is still
	// whole when redaction runs; bytes past it are read and discarded.
	stderrLineKeepBytes = 64 * 1024

	redactedPlaceholder = "[redacted]"
)

// providerDrainTimeout bounds how long waitForExit waits, after Wait returns,
// for the stdout and stderr readers to reach EOF. Each provider reads it
// once at spawn.
var providerDrainTimeout = 2 * time.Second

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

// newStdoutPipe returns an os.Pipe whose write end becomes cmd.Stdout.
// cmd.StdoutPipe is deliberately not used: Wait closes that pipe's read end,
// racing the reader, and the lines lost to that race are the child's last
// events. The caller closes w after Start; the stdout reader closes r at EOF.
func newStdoutPipe(cmd *exec.Cmd) (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = w
	return r, w, nil
}

// spawnOutput is one spawn's stdout and stderr read ends and the signals
// their readers raise once they finish.
type spawnOutput struct {
	stdoutR, stderrR *os.File
	stdoutDone       chan struct{}
	stderrTail       chan []string
}

func newSpawnOutput(stdoutR, stderrR *os.File) *spawnOutput {
	return &spawnOutput{
		stdoutR:    stdoutR,
		stderrR:    stderrR,
		stdoutDone: make(chan struct{}),
		stderrTail: make(chan []string, 1),
	}
}

// drain waits for both readers and returns the stderr tail. The deadline is
// deliberate: a grandchild that inherited either write end can hold it open
// long after the child exits, and process_exited must not wait on it.
func (o *spawnOutput) drain(timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	_ = o.stdoutR.SetReadDeadline(deadline)
	_ = o.stderrR.SetReadDeadline(deadline)
	<-o.stdoutDone
	return <-o.stderrTail
}

func logStdoutReadError(sessionID, kind string, err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Debug("provider stdout still open after exit; stopped reading", "session", sessionID, "kind", kind)
		return
	}
	slog.Error("provider stdout read error", "session", sessionID, "kind", kind, "error", err)
}

func warnProviderExit(sessionID, kind string, exitCode int, tail []string) {
	if exitCode == 0 {
		return
	}
	slog.Warn("provider exited with error", "session", sessionID, "kind", kind, "exitCode", exitCode, "stderr_tail", tail)
}

// logProviderStderr logs each non-empty stderr line of one provider spawn.
// Lines arriving before the spawn's first stdout line log at Warn, up to
// stderrWarnLineLimit of them; the rest log at Debug. It reads r to EOF,
// closes it, and returns the last stderrTailLines logged texts, oldest first.
func logProviderStderr(r io.ReadCloser, sessionID, kind string, sawStdout *atomic.Bool, secrets ...string) []string {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, 4096)
	warned := 0
	limitLogged := false
	tail := make([]string, 0, stderrTailLines)
	for {
		raw, err := readBoundedLine(br)
		if line := strings.TrimSuffix(string(raw), "\r"); line != "" {
			text, truncated := truncateOnRune(redactStderrLine(line, secrets), stderrLineMaxBytes)
			if len(tail) == stderrTailLines {
				tail = append(tail[:0], tail[1:]...)
			}
			tail = append(tail, text)
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
			return tail
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
