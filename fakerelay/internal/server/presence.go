package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// presenceGate answers a presence check from the world's script: approve runs
// the operation, deny refuses it, timeout holds until the caller leaves. An
// operation the script does not name is denied.
type presenceGate struct {
	mu       sync.Mutex
	outcomes map[string]string
	log      *events.Log
	audit    *events.Audit
}

func newPresenceGate(script map[string]string, log *events.Log, audit *events.Audit) *presenceGate {
	g := &presenceGate{log: log, audit: audit}
	_ = g.Set(script)
	return g
}

// Set replaces the whole script.
func (g *presenceGate) Set(script map[string]string) error {
	next := map[string]string{}
	for op, out := range script {
		if !world.PresenceOps[op] {
			return fmt.Errorf("unknown operation %q", op)
		}
		if out != "approve" && out != "deny" && out != "timeout" {
			return fmt.Errorf("%s: outcome must be approve, deny or timeout", op)
		}
		next[op] = out
	}
	g.mu.Lock()
	g.outcomes = next
	g.mu.Unlock()
	return nil
}

func (g *presenceGate) Require(ctx context.Context, op string) error {
	start := time.Now()
	g.mu.Lock()
	answer := g.outcomes[op]
	g.mu.Unlock()
	if answer == "" {
		answer = "deny"
	}
	// The gated op rides in presence_op because the event line's own op key is the event name.
	// The answer line is written before a timeout blocks, so a test can wait on it.
	g.log.Begin(ctx, "debug.presence.answer", events.Debug()).Set("presence_op", op).Set("answer", answer).End("ok", "", nil)
	switch answer {
	case "approve":
		return nil
	case "timeout":
		<-ctx.Done()
		g.refuse(ctx, op, ctx.Err().Error(), start)
		return ErrPresenceTimeout
	}
	g.refuse(ctx, op, ErrPresenceRefused.Error(), start)
	return ErrPresenceRefused
}

// refuse writes the control_decision denied row of a presence refusal.
func (g *presenceGate) refuse(ctx context.Context, op, reason string, start time.Time) {
	if g.audit == nil {
		return
	}
	c := CallerFrom(ctx)
	actor := map[string]any{"kind": "operator", "auth": "none"}
	via := "cli"
	if c.Kind == "bearer" || c.Kind == "identity" {
		actor = map[string]any{"kind": "control", "auth": "token", "cred_id": c.CredID}
		via = "http"
	}
	row := map[string]any{
		"id": events.NewUUID(), "ts": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"dur_ms": time.Since(start).Milliseconds(), "event": "control_decision", "actor": actor,
		"outcome": "denied", "error": reason, "scope": nil, "method": op, "via": via,
		"presence_approver": "testapprover",
	}
	if s, ok := ctx.Value(subjectKey).(string); ok && s != "" {
		row["subject"] = s
	}
	_ = g.audit.Append(row)
}
