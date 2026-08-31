package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// What a challenge flood can and cannot do, pinned so the comment on Issue
// stays true. It CAN keep the owner refused for as long as it runs — there is
// no identity on this route to allocate a slot against, so that is stated
// rather than claimed away. It CANNOT grow the table past its bound, and it
// cannot do any of it silently: the condition warns once per TTL, which is
// both often enough for an operator to see it and rare enough not to become
// the log flood it is warning about.
func TestWebAuthnChallengeFloodIsBoundedAndVisibleOncePerTTL(t *testing.T) {
	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store := newWebAuthnChallengeStore()
	now := time.Now()
	store.now = func() time.Time { return now }

	warnings := func() int {
		return strings.Count(logs.String(), challengeTableFullWarning)
	}
	flood := func(n int) int {
		refused := 0
		for i := 0; i < n; i++ {
			if _, err := store.Issue(WebAuthnCeremonyAssert); err != nil {
				if !errors.Is(err, errChallengeTableFull) {
					t.Fatalf("issue: %v", err)
				}
				refused++
			}
		}
		return refused
	}

	if refused := flood(maxOutstandingChallenges); refused != 0 {
		t.Fatalf("%d of the first %d challenges were refused", refused, maxOutstandingChallenges)
	}
	if got := store.outstanding(); got != maxOutstandingChallenges {
		t.Fatalf("outstanding = %d, want %d", got, maxOutstandingChallenges)
	}

	const floodSize = 500
	if refused := flood(floodSize); refused != floodSize {
		t.Fatalf("%d of %d flood requests were served past the bound", floodSize-refused, floodSize)
	}
	if got := store.outstanding(); got > maxOutstandingChallenges {
		t.Fatalf("a flood grew the table to %d, past its bound of %d", got, maxOutstandingChallenges)
	}
	if got := warnings(); got != 1 {
		t.Fatalf("%d warnings for a %d-request flood inside one TTL, want exactly 1", got, floodSize)
	}

	// The owner arriving mid-flood is refused, and the refusal does not lapse
	// on its own: the flood re-fills the table the moment the TTL retires an
	// entry. This is the cost the comment on Issue now names.
	if _, err := store.Issue(WebAuthnCeremonyRegister); !errors.Is(err, errChallengeTableFull) {
		t.Fatalf("the owner's challenge during a flood: got %v, want %v", err, errChallengeTableFull)
	}
	now = now.Add(challengeTTL + time.Second)
	if refused := flood(maxOutstandingChallenges); refused != 0 {
		t.Fatalf("the flood could not re-fill the table after a TTL: %d refusals", refused)
	}
	if _, err := store.Issue(WebAuthnCeremonyRegister); !errors.Is(err, errChallengeTableFull) {
		t.Fatalf("the owner's challenge a TTL later: got %v, want %v", err, errChallengeTableFull)
	}
	if got := warnings(); got != 2 {
		t.Fatalf("%d warnings across two TTL windows, want exactly 2", got)
	}

	// And when the pressure stops, the table drains and the owner is served
	// again — the refusal is a consequence of the flood and not a latch.
	now = now.Add(challengeTTL + time.Second)
	if _, err := store.Issue(WebAuthnCeremonyRegister); err != nil {
		t.Fatalf("the owner's challenge after the flood stopped: %v", err)
	}
}
