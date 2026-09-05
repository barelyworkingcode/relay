package presencetest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

func TestAllow_Succeeds(t *testing.T) {
	if err := presencetest.Allow().Evaluate(context.Background(), "do a thing"); err != nil {
		t.Fatalf("Allow().Evaluate = %v, want nil", err)
	}
}

func TestDeny_ReturnsTheRefusalSentinel(t *testing.T) {
	// A gated core switches on presence.ErrRefused specifically, so the
	// fake must return exactly that value, not merely "some error".
	err := presencetest.Deny().Evaluate(context.Background(), "do a thing")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("Deny().Evaluate = %v, want presence.ErrRefused", err)
	}
}

func TestNoSession_ReturnsTheUnavailableSentinel(t *testing.T) {
	err := presencetest.NoSession().Evaluate(context.Background(), "do a thing")
	if !errors.Is(err, presence.ErrUnavailable) {
		t.Fatalf("NoSession().Evaluate = %v, want presence.ErrUnavailable", err)
	}
}

func TestRecording_CountsCallsAndCapturesReasons(t *testing.T) {
	r := presencetest.NewRecording(nil)
	if r.Calls() != 0 {
		t.Fatalf("Calls() = %d before any Evaluate, want 0", r.Calls())
	}
	if err := r.Evaluate(context.Background(), "mint a credential"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if err := r.Evaluate(context.Background(), "revoke an enrolment"); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if r.Calls() != 2 {
		t.Fatalf("Calls() = %d, want 2", r.Calls())
	}
	got := r.Reasons()
	want := []string{"mint a credential", "revoke an enrolment"}
	if len(got) != len(want) {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Reasons()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGate_NeverReachesTheProviderOnRefusal is the shape AC-19b and AC-26b
// need from the rest of the suite: a Recording fake proves a refusal
// happened WITHOUT a prompt, by asserting zero calls rather than trusting
// that the refusal implies it.
func TestGate_NeverReachesTheProviderOnRefusal(t *testing.T) {
	r := presencetest.NewRecording(nil)
	g, err := presence.NewGate(r)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	ctx := presence.WithCallerSession(context.Background(), presence.CallerSession{GraphicAccess: false})
	d := presence.NewDigestBuilder("credential.mint").StringField("name", true, "x").Build()

	if _, err := g.Request(ctx, "credential.mint", d, "mint a credential"); !errors.Is(err, presence.ErrNoSession) {
		t.Fatalf("Request = %v, want presence.ErrNoSession", err)
	}
	if r.Calls() != 0 {
		t.Fatalf("provider was called %d time(s); a peer with no graphic access must never reach it", r.Calls())
	}
}
