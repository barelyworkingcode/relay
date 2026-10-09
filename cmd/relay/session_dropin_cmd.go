package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

const dropInUsage = "usage: relay drop-in <session-id>"

// runDropInCommand takes a headless Claude session over in this terminal.
// Relay hosts the terminal and this process only shows it, so closing the
// window ends the terminal, which hands the session back. The exit status is
// Claude's.
func runDropInCommand(args []string) {
	os.Exit(dropInMain(args))
}

func dropInMain(args []string) int {
	// A courtesy only: relay refuses the same request server-side, from the
	// kernel's account of who is calling.
	if os.Getenv("RELAY_SESSION_ID") != "" {
		return sandboxFail("relay drop-in cannot run inside a relay session; run it from your own terminal")
	}
	if !isTerminal(int(os.Stdin.Fd())) || !isTerminal(int(os.Stdout.Fd())) {
		return sandboxFail("relay drop-in needs an interactive terminal on stdin and stdout")
	}
	if !serviceReachable() {
		return sandboxFail("relay is not running at %s; `relay drop-in` needs the Relay app. Start Relay and retry.", bridge.ConfigDir())
	}
	if len(args) == 0 || args[0] == "" {
		return sandboxFail("a session id is required\n%s", dropInUsage)
	}
	if len(args) > 1 {
		return sandboxFail("unexpected argument %q\n%s", args[1], dropInUsage)
	}
	id := args[0]

	cols, rows := terminalSize(int(os.Stdout.Fd()))
	conn, err := net.Dial("unix", bridge.SocketPath())
	if err != nil {
		return sandboxFail("relay is not running at %s; `relay drop-in` needs the Relay app. Start Relay and retry.", bridge.ConfigDir())
	}
	defer func() { _ = conn.Close() }()

	arg, err := json.Marshal(bridge.DropInAttachRequest{SessionID: id, Cols: cols, Rows: rows})
	if err != nil {
		return sandboxFail("%v", err)
	}
	fmt.Fprintf(os.Stderr, "relay: handing over %s; waiting up to 60 s for the current turn to end\n", id)
	if err := writeStreamLine(conn, bridge.BridgeRequest{Type: bridge.ReqDropInAttach, Arguments: arg}); err != nil {
		return sandboxFail("could not reach relay: %v", err)
	}

	// No deadline on this read or any later one: the handoff waits for the
	// turn on relay's side, and a machine that sleeps under an attached
	// terminal must find the stream where it left it.
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

	status := attachTerminal(conn, in)
	fmt.Fprintf(os.Stderr, "relay: handed back %s\n", id)
	return status
}
