package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

type stream struct {
	net.Conn
	r *bufio.Reader
	bridge.SandboxAttachResult
}

// sandboxAttach answers either an open stream or relay's refusal reason, and
// closes the connection on a refusal. An Error answer with no structured
// refusal reads as reason "error".
func sandboxAttach(ctx context.Context, e env, template, cwd string) (*stream, string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(e.ConfigDir, "relay.sock"))
	if err != nil {
		return nil, "", errors.New("bridge socket unreachable")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	s := &stream{Conn: conn, r: bufio.NewReader(conn)}
	args, _ := json.Marshal(bridge.SandboxAttachRequest{Template: template, Cwd: cwd, Cols: 120, Rows: 40})
	var ack bridge.BridgeResponse
	err = json.NewEncoder(conn).Encode(bridge.BridgeRequest{Type: bridge.ReqSandboxAttach, Arguments: args})
	if err == nil {
		var line []byte
		if line, err = s.r.ReadBytes('\n'); err == nil {
			err = json.Unmarshal(line, &ack)
		}
	}
	if err == nil && ack.Type == bridge.RespAttached {
		_ = json.Unmarshal(ack.Data, &s.SandboxAttachResult)
		return s, "", nil
	}
	_ = conn.Close()
	if err != nil {
		return nil, "", errors.New("no answer to the attach request")
	}
	var refusal bridge.SandboxRefusal
	if json.Unmarshal(ack.Data, &refusal) != nil || refusal.Reason == "" {
		refusal.Reason = "error"
	}
	return nil, refusal.Reason, nil
}

// liveSession is an attached sandbox session driven one shell line at a time.
type liveSession struct {
	s      *stream
	ID     string
	exited bool
}

// openSession answers either a live session or relay's refusal reason.
func openSession(ctx context.Context, e env, template, cwd string) (*liveSession, string, error) {
	s, reason, err := sandboxAttach(ctx, e, template, cwd)
	if err != nil || s == nil {
		return nil, reason, err
	}
	return &liveSession{s: s, ID: s.SessionID}, "", nil
}

// bindDeadline makes a read or write on the stream give up when ctx does.
func (l *liveSession) bindDeadline(ctx context.Context) (unbind func() bool) {
	dl, _ := ctx.Deadline()
	_ = l.s.SetDeadline(dl)
	return context.AfterFunc(ctx, func() { _ = l.s.SetDeadline(time.Now()) })
}

func (l *liveSession) send(data string) error {
	return json.NewEncoder(l.s).Encode(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte(data)})
}

// next reads one frame; output frames return their bytes, and an exit frame
// marks the session exited.
func (l *liveSession) next() ([]byte, error) {
	for {
		line, err := l.s.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var f bridge.StreamFrame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		switch f.Type {
		case bridge.StreamOutput:
			return f.Data, nil
		case bridge.StreamExit:
			l.exited = true
			return nil, errSessionExited
		}
	}
}

var errSessionExited = errors.New("the session exited")

// execMarker builds the line that reports line's exit status. The marker is
// assembled by printf at run time so the terminal's echo of this input can
// never contain it; markerPattern matches only the printed form.
func execMarker(tag string) (input string, pattern *regexp.Regexp) {
	return "printf '%s%s:%d\\n' 'DBVM' '" + tag + "' \"$?\"\n", regexp.MustCompile(`DBVM` + tag + `:(-?\d+)\r?\n`)
}

// Exec runs one shell line and answers its output up to the exit marker,
// without the terminal's echo of the line or of the marker command.
func (l *liveSession) Exec(ctx context.Context, line string) (out string, exit int, err error) {
	if l.exited {
		return "", 0, errSessionExited
	}
	defer l.bindDeadline(ctx)()
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	tag := hex.EncodeToString(raw)
	marker, pattern := execMarker(tag)
	line = strings.TrimRight(line, "\n")
	if err := l.send(line + "\n" + marker); err != nil {
		return "", 0, fmt.Errorf("could not send input: %w", err)
	}
	var transcript strings.Builder
	for {
		data, err := l.next()
		if err != nil {
			return transcript.String(), 0, fmt.Errorf("no exit marker: %w", err)
		}
		transcript.Write(data)
		if m := pattern.FindStringSubmatchIndex(transcript.String()); m != nil {
			t := transcript.String()
			code, _ := strconv.Atoi(t[m[2]:m[3]])
			return execOutput(t[:m[0]], line, tag), code, nil
		}
	}
}

var terminalNoise = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|[\x00-\x20\x7f]`)

// execOutput cuts the echoes out of a transcript that ends before the
// printed exit marker. A line-editing shell echoes the marker command after
// the output and a cooked terminal echoes it before, so that echo is removed
// wherever it sits, found by its tag. The input's echo is cut only when the
// first line carries it: with echo off, that line is output.
func execOutput(transcript, line, tag string) string {
	t := transcript
	if i := strings.Index(t, tag); i >= 0 {
		start := strings.LastIndex(t[:i], "\n") + 1
		end := len(t)
		if j := strings.Index(t[i:], "\n"); j >= 0 {
			end = i + j + 1
		}
		t = t[:start] + t[end:]
	}
	if nl := strings.Index(t, "\n"); nl >= 0 {
		// Subtle: a wrapped echo carries the shell's own padding, carriage
		// returns and escapes, so the match ignores all of them.
		first, want := terminalNoise.ReplaceAllString(t[:nl], ""), terminalNoise.ReplaceAllString(line, "")
		if want != "" && strings.Contains(first, want) {
			t = t[nl+1:]
		}
	}
	return t
}

// Close asks the shell to exit and waits up to 5 s for its exit frame.
func (l *liveSession) Close(ctx context.Context) {
	defer func() { _ = l.s.Close() }()
	if !l.exited {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		defer l.bindDeadline(ctx)()
		if l.send("exit\n") == nil {
			for !l.exited {
				if _, err := l.next(); err != nil {
					break
				}
			}
		}
	}
}
