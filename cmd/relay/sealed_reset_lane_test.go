package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// srHookKeyring lets a test park the reset inside its destructive step.
type srHookKeyring struct {
	sealed.Keyring
	onDestroy func()
}

func (k *srHookKeyring) Destroy() error {
	if k.onDestroy != nil {
		k.onDestroy()
	}
	return k.Keyring.Destroy()
}

// srFuncProvider answers a presence prompt by running fn, so a test can act
// while the "human" is deciding.
type srFuncProvider func(ctx context.Context) error

func (f srFuncProvider) Evaluate(ctx context.Context, _ string) error { return f(ctx) }

func srQueue(t *testing.T) *config.CommandQueue {
	t.Helper()
	queue, err := config.NewCommandQueue(4)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	return queue
}

func srGate(t *testing.T, p presence.Provider) *presence.Gate {
	t.Helper()
	gate, err := presence.NewGate(p)
	assertNoErr(t, err, "NewGate")
	return gate
}

func TestResetSealedStore_PromptDoesNotHoldTheLane(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	queue := srQueue(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	answer := func() { once.Do(func() { close(release) }) }
	t.Cleanup(answer)
	gate := srGate(t, srFuncProvider(func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	resetDone := make(chan error, 1)
	go func() { resetDone <- resetSealedStore(context.Background(), dir, store, keyring, gate, queue) }()
	<-entered

	other := &TemplateOps{Store: store, Queue: queue}
	done := make(chan error, 1)
	go func() { done <- other.Create(context.Background(), config.TerminalTemplate{ID: "t1", Name: "T"}) }()
	select {
	case err := <-done:
		assertNoErr(t, err, "config command while the reset prompt is pending")
	case <-time.After(5 * time.Second):
		t.Fatal("the reset prompt holds the config lane")
	}
	answer()
	assertNoErr(t, <-resetDone, "resetSealedStore")
}

func TestResetSealedStore_QueuedWriteNeverInterleavesWithTheStep(t *testing.T) {
	dir, store, base := srSetup(t, "aaaaaaaaaaaaaaaa")
	queue := srQueue(t)
	inStep := make(chan struct{})
	release := make(chan struct{})
	keyring := &srHookKeyring{Keyring: base, onDestroy: func() {
		close(inStep)
		<-release
	}}
	gate := srGate(t, presencetest.Allow())

	resetDone := make(chan error, 1)
	go func() { resetDone <- resetSealedStore(context.Background(), dir, store, keyring, gate, queue) }()
	<-inStep // settings.json and the CA are already deleted; the key still is not

	other := &TemplateOps{Store: store, Queue: queue}
	writeDone := make(chan error, 1)
	go func() { writeDone <- other.Create(context.Background(), config.TerminalTemplate{ID: "t1", Name: "T"}) }()
	waitForPending(t, queue, 1) // admitted behind the running step, not run inside it
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("a queued write touched settings.json mid-reset (stat err=%v)", err)
	}

	close(release)
	assertNoErr(t, <-resetDone, "resetSealedStore")
	assertNoErr(t, <-writeDone, "queued write behind the reset")

	// The write ran strictly after the whole step, under the fresh key.
	if _, ok := templateByID(store, "t1"); !ok {
		t.Fatal("the write queued behind the reset did not land on the post-reset store")
	}
	newID, _, err := base.Load()
	assertNoErr(t, err, "keyring.Load")
	if got := store.Get().SealedKeyID; got != newID || got == "aaaaaaaaaaaaaaaa" {
		t.Fatalf("post-reset settings key id = %q, keychain = %q; want the same fresh id", got, newID)
	}
}

func TestResetSealedStore_KeysChangedDuringApprovalCommitsNothing(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	queue := srQueue(t)
	before := sdRead(t, dir)
	gate := srGate(t, srFuncProvider(func(context.Context) error {
		// The keychain item is replaced while the operator is deciding.
		assertNoErr(t, keyring.Destroy(), "swap key: destroy")
		_, _, err := keyring.Create()
		assertNoErr(t, err, "swap key: create")
		return nil
	}))

	err := resetSealedStore(context.Background(), dir, store, keyring, gate, queue)
	if !errors.Is(err, errSealedKeysChangedDuringApproval) {
		t.Fatalf("err = %v, want errSealedKeysChangedDuringApproval", err)
	}
	if string(sdRead(t, dir)) != string(before) {
		t.Error("settings.json changed although the reset was refused")
	}
	for _, name := range []string{enrolment.CAKeySealedFile, enrolment.CACertFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed although the reset was refused: %v", name, err)
		}
	}
	if id, _, err := keyring.Load(); err != nil || id == "" {
		t.Errorf("keychain item was destroyed by a refused reset (id=%q err=%v)", id, err)
	}
}

func TestResetSealedStore_UnchangedKeysResetsThroughTheQueue(t *testing.T) {
	dir, store, keyring := srSetup(t, "aaaaaaaaaaaaaaaa")
	queue := srQueue(t)
	assertNoErr(t, resetSealedStore(context.Background(), dir, store, keyring, srGate(t, presencetest.Allow()), queue), "resetSealedStore")
	if got := store.Get(); len(got.Projects) != 0 || got.SealedKeyID == "" || got.SealedKeyID == "aaaaaaaaaaaaaaaa" {
		t.Fatalf("store after reset: projects=%d key id=%q", len(got.Projects), got.SealedKeyID)
	}
}
