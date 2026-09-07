// Package presence gates the operations ADR-017 decisions 3 and 4 name —
// mint, revoke, register, grant-widening — behind a LocalAuthentication
// user-presence check bound to the exact operation and the exact arguments
// being approved (§6 of the ADR-017 implementation spec).
//
// The package boundary is doing security work, not tidiness: Grant has only
// unexported fields, so package main cannot construct a meaningful one — it
// can only be produced by Request or Require.
package presence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Provider asks the operating system for a user-presence act.
type Provider interface {
	// Evaluate blocks until the user is present, refuses, or the OS says
	// this session cannot show a prompt. reason is shown in the OS dialog.
	Evaluate(ctx context.Context, reason string) error
}

var (
	ErrNoSession   = errors.New("no session can display a presence prompt")
	ErrRefused     = errors.New("presence was refused")
	ErrUnavailable = errors.New("presence checking is unavailable")

	// ErrGrantInvalid is Redeem's one refusal reason. Expired, unknown,
	// already-burned, wrong-op and wrong-digest all return this same value:
	// a distinguishable answer per case would be an oracle, the same reason
	// AuthenticateAPICredential gives for its own uniform refusal.
	ErrGrantInvalid = errors.New("presence grant is invalid")

	// ErrUnknownOp is Request's refusal when op is not in GatedOps — a
	// programmer error (a typo in a core's op name), not a security
	// decision, but one that must fail loudly rather than silently minting
	// a grant nothing will ever check.
	ErrUnknownOp = errors.New("presence: op is not a gated operation")
)

// GatedOps is every operation name this package knows about (§6.4). Require
// and Request refuse an op that is not in it.
var GatedOps = []string{
	"credential.mint",
	"credential.revoke",
	"enrolment.create",
	"enrolment.update",
	"enrolment.revoke",
	"enrolment.sign",
	"login.bootstrap.mint",
	"login.passkey.revoke",
	"mcp.register",
	"mcp.oauth.start",
	"service.register",
	"project.rotate_token",
	"project.grant",
	"sealed.reset",
	"eve.enrolment.open",
}

var gatedOps = func() map[string]struct{} {
	m := make(map[string]struct{}, len(GatedOps))
	for _, op := range GatedOps {
		m[op] = struct{}{}
	}
	return m
}()

// Grant is proof that a presence act was completed for exactly one
// operation and one argument digest, and has not been spent.
type Grant struct {
	id     string
	op     string
	digest Digest
}

// ID is the nonce id, for the audit record (presence_id, §7.5). It carries
// no plaintext and no hash.
func (g Grant) ID() string { return g.id }

// Valid is false for the zero value.
func (g Grant) Valid() bool { return g.id != "" }

const (
	// grantLifetime is §3.1's 120-second bound. Require spends a grant in
	// the same call that mints it, so the live window production sees is
	// microseconds; this is the outer limit for a caller that splits
	// Request from Redeem.
	grantLifetime = 120 * time.Second

	// maxNonces bounds the in-memory table; the oldest entry is evicted
	// once it is exceeded. There is no persistence and no cross-process
	// nonce, by design (§6.2).
	maxNonces = 64
)

type nonce struct {
	op     string
	digest Digest
	expiry time.Time
}

// clock exists only so tests can cross the 120s boundary deterministically
// with a fake; production always uses realClock via NewGate's zero value.
type clock interface{ now() time.Time }

type realClock struct{}

func (realClock) now() time.Time { return time.Now() }

// Gate is the whole production path for a presence-bound act: prompt, mint
// a nonce, redeem it. A Gate is safe for concurrent use.
type Gate struct {
	provider Provider
	clock    clock

	mu     sync.Mutex
	nonces map[string]*nonce
	order  []string // insertion order, for maxNonces eviction
}

// NewGate refuses a nil provider: a Gate with nothing behind it would either
// panic on first use or need a nil check at every call site that someone
// eventually forgets — refusing at construction moves that failure to the
// one place it can't be missed.
func NewGate(p Provider) (*Gate, error) {
	if p == nil {
		return nil, errors.New("presence: provider must not be nil")
	}
	return &Gate{provider: p, clock: realClock{}, nonces: make(map[string]*nonce)}, nil
}

// Require is the whole production path: prompt, mint a nonce, redeem it
// immediately. It is the only path production uses, so the live window
// between a successful prompt and the act is microseconds — the 120s bound
// in Request/Redeem is the outer limit for a caller that splits the two,
// which nothing does today.
func (g *Gate) Require(ctx context.Context, op string, d Digest, reason string) (Grant, error) {
	gr, err := g.Request(ctx, op, d, reason)
	if err != nil {
		return Grant{}, err
	}
	if err := g.Redeem(gr, op, d); err != nil {
		return Grant{}, err
	}
	return gr, nil
}

// Request prompts first and mints the nonce only after the prompt succeeds.
//
// A caller session that is known and cannot display a prompt (§6.6) refuses
// immediately with ErrNoSession, without ever reaching the provider — this
// is what keeps a caller over SSH from raising a password prompt on the
// physical console. A caller with no session information on the context at
// all prompts: that covers every door with no peer to ask about (the
// WebView IPC, the tray menu, the loopback TCP mux), and refusing them would
// break the Settings window and the browser view for no security gain.
func (g *Gate) Request(ctx context.Context, op string, d Digest, reason string) (Grant, error) {
	if _, ok := gatedOps[op]; !ok {
		return Grant{}, ErrUnknownOp
	}
	if sess, ok := CallerSessionFromContext(ctx); ok && !sess.GraphicAccess {
		return Grant{}, ErrNoSession
	}
	if err := g.provider.Evaluate(ctx, reason); err != nil {
		return Grant{}, err
	}
	id, err := newNonceID()
	if err != nil {
		return Grant{}, fmt.Errorf("presence: minting grant id: %w", err)
	}
	g.mu.Lock()
	g.store(id, op, d)
	g.mu.Unlock()
	return Grant{id: id, op: op, digest: d}, nil
}

// Redeem refuses unless a live nonce exists with the same id, the same op,
// and a constant-time-equal digest, then burns it.
func (g *Gate) Redeem(gr Grant, op string, d Digest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.nonces[gr.id]
	if !ok || !g.clock.now().Before(n.expiry) || n.op != op || !n.digest.Equal(d) {
		return ErrGrantInvalid
	}
	// Single use: delete on the first successful redemption, so a second
	// attempt with the very same grant lands on the !ok branch above and
	// gets the identical ErrGrantInvalid rather than a distinguishable
	// "already used" answer.
	delete(g.nonces, gr.id)
	return nil
}

func (g *Gate) store(id, op string, d Digest) {
	if len(g.nonces) >= maxNonces {
		for len(g.order) > 0 && len(g.nonces) >= maxNonces {
			oldest := g.order[0]
			g.order = g.order[1:]
			delete(g.nonces, oldest)
		}
	}
	g.nonces[id] = &nonce{op: op, digest: d, expiry: g.clock.now().Add(grantLifetime)}
	g.order = append(g.order, id)
}

func newNonceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
