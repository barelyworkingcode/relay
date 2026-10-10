package fakes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// NewAgentFactory builds the in-process agents. The model's reply decides the
// persona: echo, text, permission, plan or fail. Each agent records its calls
// to the call log as agent-<persona>. A plan agent writes its plan under
// home/.claude/plans.
func NewAgentFactory(log *CallLog, home string) AgentFactory {
	return func(m world.Model, info SessionInfo) (Agent, error) {
		kind := m.Reply.Kind
		if kind == "" {
			kind = "echo"
		}
		return &scripted{reply: m.Reply, kind: kind, info: info, log: log, home: home, waits: map[string]chan bool{}}, nil
	}
}

type scripted struct {
	reply world.Reply
	kind  string
	info  SessionInfo
	log   *CallLog
	home  string

	inited atomic.Bool

	mu    sync.Mutex
	waits map[string]chan bool
}

func (a *scripted) name() string { return "agent-" + a.kind }

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func llm(ev map[string]any) Frame {
	ev["v"] = 2
	return Frame{"type": "llm_event", "event": ev}
}

func (a *scripted) Turn(ctx context.Context, text string, emit func(Frame)) error {
	a.log.Record(a.name(), "turn", map[string]any{"session_id": a.info.ID, "text": text})
	var planPath string
	if a.kind == "plan" {
		var err error
		if planPath, err = writePlan(filepath.Join(a.home, ".claude", "plans", a.info.ID+".md"), a.reply.Text); err != nil {
			return &TurnError{Status: 500}
		}
	}
	if a.inited.CompareAndSwap(false, true) {
		emit(llm(map[string]any{"type": "system", "subtype": "init", "model": a.info.Model, "cwd": a.info.Directory,
			"tools": []string{}, "mcp_servers": []string{}}))
	}
	msg := map[string]any{"id": "msg_" + randID(), "role": "assistant", "content": []any{}}
	if a.kind == "fail" {
		emit(llm(map[string]any{"type": "assistant", "message": msg, "error": "api_error", "apiErrorStatus": 529}))
		return &TurnError{Status: 529}
	}
	emit(llm(map[string]any{"type": "assistant", "message": msg}))
	if a.kind == "plan" {
		return a.planTurn(ctx, emit, planPath)
	}
	index := 0
	answer := "echo: " + text
	if a.kind == "text" {
		answer = a.reply.Text
	}
	if a.kind == "permission" {
		ok, err := a.askPermission(ctx, emit, &index)
		if err != nil {
			return err
		}
		if !ok {
			answer = "denied"
		}
	}
	emit(llm(map[string]any{"type": "assistant", "index": index, "content_block": map[string]any{"type": "text"}}))
	emit(llm(map[string]any{"type": "assistant", "index": index, "delta": map[string]any{"type": "text_delta", "text": answer}}))
	emit(llm(map[string]any{"type": "assistant", "index": index, "content_block_stop": true}))
	return ctx.Err()
}

// writePlan writes the plan and returns its resolved path. The path is
// resolved because a client compares it with the real path of the file.
func writePlan(path, text string) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	path = filepath.Join(real, filepath.Base(path))
	return path, os.WriteFile(path, []byte(text), 0o600)
}

// planTurn replays what a plan-mode turn shows a client: the plan file
// written, then ExitPlanMode. It has no text block.
func (a *scripted) planTurn(ctx context.Context, emit func(Frame), path string) error {
	text := a.reply.Text
	writeID, exitID := "tu"+randID(), "tu"+randID()
	write := map[string]any{"type": "tool_use", "id": writeID, "name": "Write", "input": map[string]any{"file_path": path, "content": text}}
	emit(llm(map[string]any{"type": "assistant", "index": 0, "content_block": write}))
	emit(llm(map[string]any{"type": "assistant", "index": 0, "content_block_stop": true, "content_block": write}))
	emit(llm(map[string]any{"type": "result", "subtype": "tool_result", "tool_use_id": writeID, "tool_name": "Write",
		"content": "ok", "is_error": false}))
	exit := map[string]any{"type": "tool_use", "id": exitID, "name": "ExitPlanMode", "input": map[string]any{"plan": text}}
	emit(llm(map[string]any{"type": "assistant", "index": 1, "content_block": exit}))
	emit(llm(map[string]any{"type": "assistant", "index": 1, "content_block_stop": true, "content_block": exit}))
	return ctx.Err()
}

// askPermission raises a permission_request for the scripted tool and waits
// for Answer, or for the turn to be stopped.
func (a *scripted) askPermission(ctx context.Context, emit func(Frame), index *int) (bool, error) {
	tool := a.reply.Tool
	if tool == "" {
		tool = "Bash"
	}
	useID, permID := "tu"+randID(), "perm"+randID()
	ch := make(chan bool, 1)
	a.mu.Lock()
	a.waits[permID] = ch
	a.mu.Unlock()
	use := map[string]any{"type": "tool_use", "id": useID, "name": tool, "input": map[string]any{}}
	emit(llm(map[string]any{"type": "assistant", "index": *index, "content_block": use}))
	emit(llm(map[string]any{"type": "system", "subtype": "permission_request", "permission_id": permID,
		"tool_name": tool, "tool_use_id": useID, "tool_input": map[string]any{}}))
	emit(Frame{"type": "permission_request", "permissionId": permID, "toolName": tool, "toolInput": "{}", "toolUseId": useID})
	var approved bool
	select {
	case approved = <-ch:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	emit(llm(map[string]any{"type": "assistant", "index": *index, "content_block_stop": true, "content_block": use}))
	content := "ok"
	if !approved {
		content = "Denied by user"
	}
	emit(llm(map[string]any{"type": "result", "subtype": "tool_result", "tool_use_id": useID, "tool_name": tool,
		"content": content, "is_error": !approved}))
	*index++
	return approved, nil
}

func (a *scripted) Answer(permissionID string, approved bool) {
	a.log.Record(a.name(), "answer", map[string]any{"session_id": a.info.ID, "permission_id": permissionID, "approved": approved})
	a.mu.Lock()
	ch := a.waits[permissionID]
	delete(a.waits, permissionID)
	a.mu.Unlock()
	if ch != nil {
		ch <- approved
	}
}
