package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/cmdlookup"
)

const (
	histTermID       = "session-history-terminal"
	histTermInstance = "/tmp/dbv-histterm"
	histTermLinkWait = 60 * time.Second
	histTermReplyMax = 120 * time.Second
	histTermTitleMax = 90 * time.Second
	histTermScanWait = 60 * time.Second
	histTermRowWait  = 60 * time.Second
	histTermShowWait = 60 * time.Second
	histTermImport   = 30 * time.Second
)

// histRow is the part of a session-history row this journey reads.
type histRow struct {
	ID             string  `json:"id"`
	Record         string  `json:"record"`
	Kind           string  `json:"kind"`
	Agent          *string `json:"agent"`
	ProjectID      *string `json:"projectId"`
	ConversationID *string `json:"conversationId"`
	Title          *string `json:"title"`
	State          string  `json:"state"`
	Resume         string  `json:"resume"`
}

func (r histRow) conversation() string {
	if r.ConversationID == nil {
		return ""
	}
	return *r.ConversationID
}

func (r histRow) title() string {
	if r.Title == nil {
		return ""
	}
	return *r.Title
}

// histTerm is what the journey set up and is answerable for in cleanup.
type histTerm struct {
	e         env
	token     string
	projectID string
	terminals []string
	conn      *websocket.Conn
	log       *dropLog
}

// runSessionHistoryTerminal drives the history of a terminal Claude: launched
// from a project's claude-code template, linked to its conversation and named
// with /rename, listed dormant after a relay restart, and resumed by the
// history door onto the same conversation.
//
// Deliberate: the journey runs its own `relay serve` instance (the restart is
// serveInstance.Restart, as sealed-reset restarts one). The stack's own
// instance has no restart handle, and a restart of it would end every other
// journey's terminals. The instance gets one project, a claude-code template
// that runs a launcher pointing Claude at the stack's local model endpoint,
// and one credential, all seeded through settings.json. Off a stack, or with
// no claude_base_url, the journey is NOTRUN, so it never runs claude against
// a hosted model.
func runSessionHistoryTerminal(ctx context.Context, e env) (res result) {
	const id = histTermID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, err.Error())
	}
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	claude, err := cmdlookup.Local(ctx, "claude")
	if err != nil {
		return blocked(id, "no claude on this machine's login-shell PATH")
	}
	dir, err := prepareInstanceDir(ctx, e, histTermInstance)
	if err != nil {
		return blocked(id, err.Error())
	}
	work, err := os.MkdirTemp("", "dbv-hist-")
	if err != nil {
		return blocked(id, "scratch folder: "+err.Error())
	}
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	defer func() { _ = os.RemoveAll(work) }()
	projectDir, launcher := filepath.Join(work, "proj"), filepath.Join(work, "claude.sh")
	if err := writeAdoptSettings(projectDir); err != nil {
		return blocked(id, "scratch folder: "+err.Error())
	}
	if err := os.WriteFile(launcher, []byte(detachedLauncher(claude, baseURL, model, tokenFile)), 0o700); err != nil {
		return blocked(id, "launcher: "+err.Error())
	}

	var inst *serveInstance
	defer func() { res = withTeardown(ctx, res, inst) }() // runs after the terminals are closed below
	if inst, err = startServe(ctx, e, dir); err != nil {
		return fail("%s", err)
	}
	h := &histTerm{e: e, projectID: uuid.NewString(), log: &dropLog{}}
	if h.token, err = seedHistoryInstance(dir, h.projectID, projectDir, launcher, tokenFile, e.Nonce); err != nil {
		return blocked(id, err.Error())
	}
	if h.e.FrontendSocket, err = instanceFrontend(dir); err != nil {
		return fail("%s", err)
	}
	tag := "verify-" + e.Nonce + "-hist"
	marker := "verify-" + e.Nonce + "-title"
	var convID string
	defer func() {
		ctx := context.WithoutCancel(ctx)
		h.closeAll(ctx)
		removeTranscript(id, convID, tag, false, func(ctx context.Context) string {
			if adoptPID(ctx, convID) > 0 {
				return "process still running"
			}
			return ""
		})
	}()

	// Waits: none possible for the settings import (relay's file watch emits no
	// event): the seeded template reads back through the API, bounded.
	if !pollUntil(ctx, histTermImport, func() bool {
		r := frontendDo(ctx, h.e, h.token, http.MethodGet, "/api/terminal/templates/claude-code", nil)
		return r.Status == http.StatusOK && strings.Contains(string(r.Body), launcher)
	}) {
		return blocked(id, "the seeded project and template were not imported within "+histTermImport.String())
	}

	// 1. Launch from the project's claude-code template.
	launchedAt := time.Now()
	create := frontendDoTimeout(ctx, h.e, h.token, http.MethodPost, "/api/terminals", jsonBody(map[string]any{
		"templateId": "claude-code", "projectId": h.projectID, "name": tag, "cols": dropInCols, "rows": dropInRows,
	}), 30*time.Second)
	termID := terminalIDOf(create.Body)
	if create.Status != http.StatusCreated || termID == "" {
		return fail("POST /api/terminals status %d: %s", create.Status, create.Error)
	}
	h.terminals = append(h.terminals, termID)
	if err := h.attach(ctx, termID); err != nil {
		return blocked(id, err.Error())
	}

	// 2. The record is live and links to the conversation.
	// Waits: the HTTP answer for the row, then the session.history.link event for its id.
	var live histRow
	if !pollUntil(ctx, histTermRowWait, func() bool {
		rows, _ := h.rows(ctx)
		for _, r := range rows {
			if r.Kind == "terminal" && r.State == "live" {
				live = r
			}
		}
		return live.ID != ""
	}) {
		return fail("no live terminal row in the history within %s of the launch", histTermRowWait)
	}
	if _, err := waitEvent(ctx, h.e, dir, "session.history.link", launchedAt, histTermLinkWait, func(m map[string]any) bool {
		return m["session_id"] == live.ID && m["outcome"] == "ok"
	}); err != nil {
		return fail("the terminal's record was never linked: %s", err)
	}
	linked, _ := h.rowByID(ctx, live.ID)
	if convID = linked.conversation(); !validUUID(convID) {
		convID = ""
		return fail("after the link event the row carries no conversation id")
	}

	// 3. One prompt, then /rename. Waits: none possible for the reply (model
	// output): a bounded read of the transcript for the assistant record.
	prompt := "Reply with exactly: " + tag + "-1"
	if err := h.type_(termID, prompt+"\r"); err != nil {
		return blocked(id, "terminal input: "+err.Error())
	}
	if !pollUntil(ctx, histTermReplyMax, func() bool { return adoptAssistantHas(convID, tag+"-1") }) {
		return blocked(id, "the terminal's claude did not reply "+tag+"-1 within "+histTermReplyMax.String()+" (model output): "+h.tail(termID))
	}
	// Waits: none possible for /rename (terminal UI): the history row is read, bounded.
	if err := h.type_(termID, "/rename "+marker+"\r"); err != nil {
		return blocked(id, "terminal input: "+err.Error())
	}
	var titled histRow
	if !pollUntil(ctx, histTermTitleMax, func() bool {
		titled, _ = h.rowByID(ctx, live.ID)
		return titled.title() == marker
	}) {
		return fail("the row never carried the title %q within %s (title %q); terminal: %s", marker, histTermTitleMax, titled.title(), h.tail(termID))
	}

	// 4. Restart relay. Waits: Restart returns at ready; then the first scan event.
	restartedAt := time.Now()
	h.dropConn()
	if err := inst.Restart(ctx, e); err != nil {
		return fail("restart: %s", err)
	}
	if h.e.FrontendSocket, err = instanceFrontend(dir); err != nil {
		return fail("%s", err)
	}
	if _, err := waitEvent(ctx, h.e, dir, "session.history.scan", restartedAt, histTermScanWait, nil); err != nil {
		return fail("after the restart: %s", err)
	}

	// 5. The row is dormant, resumable through the history door, titled.
	var dormant histRow
	if !pollUntil(ctx, histTermRowWait, func() bool {
		dormant, _ = h.rowByID(ctx, live.ID)
		return dormant.State == "dormant" && dormant.Resume == "history"
	}) {
		return fail("after the restart the row is state %q resume %q, want dormant and history", dormant.State, dormant.Resume)
	}
	if dormant.title() != marker || dormant.conversation() != convID {
		return fail("after the restart the row lost its title or conversation (title %q)", dormant.title())
	}

	// 6. Resume. The new terminal's Claude shows the earlier prompt.
	resume := frontendDoTimeout(ctx, h.e, h.token, http.MethodPost, "/api/session-history/"+dormant.ID+"/resume",
		jsonBody(map[string]int{"cols": dropInCols, "rows": dropInRows}), 60*time.Second)
	newTerm := terminalIDOf(resume.Body)
	if resume.Status != http.StatusCreated || newTerm == "" {
		return fail("POST /api/session-history/{id}/resume status %d, want 201: %s", resume.Status, resume.Error)
	}
	h.terminals = append(h.terminals, newTerm)
	var body struct {
		Supersedes string `json:"supersedes"`
	}
	_ = json.Unmarshal(resume.Body, &body)
	if body.Supersedes != dormant.ID {
		return fail("201 supersedes %q, want the resumed row's id", body.Supersedes)
	}
	if err := h.attach(ctx, newTerm); err != nil {
		return blocked(id, err.Error())
	}
	// Waits: none possible for the TUI redraw: the terminal frames are read, bounded.
	if !h.log.waitFor(ctx, histTermShowWait, func(fs []dropFrame) bool {
		return strings.Contains(terminalText(fs, newTerm), tag+"-1")
	}) {
		return fail("the resumed terminal never showed the earlier prompt %q within %s: %s", tag+"-1", histTermShowWait, h.tail(newTerm))
	}
	rows, _ := h.rows(ctx)
	for _, r := range rows {
		if r.ID == dormant.ID {
			return fail("the old record is still listed after the resume")
		}
	}
	if !slices.ContainsFunc(rows, func(r histRow) bool { return r.Kind == "terminal" && r.State == "live" && r.conversation() == convID }) {
		return fail("no live terminal row carries conversation %s after the resume", convID)
	}
	return result{id, statePass, "claude-code terminal in a project linked to its conversation and titled by /rename; listed dormant with resume history after a relay restart; the history door answered 201 and the new terminal's Claude showed the earlier prompt; the old record is gone"}
}

// terminalIDOf reads the terminal id from a launch body, top level or nested.
func terminalIDOf(body []byte) string {
	var v struct {
		TerminalID string `json:"terminalId"`
		Terminal   struct {
			TerminalID string `json:"terminalId"`
			ID         string `json:"id"`
		} `json:"terminal"`
	}
	_ = json.Unmarshal(body, &v)
	return cmp.Or(v.TerminalID, v.Terminal.TerminalID, v.Terminal.ID)
}

// instanceFrontend is the frontend socket ready.json names; it carries the
// server's pid, so it changes on every restart.
func instanceFrontend(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	var ready struct {
		Sockets struct {
			Frontend string `json:"frontend"`
		} `json:"sockets"`
	}
	if err != nil || json.Unmarshal(raw, &ready) != nil || ready.Sockets.Frontend == "" {
		return "", errors.New("ready.json names no frontend socket")
	}
	return ready.Sockets.Frontend, nil
}

// seedHistoryInstance writes one local project, the claude-code template (run
// through the launcher, sandboxed) and an expiring credential into the
// instance's settings.json, and returns the credential's token. The token is
// in no file or detail beyond the hash settings.json keeps.
func seedHistoryInstance(dir, projectID, projectDir, launcher, tokenFile, nonce string) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	cred := map[string]any{
		"id": uuid.NewString(), "name": "verify-" + nonce + "-hist", "hash": hex.EncodeToString(hash[:]),
		"classes": []string{"read", "execute", "proxy"}, "created": now.Format(time.RFC3339),
		"expires": now.Add(time.Hour).Format(time.RFC3339),
	}
	projSecret := make([]byte, 32)
	if _, err := rand.Read(projSecret); err != nil {
		return "", err
	}
	projHash := sha256.Sum256([]byte(hex.EncodeToString(projSecret)))
	project := map[string]any{
		"id": projectID, "name": "Verify History " + nonce, "path": projectDir,
		"allowed_mcp_ids": []string{}, "allowed_models": []string{"*"}, "allowed_templates": []string{"claude-code"},
		"token": hex.EncodeToString(projSecret), "token_hash": hex.EncodeToString(projHash[:]), "created_at": now.Format(time.RFC3339),
	}
	template := map[string]any{
		"id": "claude-code", "name": "Claude Code", "command": launcher, "args": []string{"--model", "haiku", "--permission-mode", "default"},
		"sandbox":    true,
		"read_write": []string{"~/.claude", "~/.claude.json", "~/.cache", "~/Library/Caches"},
		"read": []string{"~/Library/Keychains", "~/.local/bin", "~/.local/share/claude", "/opt/homebrew",
			filepath.Dir(launcher), filepath.Dir(tokenFile)},
	}
	err := editSettings(dir, func(top map[string]json.RawMessage) error {
		var templates []json.RawMessage
		if raw := top["terminal_templates"]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &templates); err != nil {
				return fmt.Errorf("settings.json terminal_templates: %w", err)
			}
		}
		kept := templates[:0]
		for _, t := range templates {
			var h struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(t, &h) != nil || h.ID != "claude-code" {
				kept = append(kept, t)
			}
		}
		b, _ := json.Marshal(template)
		top["terminal_templates"], _ = json.Marshal(append(kept, b))
		return appendRecords(top, map[string]any{"projects": project, "api_credentials": cred})
	})
	if err != nil {
		return "", fmt.Errorf("seeding the instance: %w", err)
	}
	return token, nil
}

// rows reads the history for the journey's project. ready is not required: a
// caller polls for the row it wants.
func (h *histTerm) rows(ctx context.Context) ([]histRow, bool) {
	r := frontendDo(ctx, h.e, h.token, http.MethodGet, "/api/session-history?projectId="+h.projectID, nil)
	var v struct {
		Ready bool      `json:"ready"`
		Rows  []histRow `json:"rows"`
	}
	if r.Status != http.StatusOK || json.Unmarshal(r.Body, &v) != nil {
		return nil, false
	}
	return v.Rows, v.Ready
}

func (h *histTerm) rowByID(ctx context.Context, id string) (histRow, bool) {
	rows, _ := h.rows(ctx)
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return histRow{}, false
}

// attach opens the terminal WebSocket and joins the terminal. Waits: the
// terminal_joined frame.
func (h *histTerm) attach(ctx context.Context, termID string) error {
	h.dropConn()
	conn, status, err := dialFrontendWS(ctx, h.e, h.token)
	if err != nil {
		return fmt.Errorf("GET /ws status %d: %w", status, err)
	}
	h.conn = conn
	collectFrames(conn, h.log)
	if err := wsSend(conn, map[string]string{"type": "join_terminal", "terminalId": termID}); err != nil {
		return err
	}
	if !h.log.waitFor(ctx, 15*time.Second, frameSeen(0, func(f dropFrame) bool {
		return f.Type == "terminal_joined" && f.TerminalID == termID
	})) {
		return errors.New("no terminal_joined frame for " + termID)
	}
	return nil
}

func (h *histTerm) type_(termID, text string) error {
	return wsSend(h.conn, map[string]string{"type": "terminal_input", "terminalId": termID,
		"data": base64.StdEncoding.EncodeToString([]byte(text))})
}

func (h *histTerm) tail(termID string) string {
	t := strings.TrimSpace(terminalText(h.log.snapshot(), termID))
	if len(t) > 300 {
		t = t[len(t)-300:]
	}
	return fmt.Sprintf("%q", t)
}

func (h *histTerm) dropConn() {
	if h.conn != nil {
		_ = h.conn.Close()
		h.conn = nil
	}
}

// closeAll closes only the terminals this journey opened, over the instance's
// current socket; a terminal the restart already ended answers 404.
func (h *histTerm) closeAll(ctx context.Context) {
	h.dropConn()
	for _, t := range h.terminals {
		frontendDo(ctx, h.e, h.token, http.MethodDelete, "/api/terminals/"+t, nil)
	}
}
