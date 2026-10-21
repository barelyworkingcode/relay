package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const (
	swapBackID       = "session-swap-back"
	swapDropInWait   = 75 * time.Second
	swapHandBackWait = 90 * time.Second
)

// runSwapBack drives the swap-and-back an owner does from eve: a headless
// Claude session on a loopback host is dropped into a terminal, a send while
// it is held is refused, and closing the terminal resumes the same
// conversation headless with no further message. The fixture is
// session-detached-host's: a loopback host, a project on it and an agent
// entry in settings.json. Deliberate additions: the host's claude-code
// template runs the entry's launcher too, so the drop-in terminal reaches the
// stack's local model endpoint, and a credential of classes proxy and execute
// stands in for eve's (the drop-in needs execute, the terminal delete proxy).
func runSwapBack(ctx context.Context, e env) result {
	const id = swapBackID
	baseURL, model, tokenFile, err := stackClaude(e)
	if err != nil {
		return blocked(id, "fixture host or project: "+err.Error())
	}
	// Deliberate: off a stack the launcher would run claude against the
	// machine's own login, a hosted model.
	if baseURL == "" {
		return result{id, stateNotRun, "needs a stack's local model endpoint (claude_base_url in stack.json)"}
	}
	fx, err := setUpDetachedFixture(ctx, e, baseURL, model, tokenFile)
	if err != nil {
		return blocked(id, "fixture host or project: "+err.Error())
	}
	token, credID, err := seedSwapCredential(e, fx)
	if err != nil {
		return blocked(id, "fixture credential: "+err.Error()+fx.tearDown())
	}
	res := driveSwapBack(ctx, e, fx, token)
	res.Detail += dropSwapCredential(fx, credID) + fx.tearDown()
	return res
}

// seedSwapCredential points the host's claude-code template at the entry's
// launcher and adds the proxy+execute credential. The token goes into no
// file, log or detail beyond the hash settings.json keeps.
func seedSwapCredential(e env, fx *detachedFixture) (token, credID string, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	credID = uuid.NewString()
	cred := map[string]any{
		"id": credID, "name": "verify-" + e.Nonce + "-swap", "hash": hex.EncodeToString(hash[:]),
		"classes": []string{"proxy", "execute"}, "created": time.Now().UTC().Format(time.RFC3339),
		"expires": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	err = editSettings(fx.configDir, func(top map[string]json.RawMessage) error {
		var hosts []map[string]any
		if err := json.Unmarshal(top["hosts"], &hosts); err != nil {
			return fmt.Errorf("settings.json hosts: %w", err)
		}
		for _, h := range hosts {
			if h["id"] == fx.hostID {
				h["terminal_templates"] = []map[string]string{
					{"id": "shell", "name": "Shell"},
					{"id": "claude-code", "name": "Claude Code", "command": fx.launcher},
				}
			}
		}
		b, err := json.Marshal(hosts)
		if err != nil {
			return err
		}
		top["hosts"] = b
		return appendRecords(top, map[string]any{"api_credentials": cred})
	})
	return token, credID, err
}

func dropSwapCredential(fx *detachedFixture, credID string) string {
	if err := editSettings(fx.configDir, func(top map[string]json.RawMessage) error {
		dropRecords(top, "api_credentials", credID)
		return nil
	}); err != nil {
		return "; teardown: " + err.Error()
	}
	return ""
}

// swapBody is the part of the drop-in's 201 the journey reads.
type swapBody struct {
	SessionID       string `json:"sessionId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	Terminal        struct {
		ID string `json:"id"`
	} `json:"terminal"`
}

func driveSwapBack(ctx context.Context, e env, fx *detachedFixture, token string) (res result) {
	const id = swapBackID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }

	// Waits: none possible for the settings import; relay's file watch emits
	// no event. The seeded credential answers 200 only once the import has
	// run, so the read is retried, bounded.
	if !pollUntil(ctx, detachedStartTries, func() bool {
		return frontendDo(ctx, e, token, http.MethodGet, "/api/terminals", nil).Status == http.StatusOK
	}) {
		return blocked(id, "the seeded credential was not imported within "+detachedStartTries.String())
	}

	// A start can fail until the import has also added the project, so the
	// start is retried, bounded, as in session-detached-host.
	var sid string
	var startErr error
	if !pollUntil(ctx, detachedStartTries, func() bool {
		sid, startErr = startHostSession(ctx, e, fx.projectID, fx.entry)
		return startErr == nil
	}) {
		return fail("relay session start: %v", startErr)
	}
	var termID string
	defer func() {
		bg := context.WithoutCancel(ctx)
		if termID != "" {
			frontendDo(bg, e, token, http.MethodDelete, "/api/terminals/"+termID, nil)
		}
		cliStopSession(bg, e, sid)
	}()

	marker1 := "verify-" + e.Nonce + "-swap-one"
	if d := swapTurn(ctx, e, sid, marker1); d != "" {
		return fail("first turn: %s", d)
	}

	since := time.Now()
	drop := frontendDoTimeout(ctx, e, token, http.MethodPost, "/api/sessions/"+sid+"/drop-in", []byte(`{}`), swapDropInWait)
	if drop.Status != http.StatusCreated {
		return fail("POST /api/sessions/{id}/drop-in status %d, want 201: %s %s", drop.Status, drop.Error, dropInMessage(drop.Body))
	}
	var body swapBody
	_ = json.Unmarshal(drop.Body, &body)
	termID = body.Terminal.ID
	if body.SessionID != sid || termID == "" {
		return fail("drop-in answered session %q terminal %q, want session %s and a terminal id", body.SessionID, termID, sid)
	}

	held := cliWithTimeout(ctx, e, detachedCLIWait, "session", "send", "--id", sid, "--json", "--text", "Reply with exactly: verify-"+e.Nonce+"-held")
	if held.Exit != 1 || !strings.Contains(held.Stdout+held.Stderr, "dropped_in") {
		return fail("relay session send while held: exit %d, want 1 with dropped_in: %q", held.Exit, firstLine(held.Stderr+held.Stdout))
	}

	del := frontendDo(ctx, e, token, http.MethodDelete, "/api/terminals/"+termID, nil)
	if del.Status/100 != 2 {
		return fail("DELETE /api/terminals/{id} status %d", del.Status)
	}
	termID = ""
	ev, err := waitEvent(ctx, e, e.ConfigDir, "session.hand_back", since, swapHandBackWait, func(m map[string]any) bool {
		return m["session_id"] == sid
	})
	if err != nil {
		return fail("after the terminal was deleted: %v", err)
	}
	if ev["status"] != "ok" {
		return fail("session.hand_back status %v reason %v, want ok", ev["status"], ev["reason"])
	}

	marker2 := "verify-" + e.Nonce + "-swap-two"
	if d := swapTurn(ctx, e, sid, marker2); d != "" {
		return fail("second turn on the same session after the hand back: %s", d)
	}

	if d := swapAuditProblem(ctx, e, sid); d != "" {
		return fail("%s", d)
	}
	return result{id, statePass, "a session on a loopback host was dropped into a terminal, a send while held answered dropped_in, deleting the terminal resumed the session headless (session.hand_back ok), the next turn on the same session was answered, and four session_swap rows read pending then ok in both directions"}
}

// swapTurn sends one message and reads its answer; relay session message
// returns after the turn, so the exit is the wait. It returns a problem text.
func swapTurn(ctx context.Context, e env, sid, marker string) string {
	r := cliWithTimeout(ctx, e, detachedTurnWait, "session", "message", "--id", sid, "--json", "--text", "Reply with exactly: "+marker)
	var reply struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(r.Stdout), &reply)
	if r.Exit != 0 || !bytes.Contains([]byte(reply.Text), []byte(marker)) {
		return fmt.Sprintf("relay session message: exit %d, stderr %q, reply %q", r.Exit, firstLine(r.Stderr), tail(reply.Text, 80))
	}
	return ""
}

// swapAuditProblem reads the session_swap rows once, after session.hand_back:
// completion rows are durable before the event ends.
func swapAuditProblem(ctx context.Context, e env, sid string) string {
	rows, err := auditJSONRows(ctx, e, "--event", audit.AuditEventSessionSwap, "--grep", sid, "--json", "--tail", "50")
	if err != nil {
		return err.Error()
	}
	type key struct{ direction, phase string }
	seen := map[key][]audit.AuditEvent{}
	for _, row := range rows {
		var a struct {
			SessionID string `json:"session_id"`
			Direction string `json:"direction"`
		}
		_ = json.Unmarshal(row.Args, &a)
		if row.Event == audit.AuditEventSessionSwap && a.SessionID == sid {
			k := key{a.Direction, row.Phase}
			seen[k] = append(seen[k], row)
		}
	}
	for _, dir := range []string{"to_terminal", "to_headless"} {
		intent, done := seen[key{dir, audit.AuditPhaseIntent}], seen[key{dir, audit.AuditPhaseCompletion}]
		if len(intent) != 1 || len(done) != 1 {
			return fmt.Sprintf("session_swap %s: %d intent and %d completion rows, want 1 and 1", dir, len(intent), len(done))
		}
		if intent[0].ID != done[0].ID {
			return fmt.Sprintf("session_swap %s: intent and completion rows carry different ids", dir)
		}
		if intent[0].Outcome != "pending" || done[0].Outcome != audit.AuditOutcomeOK {
			return fmt.Sprintf("session_swap %s: intent outcome %q completion outcome %q error %q, want pending and ok", dir, intent[0].Outcome, done[0].Outcome, done[0].Error)
		}
	}
	return ""
}
