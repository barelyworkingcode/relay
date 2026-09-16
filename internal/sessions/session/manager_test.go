package session_test

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func newTestManager(t *testing.T) (*session.Manager, *session.Store) {
	t.Helper()
	store := session.NewStore(t.TempDir())
	mgr := session.NewManager(session.Config{}, store, nil)
	return mgr, store
}

func factoryReturning(p sessionstypes.Provider) func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
	return func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return p, nil
	}
}

func TestManager_Create_SessionExists(t *testing.T) {
	mgr, _ := newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))

	spec := session.CreateSpec{SessionID: "sess-1", ProjectID: "proj-1", Kind: session.KindClaude}
	if _, err := mgr.Create(spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := mgr.Create(spec)
	if !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("second Create error = %v, want ErrSessionExists", err)
	}
}

// TestManager_Create_ConcurrentDuplicate_NoOrphan is the R-S6-precedent
// regression test: many goroutines racing Create for the same session id
// must never let more than one provider actually spawn. R-S6's reviewer
// found and required a fix for exactly this bug shape (a second spawn
// orphaned outside the manager's own tracking table) in the sibling
// terminal package; this asserts the same atomic-reservation property holds
// here.
func TestManager_Create_ConcurrentDuplicate_NoOrphan(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	const n = 25
	spec := session.CreateSpec{SessionID: "dup", ProjectID: "proj-1", Kind: session.KindClaude}
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mgr.Create(spec)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var okCount, existsCount int
	for err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, session.ErrSessionExists):
			existsCount++
		default:
			t.Fatalf("unexpected Create error: %v", err)
		}
	}
	if okCount != 1 {
		t.Fatalf("successful Creates = %d, want 1", okCount)
	}
	if existsCount != n-1 {
		t.Fatalf("ErrSessionExists count = %d, want %d", existsCount, n-1)
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("provider Start count = %d, want exactly 1 (a second value means an orphaned spawn)", got)
	}
}

// TestManager_StopAll_WaitsForInFlightLaunch is the R-S6-precedent
// regression test for StopAll's own second review-round finding: it must
// not return while a launch still in flight has a process StopAll is
// responsible for.
func TestManager_StopAll_WaitsForInFlightLaunch(t *testing.T) {
	mgr, _ := newTestManager(t)
	gate := make(chan struct{})
	fp := &fakeProvider{startGate: gate}
	mgr.SetProviderFactory(factoryReturning(fp))

	createDone := make(chan error, 1)
	go func() {
		_, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
		createDone <- err
	}()

	// Let Create reserve the slot and block inside Start on the gate.
	time.Sleep(50 * time.Millisecond)

	stopAllDone := make(chan struct{})
	go func() {
		mgr.StopAll()
		close(stopAllDone)
	}()

	select {
	case <-stopAllDone:
		t.Fatal("StopAll returned before the in-flight launch's provider was killed")
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)

	select {
	case err := <-createDone:
		if err == nil {
			t.Fatal("Create: want an error (closed while starting), got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create never returned after the gate was released")
	}

	select {
	case <-stopAllDone:
	case <-time.After(2 * time.Second):
		t.Fatal("StopAll did not return after the in-flight launch resolved")
	}

	if fp.Kills() != 1 {
		t.Fatalf("provider kill count = %d, want 1", fp.Kills())
	}
}

func TestManager_SendMessage_ProjectSessionDeadProvider_ResumeRequired(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	err = mgr.SendMessage("s1", "hello", nil)
	if !errors.Is(err, session.ErrResumeRequired) {
		t.Fatalf("SendMessage error = %v, want ErrResumeRequired", err)
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("provider Start count = %d, want 1 (SH-6: no host-driven respawn for a project-bound session)", got)
	}
	if sess.IsProcessing() {
		t.Fatal("session left in processing state after resume_required")
	}
}

func TestManager_SendMessage_AdHocDeadProvider_Respawns(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	if err := mgr.SendMessage("s1", "hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := atomic.LoadInt32(&starts); got != 2 {
		t.Fatalf("provider Start count = %d, want 2 (ad-hoc sessions still respawn)", got)
	}
	if !sess.Provider().Alive() {
		t.Fatal("respawned provider should be alive")
	}
}

func TestManager_Create_Resume_ReattachesPersistedSession(t *testing.T) {
	store := session.NewStore(t.TempDir())
	persisted := &sessionstypes.Session{
		ID:            "s1",
		ProjectID:     "proj-1",
		Directory:     "/tmp/proj",
		Model:         "sonnet",
		ProviderType:  session.KindClaude,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Messages:      []sessionstypes.Message{{Timestamp: "t0", Role: "user", Content: json.RawMessage(`"hi"`)}},
		ProviderState: json.RawMessage(`{"claudeSessionId":"abc-123"}`),
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("Save: %v", err)
	}

	mgr := session.NewManager(session.Config{}, store, nil)
	fp := &fakeProvider{}
	mgr.SetProviderFactory(factoryReturning(fp))

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "s1",
		ProjectID: "proj-1",
		Kind:      session.KindClaude,
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("Create resume: %v", err)
	}
	if len(sess.Messages) != 1 || sessionstypes.ExtractTextContent(sess.Messages[0]) != "hi" {
		t.Fatalf("resumed session lost persisted history: %+v", sess.Messages)
	}
	var restored struct {
		ClaudeSessionID string `json:"claudeSessionId"`
	}
	if err := json.Unmarshal(fp.GetState(), &restored); err != nil || restored.ClaudeSessionID != "abc-123" {
		t.Fatalf("provider did not receive persisted state via RestoreState: %q (err=%v)", fp.GetState(), err)
	}
	if !sess.Provider().Alive() {
		t.Fatal("resumed session should have a live provider")
	}
}

func TestManager_Create_Resume_NoPersistedSession_Errors(t *testing.T) {
	mgr, _ := newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))

	_, err := mgr.Create(session.CreateSpec{SessionID: "ghost", ProjectID: "proj-1", Kind: session.KindClaude, Resume: true})
	if err == nil {
		t.Fatal("resume of a session with no persisted record: want an error, got nil")
	}
}

func TestManager_Get_LazyLoadsFromDisk(t *testing.T) {
	store := session.NewStore(t.TempDir())
	persisted := &sessionstypes.Session{ID: "on-disk", ProjectID: "proj-1", ProviderType: session.KindClaude}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr := session.NewManager(session.Config{}, store, nil)

	sess, ok := mgr.Get("on-disk")
	if !ok {
		t.Fatal("Get: session persisted on disk was not found")
	}
	if sess.Provider() != nil {
		t.Fatal("a lazy-loaded session must have no live provider")
	}
}

func TestManager_DeleteSession_RemovesPersistedFile(t *testing.T) {
	mgr, store := newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))

	sess, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mgr.EndSession(sess.ID) // kills + persists, matching relayLLM's EndSession

	if _, err := store.Load(sess.ID); err != nil {
		t.Fatalf("EndSession should have persisted the session: %v", err)
	}

	mgr.DeleteSession(sess.ID)
	if _, err := store.Load(sess.ID); err == nil {
		t.Fatal("DeleteSession: persisted file still present")
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("DeleteSession: session still resolvable via Get")
	}
}

func TestManager_Create_UnsupportedKind_Refused(t *testing.T) {
	mgr, _ := newTestManager(t)
	_, err := mgr.Create(session.CreateSpec{SessionID: "s1", ProjectID: "proj-1", Kind: session.KindChat})
	if err == nil {
		t.Fatal("chat kind has no provider wired yet: want an explicit error, got nil")
	}
}
