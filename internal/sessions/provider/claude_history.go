package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// encodeClaudeProjectDir applies Claude CLI's project-directory encoding
// (replace "/" with "-").
func encodeClaudeProjectDir(dir string) string {
	return strings.ReplaceAll(dir, "/", "-")
}

const remoteClaudeHistoryTimeout = 10 * time.Second

// runSSHCommand execs argv with a timeout and returns stdout. A func var so
// hermetic tests can stub the actual ssh invocation without a real remote
// host.
var runSSHCommand = func(argv []string, timeout time.Duration) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty ssh argv")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	return cmd.Output()
}

// ReadClaudeHistory reads conversation history from Claude CLI's JSONL
// session file. Claude persists complete conversations at
// ~/.claude/projects/<encoded-dir>/<sessionID>.jsonl. When host is non-nil,
// directory lives on that SSH host instead of the console.
func ReadClaudeHistory(directory string, host *sessionstypes.HostSpec, claudeSessionID string) ([]sessionstypes.Message, error) {
	if claudeSessionID == "" {
		return nil, fmt.Errorf("no claude session ID")
	}

	if host != nil {
		return readClaudeHistoryOverSSH(host, directory, claudeSessionID)
	}

	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("eval symlinks: %w", err)
	}

	encoded := encodeClaudeProjectDir(resolved)

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home dir: %w", err)
	}

	projectDir := filepath.Join(home, ".claude", "projects", encoded)
	jsonlPath := filepath.Join(projectDir, claudeSessionID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("open jsonl: %w", err)
	}
	defer func() { _ = f.Close() }()

	sidechainQueue := loadSidechainQueue(filepath.Join(projectDir, claudeSessionID))
	return parseClaudeHistoryJSONL(f, claudeSessionID, sidechainQueue)
}

func readClaudeHistoryOverSSH(host *sessionstypes.HostSpec, directory, claudeSessionID string) ([]sessionstypes.Message, error) {
	if len(host.SSHArgv) == 0 {
		return nil, fmt.Errorf("host %q has no ssh_argv", host.Name)
	}
	path := "~/.claude/projects/" + encodeClaudeProjectDir(directory) + "/" + claudeSessionID + ".jsonl"
	remote := sshhost.RemoteCommand("", []string{"cat", path}, nil)
	argv := append([]string{host.SSHArgv[0]}, host.SSHArgv[1:]...)
	argv = append(argv, "-T", "--", remote)

	out, err := runSSHCommand(argv, remoteClaudeHistoryTimeout)
	if err != nil {
		return nil, fmt.Errorf("ssh cat history: %w", err)
	}
	return parseClaudeHistoryJSONL(bytes.NewReader(out), claudeSessionID, nil)
}

func deleteClaudeHistoryOverSSH(host *sessionstypes.HostSpec, directory, claudeSessionID string) error {
	if len(host.SSHArgv) == 0 {
		return fmt.Errorf("host %q has no ssh_argv", host.Name)
	}
	path := "~/.claude/projects/" + encodeClaudeProjectDir(directory) + "/" + claudeSessionID + ".jsonl"
	remote := sshhost.RemoteCommand("", []string{"rm", "-f", path}, nil)
	argv := append([]string{host.SSHArgv[0]}, host.SSHArgv[1:]...)
	argv = append(argv, "-T", "--", remote)

	_, err := runSSHCommand(argv, remoteClaudeHistoryTimeout)
	return err
}

// parseClaudeHistoryJSONL parses a Claude CLI JSONL transcript from r,
// grouping assistant messages by message ID. sidechainQueue may be nil (a
// host session fetches no sub-agent transcripts).
func parseClaudeHistoryJSONL(r io.Reader, claudeSessionID string, sidechainQueue *claudeSidechainQueue) ([]sessionstypes.Message, error) {
	type jsonlEntry struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	type assistantGroup struct {
		messageID string
		timestamp string
		blocks    []json.RawMessage
	}

	var messages []sessionstypes.Message
	assistantGroups := make(map[string]*assistantGroup)
	var assistantOrder []string

	flushAssistant := func(msgID string) {
		g, ok := assistantGroups[msgID]
		if !ok || len(g.blocks) == 0 {
			return
		}

		expanded := make([]json.RawMessage, 0, len(g.blocks))
		for _, block := range g.blocks {
			expanded = append(expanded, block)
			var meta struct {
				Type string `json:"type"`
				Name string `json:"name"`
				ID   string `json:"id"`
			}
			if json.Unmarshal(block, &meta) != nil {
				continue
			}
			if meta.Type != "tool_use" {
				continue
			}
			if meta.Name != "Agent" && meta.Name != "Task" {
				continue
			}
			transcript := sidechainQueue.consumeNext()
			if transcript == nil {
				continue
			}
			expanded = append(expanded, transcript)
		}

		content, _ := json.Marshal(expanded)
		messages = append(messages, sessionstypes.Message{
			Timestamp: g.timestamp,
			Role:      "assistant",
			Content:   content,
		})

		delete(assistantGroups, msgID)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		if entry.SessionID != claudeSessionID {
			continue
		}

		switch entry.Type {
		case "user":
			for _, id := range assistantOrder {
				flushAssistant(id)
			}
			assistantOrder = nil

			content := entry.Message.Content
			if len(content) == 0 {
				continue
			}

			if content[0] == '"' {
				messages = append(messages, sessionstypes.Message{
					Timestamp: entry.Timestamp,
					Role:      "user",
					Content:   content,
				})
				continue
			}

			if content[0] == '[' {
				var fullBlocks []json.RawMessage
				_ = json.Unmarshal(content, &fullBlocks)
				var textParts []string
				for _, fb := range fullBlocks {
					var meta struct {
						Type       string          `json:"type"`
						Text       string          `json:"text"`
						ToolUseID  string          `json:"tool_use_id"`
						ResultBody json.RawMessage `json:"content"`
					}
					if json.Unmarshal(fb, &meta) != nil {
						continue
					}
					switch meta.Type {
					case "text":
						textParts = append(textParts, meta.Text)
					case "tool_result":
						resultContent := meta.ResultBody
						if len(resultContent) == 0 {
							resultContent = json.RawMessage(`""`)
						}
						messages = append(messages, sessionstypes.Message{
							Timestamp: entry.Timestamp,
							Role:      "tool",
							Content:   resultContent,
							ToolUseID: meta.ToolUseID,
						})
					}
				}
				if len(textParts) > 0 {
					combined := strings.Join(textParts, "\n")
					contentJSON, _ := json.Marshal(combined)
					messages = append(messages, sessionstypes.Message{
						Timestamp: entry.Timestamp,
						Role:      "user",
						Content:   contentJSON,
					})
				}
			}

		case "assistant":
			msgID := entry.Message.ID
			if msgID == "" {
				continue
			}

			g, exists := assistantGroups[msgID]
			if !exists {
				g = &assistantGroup{
					messageID: msgID,
					timestamp: entry.Timestamp,
				}
				assistantGroups[msgID] = g
				assistantOrder = append(assistantOrder, msgID)
			}

			var blocks []json.RawMessage
			if json.Unmarshal(entry.Message.Content, &blocks) == nil {
				g.blocks = append(g.blocks, blocks...)
			}
		}
	}

	for _, id := range assistantOrder {
		flushAssistant(id)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error: %w", err)
	}

	return messages, nil
}

// claudeSidechainQueue holds parsed sub-agent transcripts in mtime order.
type claudeSidechainQueue struct {
	transcripts []json.RawMessage
}

func (q *claudeSidechainQueue) consumeNext() json.RawMessage {
	if q == nil || len(q.transcripts) == 0 {
		return nil
	}
	next := q.transcripts[0]
	q.transcripts = q.transcripts[1:]
	return next
}

// loadSidechainQueue reads <sessionDir>/subagents/agent-*.jsonl files in
// mtime order and builds an agent_transcript block per file.
func loadSidechainQueue(sessionDir string) *claudeSidechainQueue {
	q := &claudeSidechainQueue{}
	dir := filepath.Join(sessionDir, "subagents")
	files, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil || len(files) == 0 {
		return q
	}
	type fileMeta struct {
		path  string
		mtime int64
	}
	metas := make([]fileMeta, 0, len(files))
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		metas = append(metas, fileMeta{path: p, mtime: st.ModTime().UnixNano()})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].mtime < metas[j].mtime })

	for _, m := range metas {
		blocks, agentID, persona := parseSidechainFile(m.path)
		if len(blocks) == 0 {
			continue
		}
		transcript := map[string]any{
			"type":     "agent_transcript",
			"agentId":  agentID,
			"persona":  persona,
			"messages": blocks,
		}
		raw, err := json.Marshal(transcript)
		if err != nil {
			continue
		}
		q.transcripts = append(q.transcripts, raw)
	}
	return q
}

func parseSidechainFile(path string) (messages []map[string]any, agentID, persona string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	type entry struct {
		Type             string `json:"type"`
		AgentID          string `json:"agentId"`
		AttributionAgent string `json:"attributionAgent"`
		IsSidechain      bool   `json:"isSidechain"`
		Message          struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if !e.IsSidechain {
			continue
		}
		if agentID == "" && e.AgentID != "" {
			agentID = e.AgentID
		}
		if persona == "" && e.AttributionAgent != "" {
			persona = e.AttributionAgent
		}
		if e.Message.Role == "" {
			continue
		}
		messages = append(messages, map[string]any{
			"role":    e.Message.Role,
			"content": e.Message.Content,
		})
	}
	return messages, agentID, persona
}
