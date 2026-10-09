package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// cliVerb is one CLI command. Name is the words after `relay`. Calls names the
// bridge doors it reaches (a request type, or admin_op:<op>); nil means the verb
// runs locally and reaches no door.
type cliVerb struct {
	Name  string
	Run   func(args []string)
	Calls []string
	Usage string
}

// cliConfigDirSource is how main chose the config dir, for the `mcp` stdio
// server, which resolves its bridge socket from it.
var cliConfigDirSource configDirSource

func adminDoor(op string) string { return adminOpDoorName(op) }

// cliVerbTable is the one list of commands. runCLI dispatches from it and the
// doors document reads it, so a command cannot exist in one and not the other.
// A later task appends its verbs here.
func cliVerbTable() []cliVerb {
	return []cliVerb{
		{Name: "serve", Run: runServeCommand, Usage: serveUsage},

		{Name: "service register", Run: serviceRegister, Calls: []string{adminDoor("service.register")}},
		{Name: "service unregister", Run: serviceUnregister, Calls: []string{adminDoor("service.unregister")}},
		{Name: "service restart", Run: serviceRestart, Calls: []string{adminDoor("service.restart")}},
		{Name: "service list", Run: func(_ []string) { serviceList() }, Calls: []string{adminDoor("service.list")}},

		{Name: "mcp", Run: func(a []string) { runMcpServer(a, cliConfigDirSource) }, Calls: []string{bridge.ReqListTools, bridge.ReqCallTool}},
		{Name: "mcp register", Run: mcpRegister, Calls: []string{adminDoor("mcp.register")}},
		{Name: "mcp unregister", Run: mcpUnregister, Calls: []string{adminDoor("mcp.unregister")}},
		{Name: "mcp list", Run: func(_ []string) { mcpList() }, Calls: []string{adminDoor("mcp.list")}},
		{Name: "mcp call", Run: func(a []string) { runMcpExec("relay mcp call", a) }, Calls: []string{bridge.ReqListTools, bridge.ReqCallTool}},
		{Name: "mcpExec", Run: func(a []string) { runMcpExec("relay mcpExec", a) }, Calls: []string{bridge.ReqListTools, bridge.ReqCallTool}},

		{Name: "audit", Run: runAuditCommand},
		{Name: "logs", Run: func(a []string) { runLogsCommand(a, bridge.ClientTraceID()) }},

		{Name: "enrol create", Run: enrolCreate, Calls: []string{adminDoor("enrolment.create")}},
		{Name: "enrol sign", Run: enrolSign, Calls: []string{adminDoor("enrolment.sign")}},
		{Name: "enrol list", Run: func(_ []string) { enrolList() }, Calls: []string{adminDoor("enrolment.list")}},
		{Name: "enrol update", Run: enrolUpdate, Calls: []string{adminDoor("enrolment.update")}},
		{Name: "enrol revoke", Run: enrolRevoke, Calls: []string{adminDoor("enrolment.revoke")}},
		{Name: "enrol requests", Run: enrolRequests, Calls: []string{adminDoor("enrolment.request.list")}},
		{Name: "enrol approve", Run: enrolApprove, Calls: []string{adminDoor("enrolment.request.approve")}},
		{Name: "enrol refuse", Run: enrolRefuse, Calls: []string{adminDoor("enrolment.request.refuse")}},
		{Name: "enrol ca-fingerprint", Run: func(_ []string) { enrolCAFingerprint() }},

		{Name: "credential mint", Run: credentialMint, Calls: []string{adminDoor("credential.mint")}},
		{Name: "credential list", Run: credentialList, Calls: []string{adminDoor("credential.list")}},
		{Name: "credential revoke", Run: credentialRevoke, Calls: []string{adminDoor("credential.revoke")}},

		{Name: "login enrol", Run: func(_ []string) { loginEnrol() }, Calls: []string{adminDoor("login.bootstrap.mint")}},
		{Name: "login list", Run: func(_ []string) { loginList() }, Calls: []string{adminDoor("login.list")}},
		{Name: "login revoke", Run: loginRevoke, Calls: []string{adminDoor("login.passkey.revoke")}},

		{Name: "eve enrol", Run: func(_ []string) { eveEnrol() }, Calls: []string{adminDoor("eve.enrolment.open")}},
		{Name: "eve list", Run: func(_ []string) { eveList() }, Calls: []string{adminDoor("eve.list")}},
		{Name: "eve revoke", Run: eveRevoke, Calls: []string{adminDoor("eve.passkey.revoke")}},

		{Name: "grant", Run: runGrantCommand, Calls: []string{adminDoor("grant.view")}},
		{Name: "project update", Run: projectUpdate, Calls: []string{adminDoor("project.update")}},

		{Name: "sandbox", Run: runSandboxCommand, Calls: []string{bridge.ReqSandboxAttach}},
		{Name: "drop-in", Run: runDropInCommand, Calls: []string{bridge.ReqDropInAttach}},

		{Name: "doors", Run: runDoorsCommand, Calls: []string{adminDoor("doors.list")}, Usage: doorsUsage},
	}
}

// runCLI runs the verb whose name is the longest prefix of args. A first word
// that heads a group of verbs but matches none prints that group's commands.
func runCLI(args []string) {
	table := cliVerbTable()
	best, bestWords := -1, 0
	for i, v := range table {
		words := strings.Fields(v.Name)
		if len(words) > len(args) || len(words) <= bestWords {
			continue
		}
		if equalWords(words, args[:len(words)]) {
			best, bestWords = i, len(words)
		}
	}
	if best >= 0 {
		table[best].Run(args[bestWords:])
		return
	}
	if group := verbsInGroup(table, args[0]); len(group) > 0 {
		if len(args) > 1 {
			fmt.Fprintf(os.Stderr, "unknown %s command: %s\n", args[0], args[1])
		}
		fmt.Fprintf(os.Stderr, "Usage: relay %s <command>\n\nCommands:\n", args[0])
		for _, v := range group {
			fmt.Fprintf(os.Stderr, "  %s\n", strings.TrimPrefix(v.Name, args[0]+" "))
		}
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\nUsage: relay [--config-dir DIR] [--trace ID] [%s]\n", args[0], strings.Join(topLevelVerbs(table), "|"))
	os.Exit(1)
}

func equalWords(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func verbsInGroup(table []cliVerb, group string) []cliVerb {
	var out []cliVerb
	for _, v := range table {
		if strings.HasPrefix(v.Name, group+" ") {
			out = append(out, v)
		}
	}
	return out
}

func topLevelVerbs(table []cliVerb) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range table {
		first, _, _ := strings.Cut(v.Name, " ")
		if !seen[first] {
			seen[first] = true
			out = append(out, first)
		}
	}
	return out
}

// adminCall is a CLI verb's one path to an operator-only or brokered op:
// requireService, one admin_op, the raw answer. A failure exits 1 with the
// error on stderr and nothing on stdout.
func adminCall(command, op string, args any) json.RawMessage {
	client := requireService(command)
	body := marshalCLIArgs(args)
	raw, err := client.AdminOp(op, body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	return raw
}

// adminStream is adminCall for an op that answers in frames. SIGINT and SIGTERM
// end the call, which tells the server this peer left.
func adminStream(command, op string, args any, onFrame func(json.RawMessage)) json.RawMessage {
	client := requireService(command)
	body := marshalCLIArgs(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	raw, err := client.AdminOpStream(ctx, op, body, onFrame)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	return raw
}

func marshalCLIArgs(args any) json.RawMessage {
	if args == nil {
		return nil
	}
	body, err := json.Marshal(args)
	if err != nil {
		exitError("%v", err)
	}
	return body
}

// maxCLIBodyBytes bounds a --file body: a route body is small, and the cap
// keeps a mistaken path (a device, a log) from filling memory.
const maxCLIBodyBytes = 8 << 20

// readBodyArg reads a --file argument: a path, or "-" for stdin.
func readBodyArg(path string) []byte {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			exitError("%v", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	body, err := io.ReadAll(io.LimitReader(r, maxCLIBodyBytes+1))
	if err != nil {
		exitError("read %s: %v", path, err)
	}
	if len(body) > maxCLIBodyBytes {
		exitError("%s is larger than %d bytes", path, maxCLIBodyBytes)
	}
	return body
}

// printJSONLine writes raw as one line of JSON on stdout.
func printJSONLine(raw json.RawMessage) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		exitError("encode response: %v", err)
	}
	fmt.Println(buf.String())
}
