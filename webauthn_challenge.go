package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// WebAuthnCeremony starts at 1 deliberately: a zero value is not a ceremony,
// so a caller that forgets to name one matches no stored challenge rather
// than matching whichever ceremony happens to be the zero constant.
type WebAuthnCeremony uint8

const (
	WebAuthnCeremonyRegister WebAuthnCeremony = iota + 1
	WebAuthnCeremonyAssert
)

func (c WebAuthnCeremony) clientDataType() string {
	switch c {
	case WebAuthnCeremonyRegister:
		return "webauthn.create"
	case WebAuthnCeremonyAssert:
		return "webauthn.get"
	}
	return ""
}

func (c WebAuthnCeremony) String() string {
	switch c {
	case WebAuthnCeremonyRegister:
		return "registration"
	case WebAuthnCeremonyAssert:
		return "assertion"
	}
	return "unknown"
}

const (
	challengeLength          = 32
	challengeTTL             = 60 * time.Second
	maxOutstandingChallenges = 64
)

var errChallengeTableFull = errors.New("too many login ceremonies in flight")

type webauthnChallengeEntry struct {
	ceremony WebAuthnCeremony
	expires  time.Time
}

type WebAuthnChallengeStore struct {
	mu      sync.Mutex
	entries map[string]webauthnChallengeEntry
	now     func() time.Time
	rand    io.Reader
	max     int
	ttl     time.Duration
}

func newWebAuthnChallengeStore() *WebAuthnChallengeStore {
	return &WebAuthnChallengeStore{
		entries: make(map[string]webauthnChallengeEntry),
		now:     time.Now,
		rand:    rand.Reader,
		max:     maxOutstandingChallenges,
		ttl:     challengeTTL,
	}
}

// Issue refuses rather than evicting a live entry when the table is full:
// evicting the oldest would let an unauthenticated flood displace the
// owner's in-flight challenge silently, and a refusal the caller can see
// lasts at most one TTL.
func (s *WebAuthnChallengeStore) Issue(ceremony WebAuthnCeremony) ([]byte, error) {
	if ceremony.clientDataType() == "" {
		return nil, fmt.Errorf("unknown ceremony %d", ceremony)
	}
	challenge := make([]byte, challengeLength)
	if _, err := io.ReadFull(s.rand, challenge); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.entries) >= s.max {
		s.evictExpiredLocked(now)
	}
	if len(s.entries) >= s.max {
		return nil, fmt.Errorf("%w: %d outstanding", errChallengeTableFull, len(s.entries))
	}
	s.entries[string(challenge)] = webauthnChallengeEntry{
		ceremony: ceremony,
		expires:  now.Add(s.ttl),
	}
	return challenge, nil
}

// consume is lookup-and-delete under one lock, so an unknown challenge, an
// expired one, one already spent and one issued for the other ceremony are
// one answer to the caller and a replay races nothing.
func (s *WebAuthnChallengeStore) consume(ceremony WebAuthnCeremony, challenge []byte) bool {
	if len(challenge) != challengeLength {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[string(challenge)]
	if !ok {
		return false
	}
	delete(s.entries, string(challenge))
	if entry.ceremony != ceremony {
		return false
	}
	return s.now().Before(entry.expires)
}

func (s *WebAuthnChallengeStore) evictExpiredLocked(now time.Time) {
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
}

func (s *WebAuthnChallengeStore) outstanding() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

const (
	ceremonyFailureGrace = 3
	ceremonyFailureDelay = 2 * time.Second
	ceremonyMaxDelay     = 30 * time.Second
	ceremonyFailureDecay = 5 * time.Minute
)

var errWebAuthnRateLimited = errors.New("too many failed login ceremonies")

type webauthnRateLimitedError struct {
	RetryAfter time.Duration
}

func (e *webauthnRateLimitedError) Error() string {
	return fmt.Sprintf("%s: retry after %s", errWebAuthnRateLimited, e.RetryAfter.Round(time.Second))
}

func (e *webauthnRateLimitedError) Unwrap() error { return errWebAuthnRateLimited }

// ceremonyLimiter returns the delay it wants rather than sleeping: a verifier
// that slept would hold a handler goroutine per attempt, which hands the
// unauthenticated caller a cheaper denial of service than the one being
// rate-limited.
type ceremonyLimiter struct {
	mu          sync.Mutex
	failures    int
	nextAllowed time.Time
	lastFailure time.Time
	now         func() time.Time
}

func newCeremonyLimiter() *ceremonyLimiter {
	return &ceremonyLimiter{now: time.Now}
}

func (l *ceremonyLimiter) allow() (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.lastFailure.IsZero() && now.Sub(l.lastFailure) >= ceremonyFailureDecay {
		l.failures = 0
		l.nextAllowed = time.Time{}
	}
	if now.Before(l.nextAllowed) {
		return l.nextAllowed.Sub(now), false
	}
	return 0, true
}

func (l *ceremonyLimiter) recordFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.failures++
	l.lastFailure = now
	over := l.failures - ceremonyFailureGrace
	if over <= 0 {
		return
	}
	delay := time.Duration(over) * ceremonyFailureDelay
	if delay > ceremonyMaxDelay {
		delay = ceremonyMaxDelay
	}
	l.nextAllowed = now.Add(delay)
}

func (l *ceremonyLimiter) recordSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = 0
	l.nextAllowed = time.Time{}
	l.lastFailure = time.Time{}
}
