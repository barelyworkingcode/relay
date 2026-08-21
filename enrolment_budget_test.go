package main

// Per-enrolment rate and volume budgets (ADR-010 decision 7).
//
// The assertions that matter here are about *interdiction*, not about
// bookkeeping: a refused call must not have reached the MCP, one enrolment's
// exhaustion must not touch another's allowance, and a local caller must be
// provably untouched by any of it. Window expiry is driven by an injected
// clock — a budget tested with a sleep is a budget tested flakily.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// fakeClock is the injected time source. Guarded, because the concurrency test
// reads it from many goroutines at once.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// budgetFingerprint derives a distinct full-length fingerprint per client id.
// Full length on purpose: the ledger is keyed by the attested certificate, and
// a test that keyed it by a short string would not exercise that.
func budgetFingerprint(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// budgetCtx stands in for the remote listener, attesting an identity the way a
// verified TLS connection does. Nothing here is caller-asserted.
func budgetCtx(clientID string) context.Context {
	return bridge.WithRemoteCaller(context.Background(), bridge.RemoteCaller{
		ClientID:    clientID,
		Fingerprint: budgetFingerprint(clientID),
		RemoteAddr:  "127.0.0.1:52233",
	})
}

// countingMock returns an MCP that answers with result and a thread-safe count
// of how many times it was actually invoked. The count is the whole point of
// several tests below: a refusal that still ran the tool has interdicted
// nothing, and only the invocation count can tell the difference.
func countingMock(result string) (*mockMcpConn, func() int) {
	var mu sync.Mutex
	n := 0
	m := newMockConn("macmcp", simpleTools("mail_search", "mail_get_emails"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return json.RawMessage(result), nil
		})
	return m, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// budgetRouter builds a router whose settings hold one enrolment per entry in
// budgets, stored VERBATIM — no normalization — so a zero budget can be proven
// not to mean "unlimited" on the read path rather than only on the write path.
func budgetRouter(t *testing.T, mock *mockMcpConn, budgets map[string]EnrolmentBudget) (*appRouter, *AuditRecorder, *fakeClock) {
	t.Helper()
	mkSandboxRelayHome(t)

	s := makeSettings(map[string]Permission{"macmcp": PermOn}, nil, nil)
	for clientID, b := range budgets {
		s.Enrolments = append(s.Enrolments, Enrolment{
			ClientID:    clientID,
			Fingerprint: budgetFingerprint(clientID),
			ProjectIDs:  []string{"test-project"},
			Budget:      b,
		})
	}
	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "macmcp", mock)

	r := newTestRouter(t, s, mgr)
	rec := newTestAudit(t, nil)
	r.audit = rec
	clk := newFakeClock()
	r.budgets.setClock(clk.now)
	return r, rec, clk
}

// lastEvent returns the most recent record on disk.
func lastEvent(t *testing.T, rec *AuditRecorder) AuditEvent {
	t.Helper()
	events := readLoggedEvents(t, rec)
	if len(events) == 0 {
		t.Fatal("audit log is empty")
	}
	return events[len(events)-1]
}

// windowCount reports how many enrolments the ledger is tracking. Used to prove
// a local call leaves no trace in it at all.
func windowCount(b *enrolmentBudgets) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.windows)
}

// drawn reports the bytes currently charged to an enrolment's window.
func drawn(b *enrolmentBudgets, clientID string) int64 {
	b.mu.RLock()
	w := b.windows[budgetFingerprint(clientID)]
	b.mu.RUnlock()
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

const budgetResult = `{"content":[{"type":"text","text":"3 messages"}]}`

// ---------------------------------------------------------------------------
// Rate
// ---------------------------------------------------------------------------

func TestEnrolmentBudget_CallWithinBudgetSucceeds(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	r, rec, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 60, MaxCalls: 10, MaxResultBytes: 1 << 20},
	})

	ctx := budgetCtx("hermes-mail")
	for i := 0; i < 3; i++ {
		if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
			t.Fatalf("call %d inside the budget was refused: %v", i+1, err)
		}
	}
	if calls() != 3 {
		t.Errorf("MCP ran %d times, want 3", calls())
	}
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Outcome == AuditOutcomeThrottled {
			t.Errorf("a call inside the budget was logged as throttled: %+v", ev)
		}
	}
}

// The core interdiction. Asserting the third call returned an error is not
// enough: a refusal that happens after the tool ran has read the mailbox it was
// refusing to let anyone read, so the invocation count is the real assertion.
func TestEnrolmentBudget_RateRefusalIsThrottledAndTheMcpNeverRuns(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	r, rec, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 60, MaxCalls: 2, MaxResultBytes: 1 << 20},
	})

	ctx := budgetCtx("hermes-mail")
	for i := 0; i < 2; i++ {
		if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
			t.Fatalf("call %d inside the budget was refused: %v", i+1, err)
		}
	}
	result, err := r.CallTool(ctx, "mail_search", nil, testToken)
	if err == nil {
		t.Fatalf("the call over the rate budget succeeded, returned %s", result)
	}
	if !strings.Contains(err.Error(), "throttled") {
		t.Errorf("refusal should name the reason, got: %v", err)
	}
	if calls() != 2 {
		t.Errorf("MCP ran %d times; a throttled call must never reach it", calls())
	}

	ev := lastEvent(t, rec)
	if ev.Outcome != AuditOutcomeThrottled {
		t.Errorf("outcome = %q, want %q — throttled is the only outcome that says the grant was legitimate and the use was not",
			ev.Outcome, AuditOutcomeThrottled)
	}
	// No MCP was reached, so there is no side effect for an intent record to
	// bracket: the refusal is one record, like a denial.
	if ev.Phase != "" {
		t.Errorf("phase = %q, want a single standalone record", ev.Phase)
	}
	if ev.Actor.Kind != AuditActorRemote || ev.Actor.ClientID != "hermes-mail" {
		t.Errorf("refusal is not attributable: kind=%q client=%q", ev.Actor.Kind, ev.Actor.ClientID)
	}
	if ev.Actor.ProjectID != "test-project" {
		t.Errorf("refusal lost the project grant: %q", ev.Actor.ProjectID)
	}
	if ev.Tool != "mail_search" {
		t.Errorf("refusal lost the tool name: %q", ev.Tool)
	}
}

// ---------------------------------------------------------------------------
// Volume
// ---------------------------------------------------------------------------

// Volume is accounted after the fact, because a result's size is not knowable
// before the MCP answers. The call that crosses the cap therefore COMPLETES and
// the next one is refused. This test asserts exactly that, rather than a
// guarantee the design cannot make.
func TestEnrolmentBudget_VolumeOverrunRefusesTheNextCall(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	// Half a result's worth: the very first call overshoots.
	r, rec, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 60, MaxCalls: 100, MaxResultBytes: int64(len(budgetResult) / 2)},
	})

	ctx := budgetCtx("hermes-mail")
	if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err != nil {
		t.Fatalf("the first call must complete — its size was not knowable in advance: %v", err)
	}
	if got := drawn(&r.budgets, "hermes-mail"); got != int64(len(budgetResult)) {
		t.Errorf("charged %d bytes, want %d (the same quantity the audit log records as ResultBytes)",
			got, len(budgetResult))
	}

	if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err == nil {
		t.Fatal("the call after the volume budget was spent succeeded")
	} else if !strings.Contains(err.Error(), "result bytes") {
		t.Errorf("refusal should name the volume budget, got: %v", err)
	}
	if calls() != 1 {
		t.Errorf("MCP ran %d times, want 1: the second call must be refused before it", calls())
	}
	if ev := lastEvent(t, rec); ev.Outcome != AuditOutcomeThrottled {
		t.Errorf("outcome = %q, want %q", ev.Outcome, AuditOutcomeThrottled)
	}
}

// Rate alone does not stop a slow drain: with a generous call cap and a mean
// byte cap, a client that paces itself is still cut off.
func TestEnrolmentBudget_VolumeBindsEvenWhenTheRateIsFine(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	r, _, clk := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 600, MaxCalls: 1000, MaxResultBytes: int64(2 * len(budgetResult))},
	})

	ctx := budgetCtx("hermes-mail")
	refusedAt := 0
	for i := 1; i <= 10; i++ {
		if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err != nil {
			refusedAt = i
			break
		}
		// One call a minute: nowhere near the rate cap, and still refused.
		clk.advance(time.Minute)
	}
	if refusedAt != 3 {
		t.Errorf("slow drain was refused at call %d, want 3 (two results fill a two-result budget)", refusedAt)
	}
	if calls() != 2 {
		t.Errorf("MCP ran %d times, want 2", calls())
	}
}

// ---------------------------------------------------------------------------
// The window is rolling, not a bucket that resets
// ---------------------------------------------------------------------------

// A fixed bucket hands an attacker 2x the budget by straddling the reset. This
// test pins the rolling behaviour: allowance returns entry by entry as each
// individual call ages out, not all at once on a boundary.
func TestEnrolmentBudget_WindowRollsOverPerCall(t *testing.T) {
	mock, _ := countingMock(budgetResult)
	r, _, clk := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 60, MaxCalls: 2, MaxResultBytes: 1 << 30},
	})
	ctx := budgetCtx("hermes-mail")

	// t+0 and t+30 fill the window.
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clk.advance(30 * time.Second)
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err == nil {
		t.Fatal("third call inside a full window succeeded")
	}

	// Just short of the first call ageing out, it is still refused.
	clk.advance(29 * time.Second)
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err == nil {
		t.Fatal("allowance returned before the window had actually rolled")
	}

	// t+60: the first call has aged out, exactly one slot is free.
	clk.advance(time.Second)
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
		t.Fatalf("call after the window rolled was refused: %v", err)
	}
	// ...and only one. A bucket that reset would have handed back both.
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err == nil {
		t.Fatal("window reset as a fixed bucket: the whole budget came back at once")
	}
}

// Volume ages out of the rolling window too, or a slow drain would be
// permanently locked out by its first big result.
func TestEnrolmentBudget_VolumeAgesOutOfTheWindow(t *testing.T) {
	mock, _ := countingMock(budgetResult)
	r, _, clk := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 60, MaxCalls: 100, MaxResultBytes: int64(len(budgetResult))},
	})
	ctx := budgetCtx("hermes-mail")

	if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err == nil {
		t.Fatal("second call ran with the volume budget already spent")
	}
	clk.advance(60 * time.Second)
	if _, err := r.CallTool(ctx, "mail_get_emails", nil, testToken); err != nil {
		t.Fatalf("volume did not age out of the window: %v", err)
	}
	if got := drawn(&r.budgets, "hermes-mail"); got != int64(len(budgetResult)) {
		t.Errorf("window holds %d bytes after rollover, want only the fresh call's %d", got, len(budgetResult))
	}
}

// ---------------------------------------------------------------------------
// The enrolment is the unit
// ---------------------------------------------------------------------------

// Two agents sharing a grant have independent budgets: the enrolment is the
// unit of compromise, so a compromised one must not be able to consume its
// neighbour's allowance. A project-level cap would fail this test, which is the
// point of it.
func TestEnrolmentBudget_EnrolmentsAreIndependent(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	r, _, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail":   {WindowSeconds: 60, MaxCalls: 1, MaxResultBytes: int64(len(budgetResult))},
		"hermes-triage": {WindowSeconds: 60, MaxCalls: 1, MaxResultBytes: int64(len(budgetResult))},
	})

	// One enrolment burns both of its limits to the floor.
	if _, err := r.CallTool(budgetCtx("hermes-mail"), "mail_search", nil, testToken); err != nil {
		t.Fatalf("first call for hermes-mail: %v", err)
	}
	if _, err := r.CallTool(budgetCtx("hermes-mail"), "mail_search", nil, testToken); err == nil {
		t.Fatal("hermes-mail exceeded its own budget")
	}

	// Its neighbour, holding the same grant, is untouched.
	if _, err := r.CallTool(budgetCtx("hermes-triage"), "mail_search", nil, testToken); err != nil {
		t.Fatalf("one enrolment's exhaustion starved another sharing the grant: %v", err)
	}
	if got := drawn(&r.budgets, "hermes-triage"); got != int64(len(budgetResult)) {
		t.Errorf("hermes-triage drew %d bytes, want only its own %d", got, len(budgetResult))
	}
	if calls() != 2 {
		t.Errorf("MCP ran %d times, want 2 (one admitted call each)", calls())
	}
}

// ---------------------------------------------------------------------------
// Local callers are untouched
// ---------------------------------------------------------------------------

// No remote identity in the context means no budget, no accounting, and no
// ledger entry — one context lookup and nothing else. Budgets bound a remote
// enrolment's blast radius; applying them to the local tray would be a
// throttle on the machine's own owner.
func TestEnrolmentBudget_LocalCallsAreUnbudgeted(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	// A budget so tight that any accounting at all would refuse the second call.
	r, rec, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {WindowSeconds: 3600, MaxCalls: 1, MaxResultBytes: 1},
	})

	for i := 0; i < 20; i++ {
		if _, err := r.CallTool(context.Background(), "mail_search", nil, testToken); err != nil {
			t.Fatalf("local call %d was budgeted: %v", i+1, err)
		}
	}
	if calls() != 20 {
		t.Errorf("MCP ran %d times, want 20", calls())
	}
	if n := windowCount(&r.budgets); n != 0 {
		t.Errorf("local calls created %d ledger entries; they must not be accounted at all", n)
	}
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Outcome == AuditOutcomeThrottled {
			t.Fatalf("a local call was throttled: %+v", ev)
		}
	}
}

// ---------------------------------------------------------------------------
// Zero and absent budgets never read as unlimited
// ---------------------------------------------------------------------------

func TestEnrolmentBudget_ZeroAndAbsentResolveToDefaults(t *testing.T) {
	s := makeSettings(nil, nil, nil)
	s.Enrolments = append(s.Enrolments, Enrolment{
		ClientID:    "hermes-mail",
		Fingerprint: budgetFingerprint("hermes-mail"),
		// Hand-edited settings.json with the budget key stripped.
		Budget: EnrolmentBudget{},
	})
	want := normalizeEnrolmentBudget(EnrolmentBudget{})

	known := bridge.RemoteCaller{ClientID: "hermes-mail", Fingerprint: budgetFingerprint("hermes-mail")}
	if got := s.enrolmentBudget(known); got != want {
		t.Errorf("a zero stored budget resolved to %+v, want the conservative defaults %+v", got, want)
	}
	// Unreachable in practice — the listener closes a connection whose
	// certificate resolves to nothing — which is exactly why it must fail
	// closed rather than exempt.
	unknown := bridge.RemoteCaller{ClientID: "stranger", Fingerprint: budgetFingerprint("stranger")}
	if got := s.enrolmentBudget(unknown); got != want {
		t.Errorf("an unresolved fingerprint resolved to %+v, want the conservative defaults %+v", got, want)
	}
	var nilSettings *Settings
	if got := nilSettings.enrolmentBudget(known); got != want {
		t.Errorf("nil settings resolved to %+v, want the conservative defaults %+v", got, want)
	}
}

// The end-to-end half of the same property: a stored zero budget throttles at
// the default, rather than never.
func TestEnrolmentBudget_ZeroBudgetStillThrottles(t *testing.T) {
	mock, calls := countingMock(budgetResult)
	r, _, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		"hermes-mail": {},
	})

	ctx := budgetCtx("hermes-mail")
	for i := 1; i <= defaultEnrolmentMaxCalls; i++ {
		if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err != nil {
			t.Fatalf("call %d of the default allowance was refused: %v", i, err)
		}
	}
	if _, err := r.CallTool(ctx, "mail_search", nil, testToken); err == nil {
		t.Fatalf("a zero budget read as unlimited: %d calls all succeeded", defaultEnrolmentMaxCalls+1)
	}
	if calls() != defaultEnrolmentMaxCalls {
		t.Errorf("MCP ran %d times, want %d", calls(), defaultEnrolmentMaxCalls)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Several connections from one enrolment can call at once, and the cap has to
// hold across all of them: accounting that is per-goroutine is not a cap. Run
// under -race, which is where a ledger built out of an unguarded map or a
// read-modify-write on a counter shows up.
func TestEnrolmentBudget_ConcurrentCallsAccountExactly(t *testing.T) {
	const (
		maxCalls  = 20
		attempts  = 60
		clientID  = "hermes-mail"
		perResult = len(budgetResult)
	)
	mock, calls := countingMock(budgetResult)
	r, _, _ := budgetRouter(t, mock, map[string]EnrolmentBudget{
		clientID: {WindowSeconds: 3600, MaxCalls: maxCalls, MaxResultBytes: 1 << 30},
	})

	ctx := budgetCtx(clientID)
	var wg sync.WaitGroup
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := r.CallTool(ctx, "mail_search", nil, testToken)
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	admitted, throttled := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			admitted++
		case strings.Contains(err.Error(), "throttled"):
			throttled++
		default:
			t.Fatalf("unexpected error from a concurrent call: %v", err)
		}
	}
	if admitted != maxCalls {
		t.Errorf("%d of %d concurrent calls were admitted, want exactly %d", admitted, attempts, maxCalls)
	}
	if throttled != attempts-maxCalls {
		t.Errorf("%d calls were throttled, want %d", throttled, attempts-maxCalls)
	}
	if calls() != maxCalls {
		t.Errorf("MCP ran %d times, want %d", calls(), maxCalls)
	}
	if got, want := drawn(&r.budgets, clientID), int64(maxCalls*perResult); got != want {
		t.Errorf("ledger holds %d bytes after concurrent calls, want %d", got, want)
	}
}

// Concurrent traffic from many enrolments must stay independent — and must not
// serialise behind one another, which is what the per-enrolment lock buys.
func TestEnrolmentBudget_ConcurrentEnrolmentsStayIndependent(t *testing.T) {
	const enrolments = 8
	mock, calls := countingMock(budgetResult)

	budgets := map[string]EnrolmentBudget{}
	for i := 0; i < enrolments; i++ {
		budgets[fmt.Sprintf("agent-%d", i)] = EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 2, MaxResultBytes: 1 << 30}
	}
	r, _, _ := budgetRouter(t, mock, budgets)

	var wg sync.WaitGroup
	errs := make([]error, enrolments*4)
	for i := 0; i < enrolments; i++ {
		ctx := budgetCtx(fmt.Sprintf("agent-%d", i))
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				_, err := r.CallTool(ctx, "mail_search", nil, testToken)
				errs[slot] = err
			}(i*4 + j)
		}
	}
	wg.Wait()

	admitted := 0
	for _, err := range errs {
		if err == nil {
			admitted++
		}
	}
	// Every enrolment gets its own two, no more and no fewer.
	if admitted != enrolments*2 {
		t.Errorf("%d calls admitted across %d enrolments, want %d", admitted, enrolments, enrolments*2)
	}
	if calls() != enrolments*2 {
		t.Errorf("MCP ran %d times, want %d", calls(), enrolments*2)
	}
	if n := windowCount(&r.budgets); n != enrolments {
		t.Errorf("ledger tracks %d enrolments, want %d", n, enrolments)
	}
}
