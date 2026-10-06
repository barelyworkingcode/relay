// testcodex stands in for the codex CLI in tests. `debug models` prints
// TESTCODEX_MODELS. `app-server` speaks newline-delimited JSON-RPC and
// replays the recording named by TESTCODEX_FIXTURE, one turn per turn/start.
//
// Optional environment:
//
//	TESTCODEX_INJECT   a line written right after turn/started
//	TESTCODEX_REQUEST  a server request written after turn/started; the turn
//	                   waits for relay's reply to it before continuing
//	TESTCODEX_HANG     never write turn/completed
//	TESTCODEX_DIE      kill this process (SIGKILL) after turn/started
//	TESTCODEX_OUT      file that receives one JSON object describing this
//	                   start (argv, env), then every line relay sent
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) >= 2 && args[0] == "debug" && args[1] == "models":
		fmt.Print(os.Getenv("TESTCODEX_MODELS"))
	case len(args) >= 1 && args[0] == "app-server":
		appServer()
	default:
		fmt.Fprintf(os.Stderr, "testcodex: unsupported args %q\n", args)
		os.Exit(2)
	}
}

var out *os.File

func record(v any) {
	if out == nil {
		return
	}
	b, _ := json.Marshal(v)
	_, _ = out.Write(append(b, '\n'))
}

func emit(line string) {
	_, _ = os.Stdout.WriteString(line + "\n")
}

// withID rewrites a recorded reply to answer the request relay actually sent.
func withID(line string, id json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &m) != nil {
		return line
	}
	m["id"] = id
	b, _ := json.Marshal(m)
	return string(b)
}

func isTurnReply(line string) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Turn json.RawMessage `json:"turn"`
		} `json:"result"`
	}
	return json.Unmarshal([]byte(line), &m) == nil && len(m.ID) > 0 && len(m.Result.Turn) > 0
}

func methodOf(line string) string {
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(line), &m)
	return m.Method
}

func appServer() {
	if p := os.Getenv("TESTCODEX_OUT"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			out = f
			wd, _ := os.Getwd()
			record(map[string]any{"testcodex": "start", "argv": os.Args, "env": os.Environ(), "cwd": wd})
		}
	}

	raw, err := os.ReadFile(os.Getenv("TESTCODEX_FIXTURE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "testcodex: read fixture: %v\n", err)
		os.Exit(2)
	}
	// head is everything before the first turn reply (the initialize and
	// thread replies, then notifications); turns[k] starts at the k-th turn reply.
	var head []string
	var turns [][]string
	for _, l := range strings.Split(string(raw), "\n") {
		if l == "" {
			continue
		}
		switch {
		case isTurnReply(l):
			turns = append(turns, []string{l})
		case len(turns) > 0:
			turns[len(turns)-1] = append(turns[len(turns)-1], l)
		default:
			head = append(head, l)
		}
	}
	if len(head) < 2 {
		fmt.Fprintln(os.Stderr, "testcodex: fixture lacks initialize/thread replies")
		os.Exit(2)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		l := sc.Text()
		record(json.RawMessage(l))
		return l, true
	}

	turn := 0
	for {
		line, ok := next()
		if !ok {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal([]byte(line), &req) != nil || len(req.ID) == 0 || req.Method == "" {
			continue
		}
		switch req.Method {
		case "initialize":
			emit(withID(head[0], req.ID))
		case "thread/start", "thread/resume":
			emit(withID(head[1], req.ID))
			for _, l := range head[2:] {
				emit(l)
			}
		case "turn/start":
			if turn >= len(turns) {
				emit(fmt.Sprintf(`{"id":%s,"error":{"code":-32000,"message":"testcodex: fixture has no turn %d"}}`, req.ID, turn))
				continue
			}
			replay(turns[turn], req.ID, next)
			turn++
		default:
			emit(fmt.Sprintf(`{"id":%s,"result":{}}`, req.ID))
		}
	}
}

func replay(lines []string, id json.RawMessage, next func() (string, bool)) {
	for i, l := range lines {
		if i == 0 {
			emit(withID(l, id))
			continue
		}
		m := methodOf(l)
		if m == "turn/completed" && os.Getenv("TESTCODEX_HANG") != "" {
			continue
		}
		emit(l)
		if m != "turn/started" {
			continue
		}
		if v := os.Getenv("TESTCODEX_INJECT"); v != "" {
			emit(v)
		}
		if v := os.Getenv("TESTCODEX_DIE"); v != "" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		if v := os.Getenv("TESTCODEX_REQUEST"); v != "" {
			emit(v)
			awaitReply(v, next)
		}
	}
}

// awaitReply blocks until relay answers the server request, so the turn
// cannot finish ahead of the answer.
func awaitReply(request string, next func() (string, bool)) {
	var rq struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(request), &rq)
	for {
		l, ok := next()
		if !ok {
			return
		}
		var r struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal([]byte(l), &r) == nil && r.Method == "" && string(r.ID) == string(rq.ID) {
			return
		}
	}
}
