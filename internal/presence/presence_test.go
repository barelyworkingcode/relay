package presence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock lets nonce-expiry tests cross the 120s boundary without
// sleeping. Production always uses realClock (NewGate's zero value).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// countingProvider is a minimal local fake — presencetest exists for the
// rest of the suite, but the presence package's own white-box tests avoid
// depending on it so Gate's contract can be checked from first principles.
type countingProvider struct {
	mu     sync.Mutex
	calls  int
	result error
}

func (c *countingProvider) Evaluate(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.result
}

func (c *countingProvider) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newGateForTest(t *testing.T, p Provider) (*Gate, *fakeClock) {
	t.Helper()
	g, err := NewGate(p)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	fc := newFakeClock()
	g.clock = fc
	return g, fc
}

func TestGate_NilProviderRefused(t *testing.T) {
	if _, err := NewGate(nil); err == nil {
		t.Fatal("NewGate(nil) succeeded; a gate with nothing behind it must be refused at construction")
	}
}

func TestGate_UnknownOpRefused(t *testing.T) {
	g, _ := newGateForTest(t, &countingProvider{})
	_, err := g.Request(context.Background(), "not.a.real.op", Digest{}, "do a thing")
	if !errors.Is(err, ErrUnknownOp) {
		t.Fatalf("got %v, want ErrUnknownOp", err)
	}
}

func TestGate_HappyPathMintsAUsableGrant(t *testing.T) {
	g, _ := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "eve-view").Build()
	gr, err := g.Require(context.Background(), "credential.mint", d, "mint a credential")
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if !gr.Valid() || gr.ID() == "" {
		t.Fatal("Require returned an invalid or empty-id grant on success")
	}
}

func TestGate_SingleUse(t *testing.T) {
	// AC-21: redeeming the same grant twice — the second refuses.
	g, _ := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "eve-view").Build()
	gr, err := g.Request(context.Background(), "credential.mint", d, "mint a credential")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := g.Redeem(gr, "credential.mint", d); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if err := g.Redeem(gr, "credential.mint", d); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("second Redeem of the same grant returned %v, want ErrGrantInvalid", err)
	}
}

func TestGate_OperationBound(t *testing.T) {
	// AC-22: a prompt answered for an enrolment cannot be spent on a mint.
	g, _ := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("enrolment.create").StringField("client_id", true, "vm-agent-3").Build()
	gr, err := g.Request(context.Background(), "enrolment.create", d, "create an enrolment")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := g.Redeem(gr, "credential.mint", d); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("Redeem against a different op returned %v, want ErrGrantInvalid", err)
	}
	// The grant is still live for its real operation, proving the failure
	// above was the op check and not some other corruption.
	if err := g.Redeem(gr, "enrolment.create", d); err != nil {
		t.Fatalf("Redeem against the correct op failed after the wrong-op attempt: %v", err)
	}
}

func TestGate_ArgumentDigestBound(t *testing.T) {
	// AC-22: a prompt answered for `mint --class read` cannot mint
	// `--class read --class grant --class execute`.
	g, _ := newGateForTest(t, &countingProvider{})
	dRead := NewDigestBuilder("credential.mint").StringField("name", true, "x").StringSetField("classes", true, []string{"read"}).Build()
	dWide := NewDigestBuilder("credential.mint").StringField("name", true, "x").StringSetField("classes", true, []string{"read", "grant", "execute"}).Build()

	gr, err := g.Request(context.Background(), "credential.mint", dRead, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := g.Redeem(gr, "credential.mint", dWide); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("Redeem against a wider digest returned %v, want ErrGrantInvalid", err)
	}
}

func TestGate_ExpiresAt120Seconds(t *testing.T) {
	g, fc := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "x").Build()

	gr1, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	fc.advance(119 * time.Second)
	if err := g.Redeem(gr1, "credential.mint", d); err != nil {
		t.Fatalf("Redeem at 119s: %v, want success", err)
	}

	gr2, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	fc.advance(121 * time.Second)
	if err := g.Redeem(gr2, "credential.mint", d); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("Redeem at 121s returned %v, want ErrGrantInvalid", err)
	}
}

func TestGate_RefusalsAreUniform(t *testing.T) {
	// AC-22c: expired, unknown, already-burned, wrong-op and wrong-digest
	// must all be the identical error value.
	g, fc := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "x").Build()
	other := NewDigestBuilder("credential.mint").StringField("name", true, "y").Build()

	expired, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	fc.advance(121 * time.Second)

	burned, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := g.Redeem(burned, "credential.mint", d); err != nil {
		t.Fatalf("burning the grant: %v", err)
	}

	live, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	unknown := Grant{}

	cases := map[string]error{
		"expired":      g.Redeem(expired, "credential.mint", d),
		"already used": g.Redeem(burned, "credential.mint", d),
		"unknown id":   g.Redeem(unknown, "credential.mint", d),
		"wrong op":     g.Redeem(live, "enrolment.create", d),
		"wrong digest": g.Redeem(live, "credential.mint", other),
	}
	for name, got := range cases {
		if !errors.Is(got, ErrGrantInvalid) {
			t.Errorf("%s: got %v, want ErrGrantInvalid", name, got)
		}
	}
}

func TestGate_PeerWithNoGraphicAccessRefusedWithoutCallingTheProvider(t *testing.T) {
	// AC-19b.
	p := &countingProvider{}
	g, _ := newGateForTest(t, p)
	ctx := WithCallerSession(context.Background(), CallerSession{GraphicAccess: false})
	d := NewDigestBuilder("login.bootstrap.mint").Build()

	_, err := g.Request(ctx, "login.bootstrap.mint", d, "mint a login code")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v, want ErrNoSession", err)
	}
	if p.callCount() != 0 {
		t.Fatalf("provider was called %d time(s); a session with no graphic access must never reach it", p.callCount())
	}
}

func TestGate_PeerWithGraphicAccessPrompts(t *testing.T) {
	p := &countingProvider{}
	g, _ := newGateForTest(t, p)
	ctx := WithCallerSession(context.Background(), CallerSession{GraphicAccess: true})
	d := NewDigestBuilder("login.bootstrap.mint").Build()

	if _, err := g.Request(ctx, "login.bootstrap.mint", d, "mint a login code"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("provider was called %d time(s), want 1", p.callCount())
	}
}

func TestGate_NoSessionOnContextPrompts(t *testing.T) {
	// AC-19d: no CallerSession attached at all (the WebView IPC, the tray
	// menu, the loopback TCP mux) must prompt, not refuse.
	p := &countingProvider{}
	g, _ := newGateForTest(t, p)
	d := NewDigestBuilder("login.bootstrap.mint").Build()

	if _, err := g.Request(context.Background(), "login.bootstrap.mint", d, "mint a login code"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("provider was called %d time(s), want 1", p.callCount())
	}
}

func TestGate_RequireDoesNotRedeemOnRefusal(t *testing.T) {
	g, _ := newGateForTest(t, &countingProvider{result: ErrRefused})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "x").Build()
	if _, err := g.Require(context.Background(), "credential.mint", d, "mint"); !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	if len(g.nonces) != 0 {
		t.Fatal("a refused prompt must never mint a nonce")
	}
}

func TestGate_NonceTableIsBoundedAndEvictsOldest(t *testing.T) {
	g, _ := newGateForTest(t, &countingProvider{})
	d := NewDigestBuilder("credential.mint").StringField("name", true, "x").Build()

	first, err := g.Request(context.Background(), "credential.mint", d, "mint")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	var last Grant
	for i := 0; i < maxNonces; i++ {
		last, err = g.Request(context.Background(), "credential.mint", d, "mint")
		if err != nil {
			t.Fatalf("Request #%d: %v", i, err)
		}
	}
	if len(g.nonces) > maxNonces {
		t.Fatalf("nonce table grew to %d entries, want at most %d", len(g.nonces), maxNonces)
	}
	if err := g.Redeem(first, "credential.mint", d); !errors.Is(err, ErrGrantInvalid) {
		t.Fatal("the oldest grant survived past the table's bound")
	}
	if err := g.Redeem(last, "credential.mint", d); err != nil {
		t.Fatalf("the most recently minted grant was evicted: %v", err)
	}
}
