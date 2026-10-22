// Command fakeagent stands in for the claude, pi and codex CLIs. The persona
// is the basename of argv[0], so one binary hard-linked under three names
// serves all three. Each persona speaks the minimum protocol relay-sessions
// needs to start a session and complete a turn, and every turn answers
// "echo: <user text>".
//
// The call log is $TMPDIR/fakeagent-<persona>.jsonl: a start line (argv,
// cwd, watched environment, full environment, and the digest of the token
// descriptor when one is named), one input line per stdin line (line_sha256, and text when the line is
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
	TS      string `json:"ts"`
	Persona string `json:"persona"`
	// Pid names the process, so a test can tell two runners of one persona apart.
	Pid        int      `json:"pid,omitempty"`
	Event      string   `json:"event"`
	Argv       []string `json:"argv,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Text       string   `json:"text,omitempty"`
	LineSHA256 string   `json:"line_sha256,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	// Env holds the watched variables the process started with. A variable
	// that is unset has no key, so "0" and unset differ.
	Env map[string]string `json:"env,omitempty"`
	// Environ is the whole environment, for a scan that no secret is in it.
	Environ []string `json:"environ,omitempty"`
	// TokenSHA256 and TokenBytes describe what the process read from the
	// descriptor CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR names; the bytes are
	// never recorded. TokenError is the failure to read it.
	TokenSHA256 string `json:"token_sha256,omitempty"`
	TokenBytes  int    `json:"token_bytes,omitempty"`
	TokenError  string `json:"token_error,omitempty"`
	// StderrKind is the file type of descriptor 2 at start: fifo (a pipe or
	// FIFO), file, tty (a character device), socket or other.
	StderrKind string `json:"stderr_kind,omitempty"`
}

// stderrProbe is the line the fake writes to stderr once, at start.
const stderrProbe = "fakeagent: stderr probe"

// stderrKind reports the file type of descriptor 2.
func stderrKind() string {
	var st syscall.Stat_t
	if err := syscall.Fstat(2, &st); err != nil {
		return "other"
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFIFO:
		return "fifo"
	case syscall.S_IFREG:
		return "file"
	case syscall.S_IFCHR:
		return "tty"
	case syscall.S_IFSOCK:
		return "socket"
	}
	return "other"
}

// watchedEnv names the variables the start line reports.
var watchedEnv = []string{"DISABLE_AUTOUPDATER", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "RELAY_SESSION_ID"}

// maxTokenRead caps what the fake reads from the token descriptor.
const maxTokenRead = 64 << 10

// readTokenFD reads the descriptor the token variable names to EOF, the way
// Claude Code does. It returns the digest and length, never the bytes.
func readTokenFD() (sum string, n int, errText string) {
	fd, ok := os.LookupEnv("CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR")
	if !ok {
		return "", 0, ""
	}
	f, err := os.Open("/dev/fd/" + fd)
	if err != nil {
		return "", 0, err.Error()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTokenRead))
	if err != nil {
		return "", 0, err.Error()
	}
	return calllog.SHA256Hex(b), len(b), ""
}

func watchedEnvValues() map[string]string {
	out := map[string]string{}
	for _, k := range watchedEnv {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
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
	sum, n, terr := readTokenFD()
	a.record(agentLine{Event: "start", Argv: args, Cwd: cwd, Env: watchedEnvValues(),
		Environ: os.Environ(), TokenSHA256: sum, TokenBytes: n, TokenError: terr, StderrKind: stderrKind()})
	// A model listing parses stderr as well as stdout, so the probe stays out of it.
	if !flagPresent(args, "--list-models") {
		fmt.Fprintln(os.Stderr, stderrProbe)
	}

	code := a.dispatch()
	a.record(agentLine{Event: "exit", ExitCode: &code})
	return code
}

func (a *agent) dispatch() int {
	switch a.persona {
	case "claude":
		// --print-env is the fake's own flag: the start line already holds the
		// environment, so the process ends at once.
		if flagPresent(a.args, "--print-env") {
			return 0
		}
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
	l.Pid = os.Getpid()
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

// flagPresent reports whether args holds name.
func flagPresent(args []string, name string) bool {
	for _, x := range args {
		if x == name {
			return true
		}
	}
	return false
}
