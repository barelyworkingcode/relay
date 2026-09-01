package main

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"testing"
	"time"
)

func TestConsumeBootstrapCode_VerifiesOnceThenRefusesReplay(t *testing.T) {
	store := newCLISandboxStore(t)

	var plaintext string
	err := store.With(func(s *config.Settings) {
		var mintErr error
		plaintext, mintErr = mintBootstrapCode(s)
		assertNoErr(t, mintErr, "mintBootstrapCode")
	})
	assertNoErr(t, err, "store.With mint")

	err = store.With(func(s *config.Settings) {
		if cErr := consumeBootstrapCode(s, plaintext); cErr != nil {
			t.Fatalf("first consume: got %v, want nil", cErr)
		}
	})
	assertNoErr(t, err, "store.With consume 1")

	err = store.With(func(s *config.Settings) {
		if cErr := consumeBootstrapCode(s, plaintext); cErr != errBootstrapCodeInvalid {
			t.Fatalf("replay: got %v, want errBootstrapCodeInvalid", cErr)
		}
	})
	assertNoErr(t, err, "store.With consume 2")
}

// TestConsumeBootstrapCode_ExpiredWrongAndAbsentAreIdentical is the oracle
// check: an absent record, an expired one, and a wrong plaintext must be
// the SAME error value, not merely three non-nil errors, or the anchor
// leaks which of the three happened to a caller with no access to the
// config dir (ADR-016 decision 2).
func TestConsumeBootstrapCode_ExpiredWrongAndAbsentAreIdentical(t *testing.T) {
	sAbsent := &config.Settings{}
	errAbsent := consumeBootstrapCode(sAbsent, "whatever")

	sExpired := &config.Settings{}
	plaintext, err := mintBootstrapCode(sExpired)
	assertNoErr(t, err, "mintBootstrapCode (expired case)")
	sExpired.LoginBootstrap.Expires = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	errExpired := consumeBootstrapCode(sExpired, plaintext)

	sWrong := &config.Settings{}
	_, err = mintBootstrapCode(sWrong)
	assertNoErr(t, err, "mintBootstrapCode (wrong-code case)")
	errWrong := consumeBootstrapCode(sWrong, "not-the-real-code")

	if errAbsent != errBootstrapCodeInvalid {
		t.Fatalf("absent record: got %v, want errBootstrapCodeInvalid", errAbsent)
	}
	if errExpired != errBootstrapCodeInvalid {
		t.Fatalf("expired record: got %v, want errBootstrapCodeInvalid", errExpired)
	}
	if errWrong != errBootstrapCodeInvalid {
		t.Fatalf("wrong code: got %v, want errBootstrapCodeInvalid", errWrong)
	}
	if errAbsent != errExpired || errExpired != errWrong {
		t.Fatalf("the three refusals are not identical: absent=%v expired=%v wrong=%v", errAbsent, errExpired, errWrong)
	}
}

func TestMintBootstrapCode_ReplacesRatherThanAccumulates(t *testing.T) {
	s := &config.Settings{}
	first, err := mintBootstrapCode(s)
	assertNoErr(t, err, "mint 1")
	firstHash := s.LoginBootstrap.Hash

	second, err := mintBootstrapCode(s)
	assertNoErr(t, err, "mint 2")

	if first == second {
		t.Fatalf("two mints produced the same plaintext")
	}
	if s.LoginBootstrap.Hash == firstHash {
		t.Fatalf("a second mint did not replace the first record's hash")
	}

	if cErr := consumeBootstrapCode(s, first); cErr != errBootstrapCodeInvalid {
		t.Fatalf("the code from before a re-mint still verifies: got %v, want errBootstrapCodeInvalid", cErr)
	}
	if cErr := consumeBootstrapCode(s, second); cErr != nil {
		t.Fatalf("the current code failed to verify: %v", cErr)
	}
}

func TestConsumeBootstrapCode_WrongCodeDoesNotConsumeTheRealOne(t *testing.T) {
	s := &config.Settings{}
	real, err := mintBootstrapCode(s)
	assertNoErr(t, err, "mint")

	if cErr := consumeBootstrapCode(s, "totally-wrong-guess"); cErr != errBootstrapCodeInvalid {
		t.Fatalf("wrong code: got %v, want errBootstrapCodeInvalid", cErr)
	}
	if s.LoginBootstrap == nil {
		t.Fatalf("a wrong guess deleted the real record")
	}
	if cErr := consumeBootstrapCode(s, real); cErr != nil {
		t.Fatalf("the real code no longer verifies after a wrong guess: %v", cErr)
	}
}

// TestMintBootstrapCode_CrossProcessConsumable exercises the whole point of
// running consumeBootstrapCode inside the CALLER's store.With rather than
// having it read the store itself: `relay login enrol` and the tray serving
// /relay/login/verify are different processes, and a code minted by one
// FileSettingsStore must be consumable through a second over the same
// directory with no restart and no poll interval (issue #21's guarantee,
// applied here).
func TestMintBootstrapCode_CrossProcessConsumable(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	storeA := sealedSettingsStoreAt(dir)
	assertNoErr(t, storeA.EnsureInitialized(), "EnsureInitialized")

	var plaintext string
	err := storeA.With(func(s *config.Settings) {
		var mintErr error
		plaintext, mintErr = mintBootstrapCode(s)
		assertNoErr(t, mintErr, "mintBootstrapCode")
	})
	assertNoErr(t, err, "store A mint")

	storeB := sealedSettingsStoreAt(dir)
	err = storeB.With(func(s *config.Settings) {
		if cErr := consumeBootstrapCode(s, plaintext); cErr != nil {
			t.Fatalf("cross-process consume: %v", cErr)
		}
	})
	assertNoErr(t, err, "store B consume")

	storeC := sealedSettingsStoreAt(dir)
	if storeC.Get().LoginBootstrap != nil {
		t.Fatalf("bootstrap record survived consumption on disk")
	}
}
