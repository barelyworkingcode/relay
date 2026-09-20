package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// polBlockingProvider stands in for a person who has not answered yet: Evaluate
// signals entered, then waits until the test releases it.
type polBlockingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPolBlockingProvider() *polBlockingProvider {
	return &polBlockingProvider{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (p *polBlockingProvider) Evaluate(ctx context.Context, _ string) error {
	p.entered <- struct{}{}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *polBlockingProvider) answer() { p.once.Do(func() { close(p.release) }) }

// polFixture is one queue shared by the op under test and a TemplateOps that
// stands in for every other config mutation.
type polFixture struct {
	store    config.SettingsStore
	queue    *config.CommandQueue
	provider *polBlockingProvider
	gate     *presence.Gate
	audit    *pgrAuditor
	other    *TemplateOps
}

func newPolFixture(t *testing.T) *polFixture {
	t.Helper()
	store := newCLISandboxStore(t)
	queue, err := config.NewCommandQueue(4)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	provider := newPolBlockingProvider()
	t.Cleanup(provider.answer)
	gate, err := presence.NewGate(provider)
	assertNoErr(t, err, "NewGate")
	return &polFixture{store: store, queue: queue, provider: provider, gate: gate, audit: &pgrAuditor{},
		other: &TemplateOps{Store: store, Queue: queue}}
}

func (f *polFixture) services() *ServiceOps {
	return &ServiceOps{Store: f.store, Queue: f.queue, Registry: &sorRaceRegistry{}, Gate: f.gate, Issuance: f.audit}
}

func (f *polFixture) enrolments() *EnrolmentOps {
	return &EnrolmentOps{Store: f.store, Queue: f.queue, Gate: f.gate, Issuance: f.audit}
}

// assertLaneFreeWhilePromptBlocked waits until the gated op is parked on the
// human, then requires another config mutation to complete on the same queue.
// The timeout is only the failure path: a held lane never completes.
func (f *polFixture) assertLaneFreeWhilePromptBlocked(t *testing.T) {
	t.Helper()
	<-f.provider.entered
	done := make(chan error, 1)
	go func() {
		done <- f.other.Create(context.Background(), config.TerminalTemplate{ID: "other-template", Name: "Other"})
	}()
	select {
	case err := <-done:
		assertNoErr(t, err, "config mutation while a presence prompt is pending")
	case <-time.After(5 * time.Second):
		t.Fatal("config lane is held by a pending presence prompt")
	}
	if _, ok := templateByID(f.store, "other-template"); !ok {
		t.Fatal("the concurrent mutation reported success without persisting")
	}
}

func polSeedService(t *testing.T, f *polFixture, command string) {
	t.Helper()
	sorSeedService(t, f.store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: command})
}

func polServiceCommand(f *polFixture) string {
	svc, _ := config.FindServiceByID(f.store.Get(), "svc")
	if svc == nil {
		return ""
	}
	return svc.Command
}

// polRunBehindBlocker starts call, returns once it is parked in the queue
// behind a blocker (so any pre-queue decision is already made), and returns
// finish, which runs mutate, frees the lane and yields call's result.
func polRunBehindBlocker(t *testing.T, f *polFixture, call func() error) (finish func(mutate func()) error) {
	t.Helper()
	release := queueBlocker(t, f.queue)
	done := make(chan error, 1)
	go func() { done <- call() }()
	waitForPending(t, f.queue, 1)
	return func(mutate func()) error {
		mutate()
		release()
		return <-done
	}
}

// --- ServiceOps.Create ---

func TestServiceCreate_PromptDoesNotHoldTheLane(t *testing.T) {
	f := newPolFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.services().Create(context.Background(), serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		done <- err
	}()
	f.assertLaneFreeWhilePromptBlocked(t)
	f.provider.answer()
	assertNoErr(t, <-done, "Create")
	if polServiceCommand(f) != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", polServiceCommand(f))
	}
	if n := len(f.audit.records()); n != 1 {
		t.Fatalf("issuance records = %d, want 1", n)
	}
}

// --- ServiceOps.Update ---

func TestServiceUpdate_PromptDoesNotHoldTheLane(t *testing.T) {
	f := newPolFixture(t)
	polSeedService(t, f, "/bin/old")
	done := make(chan error, 1)
	go func() {
		_, err := f.services().Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		done <- err
	}()
	f.assertLaneFreeWhilePromptBlocked(t)
	f.provider.answer()
	assertNoErr(t, <-done, "Update")
	if polServiceCommand(f) != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", polServiceCommand(f))
	}
	if n := len(f.audit.records()); n != 1 {
		t.Fatalf("issuance records = %d, want 1", n)
	}
}

func TestServiceUpdate_RefusesWhenLiveRecordNeedsApprovalNotObtained(t *testing.T) {
	f := newPolFixture(t)
	f.provider.answer()
	polSeedService(t, f, "/bin/old")
	fail := &ServiceOps{Store: f.store, Queue: f.queue, Registry: &sorRaceRegistry{}, Issuance: f.audit}
	// An unchanged resend needs no prompt against the snapshot.
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := fail.Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: "/bin/old"}, auditViaCLI, "")
		return err
	})
	err := finish(func() {
		sorSeedService(t, f.store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/other"})
	})
	if !errors.Is(err, errServiceChangedDuringApproval) {
		t.Fatalf("Update err = %v, want errServiceChangedDuringApproval", err)
	}
	if got := polServiceCommand(f); got != "/bin/other" {
		t.Fatalf("stored command = %q, want the concurrent /bin/other untouched", got)
	}
	if n := len(f.audit.records()); n != 0 {
		t.Fatalf("issuance records = %d, want none", n)
	}
}

func TestServiceUpdate_CommitsWhenLiveRecordIsCoveredByApproval(t *testing.T) {
	f := newPolFixture(t)
	f.provider.answer()
	polSeedService(t, f, "/bin/old")
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := f.services().Update(context.Background(), "svc", serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		return err
	})
	// The record was narrowed to the requested value meanwhile: over-prompted, still fine.
	assertNoErr(t, finish(func() {
		sorSeedService(t, f.store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/new"})
	}), "Update")
	if got := polServiceCommand(f); got != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", got)
	}
}

// --- ServiceOps.Register ---

func TestServiceRegister_CreatePromptDoesNotHoldTheLane(t *testing.T) {
	f := newPolFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.services().Register(context.Background(), serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		done <- err
	}()
	f.assertLaneFreeWhilePromptBlocked(t)
	f.provider.answer()
	assertNoErr(t, <-done, "Register")
	if polServiceCommand(f) != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", polServiceCommand(f))
	}
}

func TestServiceRegister_UpdatePromptDoesNotHoldTheLane(t *testing.T) {
	f := newPolFixture(t)
	polSeedService(t, f, "/bin/old")
	done := make(chan error, 1)
	go func() {
		_, err := f.services().Register(context.Background(), serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		done <- err
	}()
	f.assertLaneFreeWhilePromptBlocked(t)
	f.provider.answer()
	assertNoErr(t, <-done, "Register")
	if polServiceCommand(f) != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", polServiceCommand(f))
	}
}

func TestServiceRegister_RefusesWhenRecordVanishesAfterUnpromptedUpdateDecision(t *testing.T) {
	f := newPolFixture(t)
	polSeedService(t, f, "/bin/old")
	noGate := &ServiceOps{Store: f.store, Queue: f.queue, Registry: &sorRaceRegistry{}, Issuance: f.audit}
	// Snapshot says: existing record, unchanged resend, no prompt.
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := noGate.Register(context.Background(), serviceFields{DisplayName: "Svc", Command: "/bin/old"}, auditViaCLI, "")
		return err
	})
	// Live says: gone, so this would be a create, which always needs approval.
	err := finish(func() {
		assertNoErr(t, f.store.With(func(s *config.Settings) { s.RemoveService("svc") }), "remove")
	})
	if !errors.Is(err, errServiceChangedDuringApproval) {
		t.Fatalf("Register err = %v, want errServiceChangedDuringApproval", err)
	}
	if polServiceCommand(f) != "" {
		t.Fatal("a create was committed without an approval")
	}
}

func TestServiceRegister_CreateApprovalCoversRecordAppearingMeanwhile(t *testing.T) {
	f := newPolFixture(t)
	f.provider.answer()
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := f.services().Register(context.Background(), serviceFields{DisplayName: "Svc", Command: "/bin/new"}, auditViaCLI, "")
		return err
	})
	assertNoErr(t, finish(func() { polSeedService(t, f, "/bin/old") }), "Register")
	if got := polServiceCommand(f); got != "/bin/new" {
		t.Fatalf("stored command = %q, want /bin/new", got)
	}
}

// --- EnrolmentOps.SetRemoteConfig ---

func TestSetRemoteConfig_PromptDoesNotHoldTheLane(t *testing.T) {
	f := newPolFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.enrolments().SetRemoteConfig(context.Background(),
			remoteConfigFields{Enabled: true, Listen: "127.0.0.1:9910"}, auditViaCLI, "")
		done <- err
	}()
	f.assertLaneFreeWhilePromptBlocked(t)
	f.provider.answer()
	assertNoErr(t, <-done, "SetRemoteConfig")
	if r := f.store.Get().Remote; r == nil || r.Listen != "127.0.0.1:9910" {
		t.Fatalf("remote config = %+v, want listen 127.0.0.1:9910", r)
	}
	if n := len(f.audit.records()); n != 1 {
		t.Fatalf("issuance records = %d, want 1", n)
	}
}

func TestSetRemoteConfig_RefusesWhenRemoveBecomesGatedDuringApproval(t *testing.T) {
	f := newPolFixture(t)
	ops := &EnrolmentOps{Store: f.store, Queue: f.queue, Issuance: f.audit}
	// No remote block: Remove needs no prompt against the snapshot.
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := ops.SetRemoteConfig(context.Background(), remoteConfigFields{Remove: true}, auditViaCLI, "")
		return err
	})
	err := finish(func() {
		assertNoErr(t, f.store.With(func(s *config.Settings) {
			s.Remote = &config.RemoteConfig{Listen: "127.0.0.1:9910", Enabled: boolPtr(true)}
		}), "seed remote")
	})
	if !errors.Is(err, errRemoteConfigChangedDuringApproval) {
		t.Fatalf("Remove err = %v, want errRemoteConfigChangedDuringApproval", err)
	}
	if f.store.Get().Remote == nil {
		t.Fatal("a Remove cleared the remote block without an approval")
	}
	if n := len(f.audit.records()); n != 0 {
		t.Fatalf("issuance records = %d, want none", n)
	}
}

func TestSetRemoteConfig_RefusesWhenUnchangedResendBecomesAChange(t *testing.T) {
	f := newPolFixture(t)
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		s.Remote = &config.RemoteConfig{Listen: "127.0.0.1:9910", Enabled: boolPtr(true)}
	}), "seed remote")
	ops := &EnrolmentOps{Store: f.store, Queue: f.queue, Issuance: f.audit}
	finish := polRunBehindBlocker(t, f, func() error {
		_, err := ops.SetRemoteConfig(context.Background(), remoteConfigFields{Enabled: true, Listen: "127.0.0.1:9910"}, auditViaCLI, "")
		return err
	})
	err := finish(func() {
		assertNoErr(t, f.store.With(func(s *config.Settings) { s.Remote.Listen = "127.0.0.1:9911" }), "move listener")
	})
	if !errors.Is(err, errRemoteConfigChangedDuringApproval) {
		t.Fatalf("err = %v, want errRemoteConfigChangedDuringApproval", err)
	}
	if got := f.store.Get().Remote.Listen; got != "127.0.0.1:9911" {
		t.Fatalf("listen = %q, want the concurrent value untouched", got)
	}
}

func TestSetRemoteConfig_RefusesWhenRemoveApprovalMeetsDifferentDigest(t *testing.T) {
	// An approval for one digest never covers a live decision bound to another.
	// Nothing can flip an update into a remove, so the comparison is pinned directly.
	approved := remoteConfigFields{Enabled: true, Listen: "127.0.0.1:9910"}.presenceDigest("127.0.0.1:9910", "")
	live := decideRemoteGate(&config.RemoteConfig{Listen: "x"}, remoteConfigFields{Remove: true}, "", "")
	if !live.needGate || approved.Equal(live.digest) {
		t.Fatal("remove and update decisions must bind different digests")
	}
}

func TestSetRemoteConfig_UnchangedResendNeedsNoPrompt(t *testing.T) {
	f := newPolFixture(t)
	assertNoErr(t, f.store.With(func(s *config.Settings) {
		s.Remote = &config.RemoteConfig{Listen: "127.0.0.1:9910", Enabled: boolPtr(true)}
	}), "seed remote")
	ops := &EnrolmentOps{Store: f.store, Queue: f.queue, Issuance: f.audit}
	_, err := ops.SetRemoteConfig(context.Background(), remoteConfigFields{Enabled: true, Listen: "127.0.0.1:9910"}, auditViaCLI, "")
	assertNoErr(t, err, "SetRemoteConfig")
}

// --- doors ---

func TestChangedDuringApprovalMapsTo409(t *testing.T) {
	if got := serviceHTTPStatus(fmt.Errorf("%w: svc", errServiceChangedDuringApproval)); got != http.StatusConflict {
		t.Fatalf("serviceHTTPStatus = %d, want 409", got)
	}
	if got := enrolmentHTTPStatus(errRemoteConfigChangedDuringApproval); got != http.StatusConflict {
		t.Fatalf("enrolmentHTTPStatus = %d, want 409", got)
	}
}
