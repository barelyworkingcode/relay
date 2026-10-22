package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// piModelTable is `pi --list-models`: a fixed-width table whose header sets
// the column offsets relay slices each row by.
const piModelTable = "provider  model       context\nfake      fake-echo   128K\n"

// runPi speaks pi's RPC mode: commands in, one response per command that has
// an id, and agent events for each prompt.
func (a *agent) runPi() int {
	if hasArg(a.args, "--list-models") {
		fmt.Fprint(os.Stdout, piModelTable)
		return 0
	}
	if flagValue(a.args, "--mode") != "rpc" {
		return a.runPiTUI()
	}
	sessionID := flagValue(a.args, "--session")
	sessionFile := ""
	leaf := ""
	if filepath.IsAbs(sessionID) {
		sessionFile = sessionID
		if _, err := os.Stat(sessionFile + ".fail-start"); err == nil {
			a.record(agentLine{Event: "pi_start_failed", Text: sessionFile})
			return 1
		}
		id, l, err := readPiSession(sessionFile)
		if err != nil {
			return 1
		}
		sessionID, leaf = id, l
	}
	// A session started or named by id lives in --session-dir as
	// <stamp>_<id>.jsonl, as pi's own files do, and get_state reports that file
	// as it reports one named by path. A start with no --session makes a new
	// UUID session when a directory is named.
	sessionDir := flagValue(a.args, "--session-dir")
	if sessionID == "" {
		if sessionDir != "" {
			sessionID = randomUUID()
		} else {
			sessionID = randomID("pi-")
		}
	}
	if sessionFile == "" {
		if f, err := piSessionDirFile(sessionID, sessionDir); err == nil && f != "" {
			sessionFile = f
			if _, l, err := readPiSession(f); err == nil {
				leaf = l
			}
		}
	}
	entryFile := sessionFile

	holding := false
	a.stdinLines(func(line []byte) {
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &cmd) != nil {
			a.logInput(line, nil)
			return
		}
		if cmd.Type != "prompt" {
			a.logInput(line, nil)
			if cmd.Type == "abort" {
				a.record(agentLine{Event: "pi_abort", Text: sessionID})
				if holding {
					holding = false
					a.emitJSON(map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false})
					a.emitJSON(map[string]any{"type": "agent_settled"})
				}
			}
			if cmd.ID != "" {
				resp := map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true}
				if cmd.Type == "get_state" {
					resp["data"] = map[string]any{"sessionId": sessionID}
					if sessionFile != "" {
						resp["data"].(map[string]any)["sessionFile"] = sessionFile
					}
				}
				a.emitJSON(resp)
			}
			return
		}
		a.logInput(line, &cmd.Message)
		reply := "echo: " + cmd.Message
		if cmd.ID != "" {
			a.emitJSON(map[string]any{"type": "response", "id": cmd.ID, "command": "prompt", "success": true})
		}
		a.emitJSON(map[string]any{"type": "agent_start"})
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_start", "contentIndex": 0}})
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": reply}})
		if cmd.Message == "!hold" {
			holding = true
			a.record(agentLine{Event: "pi_holding", Text: sessionID})
			return
		}
		if entryFile != "" {
			f, err := os.OpenFile(entryFile, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				panic(err)
			}
			enc := json.NewEncoder(f)
			for _, msg := range []map[string]any{
				{"role": "user", "content": cmd.Message, "timestamp": time.Now().UnixMilli()},
				{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply}}, "stopReason": "stop", "timestamp": time.Now().UnixMilli()},
			} {
				id := randomID("")
				if err := enc.Encode(map[string]any{"type": "message", "id": id, "parentId": leaf, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": msg}); err != nil {
					panic(err)
				}
				leaf = id
			}
			if err := f.Close(); err != nil {
				panic(err)
			}
		}
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0, "content": reply}})
		a.emitJSON(map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false})
		a.emitJSON(map[string]any{"type": "agent_settled"})
	})
	return 0
}

func hasArg(args []string, name string) bool {
	for _, x := range args {
		if x == name {
			return true
		}
	}
	return false
}

// readPiSession returns the session id the file's header names and the id of
// its last entry.
func readPiSession(file string) (id, leaf string, err error) {
	f, err := os.Open(file)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		var entry struct{ Type, ID string }
		if json.Unmarshal(sc.Bytes(), &entry) == nil {
			if entry.Type == "session" {
				id = entry.ID
			} else {
				leaf = entry.ID
			}
		}
	}
	return id, leaf, sc.Err()
}

// piSessionDirFile finds the session file of id in dir, or creates it with its
// header line. It returns "" when no directory was named.
func piSessionDirFile(id, dir string) (string, error) {
	if id == "" || dir == "" || !filepath.IsAbs(dir) {
		return "", nil
	}
	if found, _ := filepath.Glob(filepath.Join(dir, "*_"+id+".jsonl")); len(found) > 0 {
		return found[0], nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(now.Format("2006-01-02T15:04:05.000Z"))
	file := filepath.Join(dir, stamp+"_"+id+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": id, "timestamp": now.Format(time.RFC3339Nano)})
	return file, os.WriteFile(file, append(header, '\n'), 0o600)
}

// runPiTUI is pi without --mode rpc: the terminal UI a drop-in opens. It logs
// tui_ready (after its session file opens for append) and writes one ready line
// to the terminal; each terminal input line becomes one entry of the --session
// file; /quit or a closed terminal ends it. A file it cannot open or append to
// is logged as tui_failed, so a sandbox that withholds the file shows in the
// call log.
func (a *agent) runPiTUI() int {
	session := flagValue(a.args, "--session")
	file := session
	if !filepath.IsAbs(file) {
		var err error
		if file, err = piSessionDirFile(session, flagValue(a.args, "--session-dir")); err != nil || file == "" {
			a.record(agentLine{Event: "tui_failed", Text: "session"})
			return 2
		}
	}
	_, leaf, err := readPiSession(file)
	if err != nil {
		a.record(agentLine{Event: "tui_failed", Text: "read"})
		return 1
	}
	out, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		a.record(agentLine{Event: "tui_failed", Text: "open"})
		return 1
	}
	defer out.Close()
	a.record(agentLine{Event: "tui_ready", Text: file})
	fmt.Fprintln(os.Stdout, "fakepi: ready")

	a.stdinLines(func(line []byte) {
		text := string(line)
		if text == "/quit" {
			code := 0
			a.record(agentLine{Event: "exit", ExitCode: &code})
			os.Exit(0)
		}
		id := randomID("")
		entry := map[string]any{"type": "message", "id": id, "parentId": leaf, "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": text, "timestamp": time.Now().UnixMilli()}}
		b, _ := json.Marshal(entry)
		if _, err := out.Write(append(b, '\n')); err != nil {
			a.record(agentLine{Event: "tui_failed", Text: "write"})
			return
		}
		leaf = id
		a.logInput(line, &text)
	})
	return 0
}
