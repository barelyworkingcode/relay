package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

const sandboxUsage = "usage: relay sandbox <template> [--project <name-or-id>]"

// runSandboxCommand launches <template> in the project holding the current
// directory and attaches this terminal to it. The exit status is the tool's.
func runSandboxCommand(args []string) {
	os.Exit(sandboxMain(args))
}

func sandboxFail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	return 1
}

func sandboxMain(args []string) int {
	fs := flag.NewFlagSet("sandbox", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	project := fs.String("project", "", "project name or id, when the directory is inside more than one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	// Flags may follow the template: `relay sandbox claude-code --project x`.
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			return sandboxFail("unexpected argument %q\n%s", fs.Arg(0), sandboxUsage)
		}
		rest = rest[:1]
	}

	// A courtesy only: relay refuses the same request server-side, from the
	// kernel's account of who is calling, which this variable cannot forge or
	// hide.
	if os.Getenv("RELAY_SESSION_ID") != "" {
		return sandboxFail("relay sandbox cannot run inside a relay session; run it from your own terminal")
	}
	if !isTerminal(int(os.Stdin.Fd())) || !isTerminal(int(os.Stdout.Fd())) {
		return sandboxFail("relay sandbox needs an interactive terminal on stdin and stdout")
	}
	if !serviceReachable() {
		return sandboxFail("relay is not running; `relay sandbox` needs the Relay app. Start Relay and retry.")
	}
	if len(rest) == 0 || rest[0] == "" {
		return sandboxFail("a terminal template is required\n%s", sandboxUsage)
	}
	template := rest[0]

	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		return sandboxFail("cannot resolve the current directory: %v", err)
	}

	cols, rows := terminalSize(int(os.Stdout.Fd()))
	conn, err := net.Dial("unix", bridge.SocketPath())
	if err != nil {
		return sandboxFail("relay is not running; `relay sandbox` needs the Relay app. Start Relay and retry.")
	}
	defer func() { _ = conn.Close() }()

	arg, err := json.Marshal(bridge.SandboxAttachRequest{Template: template, Cwd: cwd, Project: *project, Cols: cols, Rows: rows})
	if err != nil {
		return sandboxFail("%v", err)
	}
	if err := writeStreamLine(conn, bridge.BridgeRequest{Type: bridge.ReqSandboxAttach, Arguments: arg}); err != nil {
		return sandboxFail("could not reach relay: %v", err)
	}

	// No deadline on this read or on any later one: a launch can take as long
	// as it takes, and a machine that sleeps under an attached terminal must
	// find the stream where it left it.
	in := bufio.NewReader(conn)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return sandboxFail("relay closed the connection before answering")
	}
	var ack bridge.BridgeResponse
	if err := json.Unmarshal(line, &ack); err != nil {
		return sandboxFail("unreadable answer from relay: %v", err)
	}
	if ack.Type != bridge.RespAttached {
		return sandboxFail("%s", sandboxRefusalText(&ack))
	}

	return attachTerminal(conn, in)
}

// sandboxRefusalText prefers the structured refusal's message and falls back
// to the response's own.
func sandboxRefusalText(resp *bridge.BridgeResponse) string {
	var refusal bridge.SandboxRefusal
	if len(resp.Data) > 0 && json.Unmarshal(resp.Data, &refusal) == nil && refusal.Message != "" {
		return refusal.Message
	}
	if resp.Message != "" {
		return resp.Message
	}
	return "relay refused the request"
}

// attachTerminal owns the terminal from raw mode to restoration. Every way out
// of this function, and the signal handler, restores it first.
func attachTerminal(conn net.Conn, in *bufio.Reader) int {
	stdinFD := int(os.Stdin.Fd())
	restore, err := makeRaw(stdinFD)
	if err != nil {
		return sandboxFail("cannot put the terminal in raw mode: %v", err)
	}
	var restoreOnce sync.Once
	restoreTerminal := func() { restoreOnce.Do(restore) }
	defer restoreTerminal()

	var wmu sync.Mutex
	send := func(f bridge.StreamFrame) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeStreamLine(conn, f)
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	go func() {
		for sig := range sigs {
			s, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			if s == syscall.SIGWINCH {
				if cols, rows := terminalSize(int(os.Stdout.Fd())); cols > 0 {
					_ = send(bridge.StreamFrame{Type: bridge.StreamResize, Cols: cols, Rows: rows})
				}
				continue
			}
			// Closing the connection is what tells relay to end the session.
			restoreTerminal()
			_ = conn.Close()
			os.Exit(128 + int(s))
		}
	}()

	// The size may have changed while the launch was in flight.
	if cols, rows := terminalSize(int(os.Stdout.Fd())); cols > 0 {
		_ = send(bridge.StreamFrame{Type: bridge.StreamResize, Cols: cols, Rows: rows})
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if send(bridge.StreamFrame{Type: bridge.StreamInput, Data: buf[:n]}) != nil {
					return
				}
			}
			if err != nil {
				// The terminal is gone. Closing tells relay to end the session.
				_ = conn.Close()
				return
			}
		}
	}()

	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			restoreTerminal()
			if !errors.Is(err, io.EOF) {
				return sandboxFail("connection to relay lost: %v", err)
			}
			return sandboxFail("the session ended (connection dropped)")
		}
		var f bridge.StreamFrame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		switch f.Type {
		case bridge.StreamOutput:
			if _, err := os.Stdout.Write(f.Data); err != nil {
				return 1
			}
		case bridge.StreamExit:
			return exitStatusFor(f.Code)
		}
	}
}

// exitStatusFor passes through a status a shell could have produced. Anything
// else, such as the -1 a signalled shim reports, has no faithful shell form.
func exitStatusFor(code int) int {
	if code < 0 || code > 255 {
		return 1
	}
	return code
}

func writeStreamLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	return err == nil
}

// terminalSize returns 0, 0 when fd has no size to report.
func terminalSize(fd int) (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// makeRaw switches fd to raw mode and returns the function that undoes it.
func makeRaw(fd int) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, old) }, nil
}
