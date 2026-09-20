package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/login/loginfake"
)

func newLoginQueue(t *testing.T, capacity int) *config.CommandQueue {
	t.Helper()
	queue, err := config.NewCommandQueue(capacity)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	return queue
}

type responseResult struct {
	status int
	body   []byte
}

func TestLoginRouteRegisterWaitsBehindQueuedCommandAndLandsInOrder(t *testing.T) {
	queue := newLoginQueue(t, 4)
	s := lrNewServerWith(t, queue, nil)
	code := s.mintCode()
	a := loginfake.NewSoftAuthenticator(t)

	releaseBlocker := queueBlocker(t, queue)
	registered := make(chan responseResult, 1)
	go func() {
		resp, body := s.register(a, code, 1)
		registered <- responseResult{resp.StatusCode, body}
	}()
	waitForMutationAdmission(t, queue)
	if got := len(s.store.Get().Passkeys); got != 0 {
		t.Fatalf("register persisted %d passkeys before the queue admitted it", got)
	}

	seen := make(chan int, 1)
	go func() {
		_ = queue.Do(context.Background(), func(context.Context) error {
			seen <- len(s.store.Get().Passkeys)
			return nil
		})
	}()
	waitForPending(t, queue, 2)
	releaseBlocker()

	if r := <-registered; r.status != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", r.status, r.body)
	}
	if got := <-seen; got != 1 {
		t.Fatalf("a command queued after the registration saw %d passkeys, want 1", got)
	}
	if s.store.Get().LoginBootstrap != nil {
		t.Fatal("the bootstrap code survived the registration that consumed it")
	}
}

func TestLoginRouteAssertWaitsBehindQueuedCommandAndLandsInOrder(t *testing.T) {
	queue := newLoginQueue(t, 4)
	s := lrNewServerWith(t, queue, nil)
	a := loginfake.NewSoftAuthenticator(t)
	if resp, body := s.register(a, s.mintCode(), 1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}

	baseline := len(s.store.Get().APICredentials)
	releaseBlocker := queueBlocker(t, queue)
	signedIn := make(chan responseResult, 1)
	go func() {
		resp, body := s.assert(a, 7)
		signedIn <- responseResult{resp.StatusCode, body}
	}()
	waitForMutationAdmission(t, queue)
	if n := len(s.store.Get().APICredentials); n != baseline {
		t.Fatalf("assert left %d credentials (was %d) before the queue admitted it", n, baseline)
	}
	if got := s.store.Get().Passkeys[0].SignCount; got != 1 {
		t.Fatalf("sign count advanced to %d before the queue admitted the assertion", got)
	}

	seen := make(chan int, 1)
	go func() {
		_ = queue.Do(context.Background(), func(context.Context) error {
			seen <- len(s.store.Get().APICredentials)
			return nil
		})
	}()
	waitForPending(t, queue, 2)
	releaseBlocker()

	if r := <-signedIn; r.status != http.StatusOK {
		t.Fatalf("assert: status %d, body %s", r.status, r.body)
	}
	if got := <-seen; got != baseline+1 {
		t.Fatalf("a command queued after the assertion saw %d credentials, want %d", got, baseline+1)
	}
	if got := s.store.Get().Passkeys[0].SignCount; got != 7 {
		t.Fatalf("sign count = %d, want 7", got)
	}
}

// gatedFailingIssuance parks inside RecordIssuance, off the lane, so a test
// can occupy the queue between the registration commit and its undo.
type gatedFailingIssuance struct {
	entered chan struct{}
	proceed chan struct{}
}

func (g *gatedFailingIssuance) RecordIssuance(audit.CredentialIssuance) error {
	close(g.entered)
	<-g.proceed
	return errors.New("audit log unavailable")
}

func TestLoginRouteUndoOfUnrecordedPasskeyRunsOnTheQueue(t *testing.T) {
	queue := newLoginQueue(t, 4)
	issuance := &gatedFailingIssuance{entered: make(chan struct{}), proceed: make(chan struct{})}
	s := lrNewServerWith(t, queue, issuance)
	code := s.mintCode()
	a := loginfake.NewSoftAuthenticator(t)

	registered := make(chan responseResult, 1)
	go func() {
		resp, body := s.register(a, code, 1)
		registered <- responseResult{resp.StatusCode, body}
	}()
	<-issuance.entered
	if got := len(s.store.Get().Passkeys); got != 1 {
		t.Fatalf("passkeys before the undo = %d, want 1", got)
	}

	releaseBlocker := queueBlocker(t, queue)
	close(issuance.proceed)
	waitForMutationAdmission(t, queue)
	if got := len(s.store.Get().Passkeys); got != 1 {
		t.Fatalf("the undo removed the passkey before the queue admitted it (%d left)", got)
	}
	releaseBlocker()

	if r := <-registered; r.status != http.StatusInternalServerError {
		t.Fatalf("register with a failing audit log: status %d, body %s", r.status, r.body)
	}
	if got := len(s.store.Get().Passkeys); got != 0 {
		t.Fatalf("unrecorded passkey survived the undo: %d left", got)
	}
}

func TestLoginOpsPasskeyWritesRefuseWithoutQueueSideEffects(t *testing.T) {
	queue := newLoginQueue(t, 1)
	store := eveOpsStore(t)
	ops := &LoginOps{Store: store, Queue: queue}

	refusal, saveErr := ops.RegisterPasskey(context.Background(), config.Passkey{ID: "p1"}, "no-such-code")
	if !errors.Is(refusal, errBootstrapCodeInvalid) || saveErr == nil {
		t.Fatalf("refusal = %v, saveErr = %v; want errBootstrapCodeInvalid and the declined write reported", refusal, saveErr)
	}
	if len(store.Get().Passkeys) != 0 {
		t.Fatal("a refused registration wrote a passkey")
	}
}
