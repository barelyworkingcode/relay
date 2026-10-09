package fakes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// NewAgentFactory builds the in-process agents. The model's reply decides the
// persona: echo, text, permission or fail. Each agent records its calls to
// the call log as agent-<persona>.
func NewAgentFactory(log *CallLog) AgentFactory {
	return func(m world.Model, info SessionInfo) (Agent, error) {
		kind := m.Reply.Kind
		if kind == "" {
			kind = "echo"
		}
		return &scripted{reply: m.Reply, kind: kind, info: info, log: log, waits: map[string]chan bool{}}, nil
	}
}

type scripted struct {
	reply world.Reply
	kind  string
	info  SessionInfo
	log   *CallLog

	inited atomic.Bool
	msgSeq atomic.Int64

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
	if a.inited.CompareAndSwap(false, true) {
		emit(llm(map[string]any{"type": "system", "subtype": "init", "model": a.info.Model, "cwd": a.info.Directory,
			"tools": []string{}, "mcp_servers": []string{}}))
	}
	msg := map[string]any{"id": "m" + itoa(a.msgSeq.Add(1)), "role": "assistant", "content": []any{}}
	if a.kind == "fail" {
		emit(llm(map[string]any{"type": "assistant", "message": msg, "error": "api_error", "apiErrorStatus": 529}))
		return &TurnError{Status: 529}
	}
	emit(llm(map[string]any{"type": "assistant", "message": msg}))
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
