package main

// record-macmcp takes the recording testmcp's macmcp persona serves from the
// real macMCP: its initialize answer (the context schema rides in serverInfo)
// and its tools/list. With --check it writes nothing and exits 1 naming each
// difference between the real macMCP and the recording. Only initialize and
// tools/list are sent, so the real macMCP sends no Apple Event for it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	recordReplyDeadline = 60 * time.Second
	recordingRelPath    = "cmd/devboxverify/testdata/macmcp-recording.json"
	recordingSchema     = 1
)

type macmcpRecording struct {
	Schema       int                        `json:"schema"`
	Provisional  string                     `json:"provisional,omitempty"`
	RecordedFrom string                     `json:"recorded_from"`
	Initialize   json.RawMessage            `json:"initialize"`
	ToolsList    json.RawMessage            `json:"tools_list"`
	Answers      map[string]json.RawMessage `json:"answers,omitempty"`
}

func runRecordMacmcp(args []string) int {
	fs := flag.NewFlagSet("record-macmcp", flag.ContinueOnError)
	check := fs.Bool("check", false, "compare the real macMCP with the recording; write nothing")
	mcpCmd := fs.String("mcp", "", "the real macMCP's command (default: the macmcp record relay mcp list shows)")
	recPath := fs.String("recording", "", "the recording file (default: "+recordingRelPath+" in this checkout)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: devboxverify record-macmcp [--check] [--mcp CMD] [--recording FILE]")
		return 2
	}
	if *recPath == "" {
		root, err := gitOut(".", "rev-parse", "--show-toplevel")
		if err != nil {
			fmt.Fprintln(os.Stderr, "run devboxverify from inside a relay checkout, or pass --recording")
			return 2
		}
		*recPath = filepath.Join(root, recordingRelPath)
	}
	argv := strings.Fields(*mcpCmd)
	if len(argv) == 0 {
		var err error
		if argv, err = registeredMacmcp(envOr("RELAY_BIN", "/Applications/Relay.app/Contents/MacOS/relay")); err != nil {
			fmt.Fprintln(os.Stderr, "record-macmcp:", scrub(err.Error(), home))
			return 2
		}
	}
	got, err := recordFrom(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "record-macmcp:", scrub(err.Error(), home))
		return 2
	}
	var old macmcpRecording
	raw, readErr := os.ReadFile(*recPath)
	if readErr == nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			fmt.Fprintln(os.Stderr, "record-macmcp: the recording is not valid JSON:", err)
			return 2
		}
	}
	if *check {
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "record-macmcp: cannot read the recording:", scrub(readErr.Error(), home))
			return 2
		}
		diffs, err := recordingDiff(old, got)
		if err != nil {
			fmt.Fprintln(os.Stderr, "record-macmcp:", err)
			return 2
		}
		for _, d := range diffs {
			emit("DRIFT", d)
		}
		if len(diffs) > 0 {
			emit("MACMCP", "differs", fmt.Sprintf("%d differences from the recording", len(diffs)))
			return 1
		}
		emit("MACMCP", "matches", got.RecordedFrom)
		return 0
	}
	// Deliberate: the answers are not something this command can take from
	// the real macMCP without sending mail, so a re-record keeps them.
	got.Answers = old.Answers
	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "record-macmcp:", err)
		return 2
	}
	tmp := *recPath + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "record-macmcp:", scrub(err.Error(), home))
		return 2
	}
	if err := os.Rename(tmp, *recPath); err != nil {
		fmt.Fprintln(os.Stderr, "record-macmcp:", scrub(err.Error(), home))
		return 2
	}
	emit("RECORDED", got.RecordedFrom, scrub(*recPath, home))
	return 0
}

// registeredMacmcp reads the macmcp row's ENDPOINT from relay mcp list: the
// command and its arguments, space-joined.
func registeredMacmcp(relayBin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, relayBin, "mcp", "list").Output()
	if err != nil {
		return nil, errors.New("relay mcp list failed; pass --mcp")
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "macmcp" {
			for i, x := range f {
				if x == "stdio" && i+1 < len(f) {
					return f[i+1:], nil
				}
			}
		}
	}
	return nil, errors.New("relay mcp list shows no stdio macmcp record; pass --mcp")
}

// recordFrom runs the real macMCP over stdio for initialize and every page
// of tools/list. Waits: each reply, bounded by recordReplyDeadline.
func recordFrom(argv []string) (macmcpRecording, error) {
	ctx, cancel := context.WithTimeout(context.Background(), recordReplyDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return macmcpRecording{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return macmcpRecording{}, err
	}
	if err := cmd.Start(); err != nil {
		return macmcpRecording{}, fmt.Errorf("start %s: %w", argv[0], err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	peer := &stdioPeer{in: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}

	initRes, err := peer.call("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "devboxverify", "version": "record-macmcp"}})
	if err != nil {
		return macmcpRecording{}, err
	}
	if err := peer.notify("notifications/initialized"); err != nil {
		return macmcpRecording{}, err
	}
	var tools []json.RawMessage
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page, err := peer.call("tools/list", params)
		if err != nil {
			return macmcpRecording{}, err
		}
		var p struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(page, &p); err != nil {
			return macmcpRecording{}, fmt.Errorf("tools/list answer: %w", err)
		}
		tools = append(tools, p.Tools...)
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	list, _ := json.Marshal(map[string]any{"tools": tools})
	var si struct {
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	_ = json.Unmarshal(initRes, &si)
	return macmcpRecording{Schema: recordingSchema, RecordedFrom: strings.TrimSpace(si.ServerInfo.Name + " " + si.ServerInfo.Version),
		Initialize: initRes, ToolsList: list}, nil
}

type stdioPeer struct {
	in   io.Writer
	out  *bufio.Reader
	next int
}

func (p *stdioPeer) notify(method string) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := p.in.Write(append(b, '\n'))
	return err
}

func (p *stdioPeer) call(method string, params any) (json.RawMessage, error) {
	p.next++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": method, "params": params})
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	for {
		line, err := p.out.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("%s: no answer: %w", method, err)
		}
		var resp struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &resp) != nil || resp.ID == nil || *resp.ID != p.next {
			continue // a notification, a log line, or a server request
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: error %d %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// recordingDiff names every tool added, removed or changed, every context
// schema field added, removed or changed, and a changed schema version.
// Server name and version are not compared: a release that changes neither
// tools nor schema is not drift.
func recordingDiff(want, got macmcpRecording) ([]string, error) {
	var diffs []string
	wantTools, err := toolsByName(want.ToolsList)
	if err != nil {
		return nil, fmt.Errorf("recording: %w", err)
	}
	gotTools, err := toolsByName(got.ToolsList)
	if err != nil {
		return nil, fmt.Errorf("real macMCP: %w", err)
	}
	diffs = append(diffs, mapDiff("tool", wantTools, gotTools)...)

	wantSchema, wantVer, err := contextSchemaOf(want.Initialize)
	if err != nil {
		return nil, fmt.Errorf("recording: %w", err)
	}
	gotSchema, gotVer, err := contextSchemaOf(got.Initialize)
	if err != nil {
		return nil, fmt.Errorf("real macMCP: %w", err)
	}
	if wantVer != gotVer {
		diffs = append(diffs, fmt.Sprintf("contextSchemaVersion: recording %s, real %s", wantVer, gotVer))
	}
	diffs = append(diffs, mapDiff("context field", wantSchema, gotSchema)...)
	return diffs, nil
}

func toolsByName(list json.RawMessage) (map[string]json.RawMessage, error) {
	var l struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(list, &l); err != nil {
		return nil, fmt.Errorf("tools_list: %w", err)
	}
	out := map[string]json.RawMessage{}
	for _, t := range l.Tools {
		var n struct{ Name string }
		if json.Unmarshal(t, &n) != nil || n.Name == "" {
			return nil, errors.New("tools_list holds a tool with no name")
		}
		out[n.Name] = t
	}
	return out, nil
}

func contextSchemaOf(init json.RawMessage) (map[string]json.RawMessage, string, error) {
	var i struct {
		ServerInfo struct {
			Version json.RawMessage            `json:"contextSchemaVersion"`
			Schema  map[string]json.RawMessage `json:"contextSchema"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(init, &i); err != nil {
		return nil, "", fmt.Errorf("initialize: %w", err)
	}
	ver := string(i.ServerInfo.Version)
	if ver == "" {
		ver = "absent"
	}
	return i.ServerInfo.Schema, ver, nil
}

// mapDiff compares values as decoded JSON, so key order and spacing are not
// differences; for a changed object it names the top-level keys that moved.
func mapDiff(kind string, want, got map[string]json.RawMessage) []string {
	var out []string
	for _, name := range sortedUnion(want, got) {
		w, inWant := want[name]
		g, inGot := got[name]
		switch {
		case !inGot:
			out = append(out, kind+" "+name+": in the recording, not in the real macMCP")
		case !inWant:
			out = append(out, kind+" "+name+": in the real macMCP, not in the recording")
		case canonical(w) != canonical(g):
			out = append(out, kind+" "+name+": "+changedKeys(w, g))
		}
	}
	return out
}

func sortedUnion(a, b map[string]json.RawMessage) []string {
	var names []string
	for k := range a {
		names = append(names, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	return names
}

func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func changedKeys(want, got json.RawMessage) string {
	var w, g map[string]json.RawMessage
	if json.Unmarshal(want, &w) != nil || json.Unmarshal(got, &g) != nil {
		return "differs"
	}
	var keys []string
	for _, k := range sortedUnion(w, g) {
		if canonical(w[k]) != canonical(g[k]) {
			keys = append(keys, k)
		}
	}
	return strings.Join(keys, ", ") + " differ"
}
