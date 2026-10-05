package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	chatToolSearchID      = "chat-tool-search-tokens"
	toolSearchProject     = "Verify Skills"
	toolSearchPinned      = "tides_lookup"
	toolSearchMinSkills   = 40
	toolSearchLogTailSize = 4 << 20
)

// toolSearchLog is the chat.tool_search line relay-sessions logs once when a
// chat provider starts.
type toolSearchLog struct {
	Active bool
	Reason string
	Skills int
	Pinned int
}

// toolSearchTurn is one fresh chat: a chat.json written before it, one
// message, the first model_call row it recorded, and the Start log line.
type toolSearchTurn struct {
	ConfigErr string
	Create    frontendResponse
	SessionID string
	Message   frontendResponse
	Delete    frontendResponse
	Row       *audit.AuditEvent
	RowsErr   error
	Log       *toolSearchLog
	LogErr    error
}

type chatToolSearchRun struct {
	Models      frontendResponse
	Want, Model string
	ProjectID   string
	ConfigErr   string // chat.json could not be read for the backup
	RestoreErr  string // chat.json could not be put back
	Off, On     toolSearchTurn
}

// runChatToolSearch measures what a chat turn costs in the project Verify
// Skills with tool search off, then on. It backs up the operator's chat.json,
// writes its own, and puts the original back whatever happens.
func runChatToolSearch(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, chatToolSearchID)
	if !ok {
		return res
	}
	r := chatToolSearchRun{Want: verifyModel()}
	r.ProjectID = grantedProjectID(ctx, e, toolSearchProject)
	if r.ProjectID == "" {
		return classifyChatToolSearch(r)
	}
	r.Models = frontendDo(ctx, e, run, http.MethodGet, "/api/models", nil)
	if r.Models.Status == http.StatusOK {
		r.Model = pickModel(r.Models.Body, r.Want)
	}
	if r.Model == "" {
		return classifyChatToolSearch(r)
	}
	path := filepath.Join(e.ConfigDir, "sessions", "chat.json")
	prev, existed, err := readOptionalFile(path)
	if err != nil {
		r.ConfigErr = err.Error()
		return classifyChatToolSearch(r)
	}
	logPath := filepath.Join(e.ConfigDir, "logs", "relaysessions.log")
	r.Off = driveToolSearchTurn(ctx, e, launch, run, r, path, logPath, `{"toolSearch":{"mode":"off"}}`, "off")
	if r.Off.ConfigErr == "" && r.Off.Message.Status == http.StatusOK {
		r.On = driveToolSearchTurn(ctx, e, launch, run, r, path, logPath,
			`{"toolSearch":{"mode":"on","pinned":["`+toolSearchPinned+`"]}}`, "on")
	}
	r.RestoreErr = restoreOptionalFile(path, prev, existed)
	return classifyChatToolSearch(r)
}

// driveToolSearchTurn writes cfg, then runs one chat. relay-sessions reads
// chat.json when a chat provider starts, so the file is written before the
// session is created. The session is deleted whatever happened, and the
// delete ignores cancellation.
func driveToolSearchTurn(ctx context.Context, e env, launch, run string, r chatToolSearchRun, path, logPath, cfg, label string) toolSearchTurn {
	var t toolSearchTurn
	if err := writeFileAtomic(path, []byte(cfg)); err != nil {
		t.ConfigErr = "write chat.json: " + err.Error()
		return t
	}
	start := time.Now().Add(-time.Second)
	// useRelayTools is what eve's web chat sets on every non-Claude chat; without it
	// the chat has no MCP tools and tool search reads no_tools.
	body := jsonBody(map[string]any{"projectId": r.ProjectID, "name": "verify-" + e.Nonce + "-tools-" + label, "model": r.Model, "settings": map[string]bool{"useRelayTools": true}})
	t.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(t.Create.Body, &created)
	if t.SessionID = created.SessionID; t.Create.Status != http.StatusCreated || t.SessionID == "" {
		return t
	}
	t.Message = frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/sessions/"+t.SessionID+"/message",
		jsonBody(map[string]string{"text": "Reply with the single word: ready"}), messageTimeout)
	t.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+t.SessionID, nil)
	// Relay records a model call after it returns, off the caller's path. A
	// failed turn is read once: its row, if any, says whether the host was down.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var rows []audit.AuditEvent
		rows, t.RowsErr = modelCallRows(ctx, e, start, r.ProjectID)
		t.Row = nil
		if len(rows) > 0 {
			t.Row = &rows[0]
		}
		if t.RowsErr != nil || t.Row != nil || t.Message.Status != http.StatusOK || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if t.Message.Status != http.StatusOK {
		return t
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		t.Log, t.LogErr = readToolSearchLog(logPath, t.SessionID)
		if t.LogErr != nil || t.Log != nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return t
}

// grantedProjectID resolves a project name as relay grant lists it; "" when
// it is not there.
func grantedProjectID(ctx context.Context, e env, name string) string {
	out, err := exec.CommandContext(ctx, e.RelayBin, "grant", "--json").Output()
	if err != nil {
		return ""
	}
	var projects []struct{ ID, Name string }
	if json.Unmarshal(out, &projects) != nil {
		return ""
	}
	for _, p := range projects {
		if p.Name == name {
			return p.ID
		}
	}
	return ""
}

// readToolSearchLog reads the tail of relay-sessions' log for the Start line
// of one session. nil, nil means the file is readable and has no such line yet.
func readToolSearchLog(path, sessionID string) (*toolSearchLog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("relay-sessions log unreadable: " + err.Error())
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > toolSearchLogTailSize {
		_, _ = f.Seek(fi.Size()-toolSearchLogTailSize, io.SeekStart)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.New("relay-sessions log unreadable: " + err.Error())
	}
	return parseToolSearchLog(raw, sessionID), nil
}

// parseToolSearchLog finds the line that names the session and carries the
// decision fields; the config-invalid warning has no "active" and so is not it.
func parseToolSearchLog(raw []byte, sessionID string) *toolSearchLog {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var l struct {
			SessionID string  `json:"session_id"`
			Active    *bool   `json:"active"`
			Reason    *string `json:"reason"`
			Skills    int     `json:"skills"`
			Pinned    int     `json:"pinned"`
		}
		if json.Unmarshal(line, &l) != nil || l.SessionID != sessionID || l.Active == nil || l.Reason == nil {
			continue
		}
		return &toolSearchLog{Active: *l.Active, Reason: *l.Reason, Skills: l.Skills, Pinned: l.Pinned}
	}
	return nil
}

// readOptionalFile answers the file's bytes, and false when it is absent.
func readOptionalFile(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func restoreOptionalFile(path string, prev []byte, existed bool) string {
	if !existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "remove chat.json: " + err.Error()
		}
		return ""
	}
	if err := writeFileAtomic(path, prev); err != nil {
		return "restore chat.json: " + err.Error()
	}
	return ""
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".verify-tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// toolSearchTurnFailure names what stopped one turn from yielding numbers,
// as a result, or ok false when the turn has them.
func toolSearchTurnFailure(label, project string, t toolSearchTurn) (result, bool) {
	id := chatToolSearchID
	fail := func(d string) (result, bool) { return result{id, stateFail, label + ": " + d}, true }
	switch {
	case t.ConfigErr != "":
		return blocked(id, label+": "+t.ConfigErr), true
	case t.Create.Status != http.StatusCreated || t.SessionID == "":
		r := launchRefusal(id, "/api/sessions", project, t.Create)
		r.Detail = label + ": " + r.Detail
		return r, true
	case hostUnavailable(rowsOf(t.Row)):
		return blocked(id, label+": the model host answered 503"), true
	case t.Message.Status == http.StatusUnauthorized:
		return blocked(id, label+": run credential refused (401) on the message route"), true
	case t.Message.TimedOut:
		return fail("no reply within 60 s")
	case t.Message.Status != http.StatusOK:
		return fail(fmt.Sprintf("message status %d: %s", t.Message.Status, t.Message.Error))
	case t.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: session %s may still be running", t.Delete.Status, t.SessionID))
	case t.RowsErr != nil:
		return blocked(id, t.RowsErr.Error()), true
	case t.Row == nil:
		return fail("no model_call row for the turn")
	case t.Row.Outcome != audit.AuditOutcomeOK:
		return fail(fmt.Sprintf("model_call outcome %q, status %d", t.Row.Outcome, t.Row.Status))
	case t.Row.PromptTokens <= 0:
		return fail("model_call row has no prompt_tokens")
	case t.Row.RequestBytes <= 0:
		return fail("model_call row has no request_bytes")
	case t.LogErr != nil:
		return blocked(id, t.LogErr.Error()), true
	case t.Log == nil:
		return fail("no chat.tool_search line in the relay-sessions log for the session")
	}
	return result{}, false
}

func rowsOf(row *audit.AuditEvent) []audit.AuditEvent {
	if row == nil {
		return nil
	}
	return []audit.AuditEvent{*row}
}

func classifyChatToolSearch(r chatToolSearchRun) result {
	const id = chatToolSearchID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.ProjectID == "":
		return blocked(id, "no "+toolSearchProject+" in relay grant; do the P8 setup")
	case r.Models.Status == 0:
		return blocked(id, "frontend socket unreachable")
	case r.Models.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)")
	case r.Models.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/models status %d: %s", r.Models.Status, r.Models.Error))
	case r.Model == "":
		return blocked(id, fmt.Sprintf("model %q not in GET /api/models; set RELAY_VERIFY_MODEL", r.Want))
	case r.ConfigErr != "":
		return blocked(id, "chat.json: "+r.ConfigErr)
	}
	// A chat.json left changed is a defect of the machine, not a state to hide
	// behind another outcome.
	if r.RestoreErr != "" {
		return fail(r.RestoreErr + ": the operator's chat.json may be lost")
	}
	if res, stop := toolSearchTurnFailure("off", toolSearchProject, r.Off); stop {
		return res
	}
	if res, stop := toolSearchTurnFailure("on", toolSearchProject, r.On); stop {
		return res
	}
	off, on := r.Off.Row, r.On.Row
	switch {
	case r.Off.Log.Active || r.Off.Log.Reason != "off":
		return fail(fmt.Sprintf("mode off logged active=%v reason=%s, want active=false reason=off", r.Off.Log.Active, r.Off.Log.Reason))
	case !r.On.Log.Active || r.On.Log.Reason != "on":
		return fail(fmt.Sprintf("mode on logged active=%v reason=%s, want active=true reason=on", r.On.Log.Active, r.On.Log.Reason))
	case r.On.Log.Skills < toolSearchMinSkills:
		return fail(fmt.Sprintf("tool search indexed %d skills, want at least %d", r.On.Log.Skills, toolSearchMinSkills))
	case r.On.Log.Pinned != 1:
		return fail(fmt.Sprintf("tool search pinned %d tools, want 1", r.On.Log.Pinned))
	case on.PromptTokens >= off.PromptTokens:
		return fail(fmt.Sprintf("prompt_tokens with tool search on %d, off %d; on must be fewer", on.PromptTokens, off.PromptTokens))
	case on.RequestBytes >= off.RequestBytes:
		return fail(fmt.Sprintf("request_bytes with tool search on %d, off %d; on must be fewer", on.RequestBytes, off.RequestBytes))
	}
	return result{id, statePass, fmt.Sprintf("a chat in %s sent %d prompt tokens (%d bytes) with tool search off and %d (%d bytes) with it on; %d skills indexed, %s pinned",
		toolSearchProject, off.PromptTokens, off.RequestBytes, on.PromptTokens, on.RequestBytes, r.On.Log.Skills, toolSearchPinned)}
}
