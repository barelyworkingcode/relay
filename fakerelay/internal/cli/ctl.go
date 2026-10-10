package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ctl drives the control socket: the fake-only commands tests use.
func (e *env) ctl(args []string) int {
	if len(args) == 0 {
		return e.fail(2, "usage: fakerelay ctl state|presence|fault|host|fs-event|clock ...")
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
	case "clock":
		return e.clock(rest)
	}
	return e.fail(2, "unknown ctl command %q; see docs/fakerelay.md", verb)
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
		return e.notRunning("ctl")
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
