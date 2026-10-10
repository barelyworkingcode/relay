// Package cli is the command line: global flags, the client verbs and the
// server-side service verbs.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
)

// Global holds the flags every verb takes.
type Global struct {
	ConfigDir string
	Trace     string
}

// ExitError carries an exit code and the message to print.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// ParseGlobal strips --config-dir and --trace from args up to a bare "--" and
// resolves the config dir from the flag or RELAY_CONFIG_DIR.
func ParseGlobal(args []string, getenv func(string) string) (Global, []string, error) {
	var g Global
	var rest []string
	seenDir, seenTrace := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		for _, name := range []string{"config-dir", "trace"} {
			var val string
			switch {
			case a == "--"+name:
				if i+1 >= len(args) {
					return g, nil, flagErr(name, "")
				}
				i++
				val = args[i]
			case strings.HasPrefix(a, "--"+name+"="):
				val = a[len(name)+3:]
			default:
				continue
			}
			if val == "" || strings.HasPrefix(val, "-") {
				return g, nil, flagErr(name, val)
			}
			if name == "config-dir" {
				if seenDir {
					return g, nil, &ExitError{1, "--config-dir given more than once"}
				}
				seenDir = true
				abs, err := filepath.Abs(val)
				if err != nil {
					return g, nil, &ExitError{1, err.Error()}
				}
				g.ConfigDir = abs
			} else {
				if seenTrace {
					return g, nil, &ExitError{1, "--trace given more than once"}
				}
				seenTrace = true
				if !events.ValidTrace(val) {
					return g, nil, flagErr(name, val)
				}
				g.Trace = val
			}
			a = ""
			break
		}
		if a != "" {
			rest = append(rest, a)
		}
	}
	if g.ConfigDir == "" {
		if v := getenv("RELAY_CONFIG_DIR"); v != "" {
			if !filepath.IsAbs(v) {
				return g, nil, &ExitError{1, fmt.Sprintf("RELAY_CONFIG_DIR must be an absolute path, got %q", v)}
			}
			g.ConfigDir = v
		}
	}
	return g, rest, nil
}

func flagErr(name, val string) error {
	if name == "trace" && val != "" && !strings.HasPrefix(val, "-") {
		return &ExitError{1, "--trace needs a trace ID of 8 to 64 characters from A-Z a-z 0-9 _ -"}
	}
	if name == "trace" {
		return &ExitError{1, "--trace needs a trace ID of 8 to 64 characters from A-Z a-z 0-9 _ -"}
	}
	return &ExitError{1, "--config-dir needs a directory path"}
}

// Run executes a client verb. rest[0] is the verb; serve is main's.
func Run(g Global, rest []string, out, errw io.Writer) int {
	if len(rest) == 0 {
		fmt.Fprintln(errw, "usage: fakerelay --config-dir DIR serve | logs | audit | ctl | grant | project | eve | service | mcp")
		return 2
	}
	if g.ConfigDir == "" {
		fmt.Fprintln(errw, "error: no config dir; pass --config-dir DIR or set RELAY_CONFIG_DIR")
		return 2
	}
	e := &env{g: g, out: out, err: errw}
	switch rest[0] {
	case "logs":
		return e.logs(rest[1:])
	case "audit":
		return e.audit(rest[1:])
	case "ctl":
		return e.ctl(rest[1:])
	case "grant", "project", "eve", "service", "mcp":
		return e.forward(rest)
	}
	fmt.Fprintf(errw, "error: fakerelay does not implement `%s`; see docs/fakerelay.md\n", rest[0])
	return 2
}

type env struct {
	g        Global
	out, err io.Writer
}

func (e *env) fail(code int, format string, a ...any) int {
	fmt.Fprintf(e.err, "error: "+format+"\n", a...)
	return code
}

// parse reports an undefined flag the way relay does when a flag is not
// supported here.
func (e *env) parse(fs *flag.FlagSet, args []string) (int, bool) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	if err == nil {
		return 0, true
	}
	if n, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: "); ok {
		return e.fail(2, "fakerelay does not support --%s", strings.TrimLeft(n, "-")), false
	}
	return e.fail(2, "%v", err), false
}

var errNotRunning = errors.New("not running")

func (e *env) sock() string { return filepath.Join(e.g.ConfigDir, "fakerelay-control.sock") }

func (e *env) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 2 * time.Second}
		return d.DialContext(ctx, "unix", e.sock())
	}}}
}

// call sends one request to the control socket and returns the status and body.
func (e *env) call(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://fakerelay"+path, rd)
	resp, err := e.client().Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, nil, errNotRunning
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (e *env) notRunning(verb string) int {
	return e.fail(1, "relay is not running at %s; `fakerelay %s` requires the service.", e.g.ConfigDir, verb)
}

// forward sends a verb to the running instance and prints its answer.
func (e *env) forward(argv []string) int {
	st, b, err := e.call("POST", "/v1/verb", map[string]any{"argv": argv, "trace": e.g.Trace})
	if errors.Is(err, errNotRunning) {
		return e.notRunning(strings.Join(argv, " "))
	}
	if err != nil || st != 200 {
		return e.fail(1, "%v %s", err, bytes.TrimSpace(b))
	}
	var r struct {
		Code           int
		Stdout, Stderr string
	}
	if json.Unmarshal(b, &r) != nil {
		return e.fail(1, "unreadable answer from the instance")
	}
	io.WriteString(e.out, r.Stdout)
	io.WriteString(e.err, r.Stderr)
	return r.Code
}
