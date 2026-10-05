package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func toolSearchTurnBase(prompt, bytes int64, log toolSearchLog) toolSearchTurn {
	ok := frontendResponse{Status: http.StatusOK}
	return toolSearchTurn{
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: "s1", Message: ok, Delete: ok,
		Row: &audit.AuditEvent{Outcome: audit.AuditOutcomeOK, PromptTokens: prompt, RequestBytes: bytes},
		Log: &log,
	}
}

func chatToolSearchBase() chatToolSearchRun {
	return chatToolSearchRun{
		Models: frontendResponse{Status: http.StatusOK}, Want: "Chat", Model: "Chat", ProjectID: "p1",
		Off: toolSearchTurnBase(20000, 90000, toolSearchLog{Reason: "off"}),
		On:  toolSearchTurnBase(3000, 14000, toolSearchLog{Active: true, Reason: "on", Skills: 48, Pinned: 1}),
	}
}

func TestClassifyChatToolSearch(t *testing.T) {
	both := func(f func(*toolSearchTurn)) func(*chatToolSearchRun) {
		return func(r *chatToolSearchRun) { f(&r.Off); f(&r.On) }
	}
	checkMuts(t, chatToolSearchBase, classifyChatToolSearch, []mutCase[chatToolSearchRun]{
		{"fewer tokens and bytes with tool search on", func(*chatToolSearchRun) {}, statePass},
		{"project missing", func(r *chatToolSearchRun) { r.ProjectID = "" }, stateBlocked},
		{"socket unreachable", func(r *chatToolSearchRun) { r.Models = frontendResponse{} }, stateBlocked},
		{"run credential refused", func(r *chatToolSearchRun) { r.Models = frontendResponse{Status: http.StatusUnauthorized} }, stateBlocked},
		{"models route failed", func(r *chatToolSearchRun) { r.Models = frontendResponse{Status: http.StatusInternalServerError} }, notPass},
		{"model not listed", func(r *chatToolSearchRun) { r.Model = "" }, stateBlocked},
		{"chat.json unreadable", func(r *chatToolSearchRun) { r.ConfigErr = "permission denied" }, stateBlocked},
		{"chat.json not restored", func(r *chatToolSearchRun) { r.RestoreErr = "restore chat.json: disk full" }, notPass},
		{"chat.json not written", func(r *chatToolSearchRun) { r.Off.ConfigErr = "write chat.json: read-only" }, stateBlocked},
		{"launch refused", both(func(t *toolSearchTurn) { t.Create, t.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }), stateBlocked},
		{"on launch refused", func(r *chatToolSearchRun) {
			r.On.Create, r.On.SessionID = frontendResponse{Status: http.StatusForbidden}, ""
		}, stateBlocked},
		{"model host 503", func(r *chatToolSearchRun) {
			r.Off.Message = frontendResponse{Status: http.StatusBadGateway}
			r.Off.Row = &audit.AuditEvent{Outcome: "error", Status: http.StatusServiceUnavailable}
		}, stateBlocked},
		{"no reply within 60 s", func(r *chatToolSearchRun) { r.On.Message = frontendResponse{TimedOut: true} }, notPass},
		{"message failed", func(r *chatToolSearchRun) { r.Off.Message = frontendResponse{Status: http.StatusBadGateway} }, notPass},
		{"delete failed", func(r *chatToolSearchRun) { r.On.Delete.Status = http.StatusInternalServerError }, notPass},
		{"audit unreadable", func(r *chatToolSearchRun) { r.On.RowsErr = errors.New("relay audit --event model_call failed") }, stateBlocked},
		{"no model_call row", func(r *chatToolSearchRun) { r.Off.Row = nil }, notPass},
		{"model_call not ok", func(r *chatToolSearchRun) { r.On.Row.Outcome = "error" }, notPass},
		{"no prompt_tokens off", func(r *chatToolSearchRun) { r.Off.Row.PromptTokens = 0 }, notPass},
		{"no prompt_tokens on", func(r *chatToolSearchRun) { r.On.Row.PromptTokens = 0 }, notPass},
		{"no request_bytes off", func(r *chatToolSearchRun) { r.Off.Row.RequestBytes = 0 }, notPass},
		{"no request_bytes on", func(r *chatToolSearchRun) { r.On.Row.RequestBytes = 0 }, notPass},
		{"log unreadable", func(r *chatToolSearchRun) { r.On.Log, r.On.LogErr = nil, errors.New("relay-sessions log unreadable") }, stateBlocked},
		{"no start line", func(r *chatToolSearchRun) { r.On.Log = nil }, notPass},
		{"off mode still active", func(r *chatToolSearchRun) { r.Off.Log.Active = true }, notPass},
		{"off mode wrong reason", func(r *chatToolSearchRun) { r.Off.Log.Reason = "no_tools" }, notPass},
		{"on mode inactive", func(r *chatToolSearchRun) { r.On.Log.Active = false }, notPass},
		{"on mode wrong reason", func(r *chatToolSearchRun) { r.On.Log.Reason = "name_collision" }, notPass},
		{"39 skills", func(r *chatToolSearchRun) { r.On.Log.Skills = 39 }, notPass},
		{"40 skills", func(r *chatToolSearchRun) { r.On.Log.Skills = 40 }, statePass},
		{"nothing pinned", func(r *chatToolSearchRun) { r.On.Log.Pinned = 0 }, notPass},
		{"two pinned", func(r *chatToolSearchRun) { r.On.Log.Pinned = 2 }, notPass},
		{"tokens equal", func(r *chatToolSearchRun) { r.On.Row.PromptTokens = r.Off.Row.PromptTokens }, notPass},
		{"tokens grew", func(r *chatToolSearchRun) { r.On.Row.PromptTokens = r.Off.Row.PromptTokens + 1 }, notPass},
		{"bytes equal", func(r *chatToolSearchRun) { r.On.Row.RequestBytes = r.Off.Row.RequestBytes }, notPass},
		{"bytes grew", func(r *chatToolSearchRun) { r.On.Row.RequestBytes = r.Off.Row.RequestBytes + 1 }, notPass},
	}, nil)
}

func TestParseToolSearchLog(t *testing.T) {
	raw := []byte(`{"level":"info","msg":"x","session_id":"s2","active":true,"reason":"on","skills":48,"pinned":1}
not json
{"level":"warn","msg":"chat tool search config invalid","session_id":"s1","error":"chat.json: bad"}
{"level":"info","msg":"chat tool search","op":"chat.tool_search","session_id":"s1","active":true,"reason":"on","skills":45,"pinned":1}
`)
	if got := parseToolSearchLog(raw, "s1"); got == nil || *got != (toolSearchLog{Active: true, Reason: "on", Skills: 45, Pinned: 1}) {
		t.Errorf("parseToolSearchLog(s1) = %+v, want the s1 decision line", got)
	}
	if got := parseToolSearchLog(raw, "s3"); got != nil {
		t.Errorf("parseToolSearchLog(s3) = %+v, want nil", got)
	}
	warnOnly := []byte(`{"level":"warn","msg":"chat tool search config invalid","session_id":"s1","error":"chat.json: bad"}` + "\n")
	if got := parseToolSearchLog(warnOnly, "s1"); got != nil {
		t.Errorf("a config-invalid warning without a decision read as %+v", got)
	}
}

func TestChatJSONRestoredByteForByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	orig := []byte("{\"toolSearch\": {\"mode\":\"auto\"}}\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	prev, existed, err := readOptionalFile(path)
	if err != nil || !existed {
		t.Fatalf("readOptionalFile = %v, %v", existed, err)
	}
	if err := writeFileAtomic(path, []byte(`{"toolSearch":{"mode":"off"}}`)); err != nil {
		t.Fatal(err)
	}
	if msg := restoreOptionalFile(path, prev, existed); msg != "" {
		t.Fatal(msg)
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Errorf("chat.json after restore = %q, want %q", got, orig)
	}

	// A chat.json that did not exist is removed again, not left holding the
	// journey's own settings.
	absent := filepath.Join(dir, "none", "chat.json")
	_ = os.MkdirAll(filepath.Dir(absent), 0o755)
	prev, existed, err = readOptionalFile(absent)
	if err != nil || existed {
		t.Fatalf("readOptionalFile(absent) = %v, %v", existed, err)
	}
	if err := writeFileAtomic(absent, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if msg := restoreOptionalFile(absent, prev, existed); msg != "" {
		t.Fatal(msg)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Errorf("journey's chat.json still there after restore: %v", err)
	}
}
