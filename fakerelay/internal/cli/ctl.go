package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ctl drives the control socket: the fake-only commands tests use.
func (e *env) ctl(args []string) int {
	if len(args) == 0 {
		return e.fail(2, "usage: fakerelay ctl state|presence|fault|host|fs-event|fs-write|watch-error|clock ...")
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "state":
		fs := flag.NewFlagSet("ctl state", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		if code, ok := e.parse(fs, rest); !ok {
			return code
		}
		return e.show("GET", "/v1/state", nil, !*asJSON)
	case "presence":
		script := map[string]string{}
		for _, a := range rest {
			op, out, ok := strings.Cut(a, "=")
			if !ok {
				return e.fail(2, "presence takes OP=OUTCOME arguments, got %q", a)
			}
			script[op] = out
		}
		return e.show("PUT", "/v1/presence", script, false)
	case "fault":
		return e.fault(rest)
	case "host":
		if len(rest) == 0 || rest[0] != "status" {
			return e.fail(2, "usage: fakerelay ctl host status --id ID --status S [--error TEXT]")
		}
		fs := flag.NewFlagSet("ctl host status", flag.ContinueOnError)
		id, st, msg := fs.String("id", "", ""), fs.String("status", "", ""), fs.String("error", "", "")
		if code, ok := e.parse(fs, rest[1:]); !ok {
			return code
		}
		if *id == "" || *st == "" {
			return e.fail(2, "--id and --status are required")
		}
		return e.show("PUT", "/v1/hosts/"+url.PathEscape(*id)+"/status", map[string]string{"status": *st, "error": *msg}, false)
	case "fs-event":
		fs := flag.NewFlagSet("ctl fs-event", flag.ContinueOnError)
		pr, path, kind := fs.String("project", "", ""), fs.String("path", "", ""), fs.String("kind", "", "")
		if code, ok := e.parse(fs, rest); !ok {
			return code
		}
		if *pr == "" || *path == "" {
			return e.fail(2, "--project and --path are required")
		}
		body := map[string]string{"path": *path}
		if *kind != "" {
			body["kind"] = *kind
		}
		return e.show("POST", "/v1/projects/"+url.PathEscape(*pr)+"/fs-events", body, false)
	case "fs-write":
		return e.fsWrite(rest)
	case "watch-error":
		fs := flag.NewFlagSet("ctl watch-error", flag.ContinueOnError)
		pr, code, msg := fs.String("project", "", ""), fs.String("code", "", ""), fs.String("error", "", "")
		if c, ok := e.parse(fs, rest); !ok {
			return c
		}
		if *pr == "" {
			return e.fail(2, "--project is required")
		}
		body := map[string]string{}
		if *code != "" {
			body["code"] = *code
		}
		if *msg != "" {
			body["error"] = *msg
		}
		return e.show("POST", "/v1/projects/"+url.PathEscape(*pr)+"/watch-error", body, false)
	case "clock":
		return e.clock(rest)
	}
	return e.fail(2, "unknown ctl command %q; see docs/fakerelay.md", verb)
}

// maxWriteFile is the largest --file the CLI sends; the route enforces the
// same cap on the decoded bytes.
const maxWriteFile = 10 << 20

func (e *env) fsWrite(args []string) int {
	fs := flag.NewFlagSet("ctl fs-write", flag.ContinueOnError)
	pr, path, content, file := fs.String("project", "", ""), fs.String("path", "", ""), fs.String("content", "", ""), fs.String("file", "", "")
	if code, ok := e.parse(fs, args); !ok {
		return code
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["content"] == given["file"] {
		return e.fail(2, "pass exactly one of --content and --file")
	}
	if *pr == "" || *path == "" {
		return e.fail(2, "--project and --path are required")
	}
	body := map[string]string{"path": *path, "content": *content}
	if given["file"] {
		var r io.Reader = os.Stdin
		if *file != "-" {
			f, err := os.Open(*file)
			if err != nil {
				return e.fail(1, "%v", err)
			}
			defer f.Close()
			r = f
		}
		b, err := io.ReadAll(io.LimitReader(r, maxWriteFile+1))
		if err != nil {
			return e.fail(1, "read %s: %v", *file, err)
		}
		if len(b) > maxWriteFile {
			return e.fail(1, "%s is larger than %d bytes", *file, maxWriteFile)
		}
		body["content"], body["encoding"] = base64.StdEncoding.EncodeToString(b), "base64"
	}
	return e.show("POST", "/v1/projects/"+url.PathEscape(*pr)+"/fs-write", body, false)
}

func (e *env) fault(args []string) int {
	if len(args) == 0 {
		return e.fail(2, "usage: fakerelay ctl fault add|clear|release")
	}
	fs := flag.NewFlagSet("ctl fault "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "add":
		route, mode, name, body := fs.String("route", "", ""), fs.String("mode", "", ""), fs.String("name", "", ""), fs.String("body", "", "")
		times, delay, status := fs.Int("times", 0, ""), fs.Int("delay-ms", 0, ""), fs.Int("status", 0, "")
		if code, ok := e.parse(fs, args[1:]); !ok {
			return code
		}
		f := map[string]any{"route": *route, "mode": *mode}
		for k, v := range map[string]int{"times": *times, "delay_ms": *delay, "status": *status} {
			if v != 0 {
				f[k] = v
			}
		}
		if *name != "" {
			f["name"] = *name
		}
		if *body != "" {
			if !json.Valid([]byte(*body)) {
				f["body"] = *body
			} else {
				f["body"] = json.RawMessage(*body)
			}
		}
		return e.show("POST", "/v1/faults", f, false)
	case "clear", "release":
		id := fs.String("id", "", "")
		if code, ok := e.parse(fs, args[1:]); !ok {
			return code
		}
		switch {
		case args[0] == "release" && *id == "":
			return e.fail(2, "--id is required")
		case args[0] == "release":
			return e.show("POST", "/v1/faults/"+url.PathEscape(*id)+"/release", nil, false)
		case *id == "":
			return e.show("DELETE", "/v1/faults", nil, false)
		}
		return e.show("DELETE", "/v1/faults/"+url.PathEscape(*id), nil, false)
	}
	return e.fail(2, "unknown ctl fault command %q", args[0])
}

func (e *env) clock(args []string) int {
	if len(args) == 0 {
		return e.fail(2, "usage: fakerelay ctl clock show|set TIME|advance MS")
	}
	switch args[0] {
	case "show":
		return e.show("GET", "/v1/clock", nil, false)
	case "set":
		if len(args) != 2 {
			return e.fail(2, "usage: fakerelay ctl clock set RFC3339-TIME")
		}
		return e.show("POST", "/v1/clock", map[string]string{"set": args[1]}, false)
	case "advance":
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.Join(args[1:], ""), "ms"), 10, 64)
		if err != nil {
			return e.fail(2, "usage: fakerelay ctl clock advance MILLISECONDS")
		}
		return e.show("POST", "/v1/clock", map[string]int64{"advance_ms": n}, false)
	}
	return e.fail(2, "unknown ctl clock command %q", args[0])
}

// show makes one control call and prints the body; a non-2xx status is exit 1.
func (e *env) show(method, path string, body any, indent bool) int {
	st, b, err := e.call(method, path, body)
	if errors.Is(err, errNotRunning) {
		return e.fail(1, "fakerelay is not running at %s; `fakerelay ctl` requires the service.", e.g.ConfigDir)
	}
	if err != nil {
		return e.fail(1, "%v", err)
	}
	b = bytes.TrimSpace(b)
	if st/100 != 2 {
		return e.fail(1, "%s (HTTP %d)", b, st)
	}
	if indent {
		var buf bytes.Buffer
		if json.Indent(&buf, b, "", "  ") == nil {
			b = buf.Bytes()
		}
	}
	if len(b) > 0 {
		fmt.Fprintln(e.out, string(b))
	}
	return 0
}
