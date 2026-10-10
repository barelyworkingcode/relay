// Command fakeagent stands in for the claude, pi and codex CLIs. The persona
// is the basename of argv[0], so one binary hard-linked under three names
// serves all three. Each persona speaks the minimum protocol relay-sessions
// needs to start a session and complete a turn, and every turn answers
// "echo: <user text>".
//
// The call log is $TMPDIR/fakeagent-<persona>.jsonl: a start line (argv,
// cwd), one input line per stdin line (line_sha256, and text when the line is
// a user turn) and an exit line.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"relaye2e/fakes/calllog"
)

// agentLine is one call-log line.
type agentLine struct {
	TS         string   `json:"ts"`
	Persona    string   `json:"persona"`
	Event      string   `json:"event"`
	Argv       []string `json:"argv,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Text       string   `json:"text,omitempty"`
	LineSHA256 string   `json:"line_sha256,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
}

// agent is the state every persona shares.
type agent struct {
	persona string
	args    []string
	log     *calllog.Writer
	out     io.Writer
}

func main() { os.Exit(run()) }

func run() int {
	persona := filepath.Base(os.Args[0])
	args := os.Args[1:]
	switch persona {
	case "claude", "pi", "codex":
	default:
		fmt.Fprintf(os.Stderr, "fakeagent: persona %q is not claude, pi or codex\n", persona)
		return 2
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("fakeagent 1.0")
		return 0
	}

	a := &agent{persona: persona, args: args, out: os.Stdout}
	logPath := filepath.Join(os.TempDir(), "fakeagent-"+persona+".jsonl")
	l, err := calllog.Open(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagent:", err)
		return 1
	}
	a.log = l
	cwd, _ := os.Getwd()
	a.record(agentLine{Event: "start", Argv: args, Cwd: cwd})

	code := a.dispatch()
	a.record(agentLine{Event: "exit", ExitCode: &code})
	return code
}

func (a *agent) dispatch() int {
	switch a.persona {
	case "claude":
		return a.runClaude()
	case "pi":
		return a.runPi()
	default:
		return a.runCodex()
	}
}

func (a *agent) record(l agentLine) {
	l.TS = time.Now().UTC().Format(time.RFC3339Nano)
	l.Persona = a.persona
	_ = a.log.Append(l)
}

// emit writes one stdout line with a single write.
func (a *agent) emit(line []byte) {
	_, _ = a.out.Write(append(line, '\n'))
}

// stdinLines feeds each non-empty stdin line to fn. It returns when stdin
// closes; a stop signal ends the process after the exit line is logged. Both
// end a session the way relay ends one.
func (a *agent) stdinLines(fn func(line []byte)) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		code := 0
		a.record(agentLine{Event: "exit", ExitCode: &code})
		os.Exit(0)
	}()

	br := bufio.NewReader(os.Stdin)
	for {
		line, err := br.ReadBytes('\n')
		if l := trimSpace(line); len(l) > 0 {
			fn(l)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "fakeagent: stdin:", err)
			}
			return
		}
	}
}

// logInput records one stdin line. A persona calls it before it replies, so a
// reader that has seen the reply has seen the line.
func (a *agent) logInput(line []byte, userText *string) {
	entry := agentLine{Event: "input", LineSHA256: calllog.SHA256Hex(line)}
	if userText != nil {
		entry.Text = *userText
	}
	a.record(entry)
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// flagValue returns the argument after name, or "".
func flagValue(args []string, name string) string {
	for i, x := range args {
		if x == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
