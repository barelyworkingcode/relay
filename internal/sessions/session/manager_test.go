package session_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

	spec := session.CreateSpec{SessionID: "11111111-1111-1111-1111-111111111111", ProjectID: "proj-1", Kind: session.KindClaude}
	if _, err := mgr.Create(spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := mgr.Create(spec)
	if !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("second Create error = %v, want ErrSessionExists", err)
	}
}

// TestManager_Create_ConcurrentDuplicate_NoOrphan asserts many goroutines
// racing Create for the same session id can never let more than one
// provider actually spawn — a second spawn would be orphaned outside the
// manager's own tracking table.
func TestManager_Create_ConcurrentDuplicate_NoOrphan(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	const n = 25
	spec := session.CreateSpec{SessionID: "22222222-2222-2222-2222-222222222222", ProjectID: "proj-1", Kind: session.KindClaude}
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

// TestManager_StopAll_WaitsForInFlightLaunch asserts StopAll must not
// return while a launch still in flight has a process it is responsible
// for tearing down.
func TestManager_StopAll_WaitsForInFlightLaunch(t *testing.T) {
	mgr, _ := newTestManager(t)
	gate := make(chan struct{})
	fp := &fakeProvider{startGate: gate}
	mgr.SetProviderFactory(factoryReturning(fp))

	createDone := make(chan error, 1)
	go func() {
		_, err := mgr.Create(session.CreateSpec{SessionID: "33333333-3333-3333-3333-333333333333", ProjectID: "proj-1", Kind: session.KindClaude})
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

	sess, err := mgr.Create(session.CreateSpec{SessionID: "11111111-1111-1111-1111-111111111111", ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	err = mgr.SendMessage("11111111-1111-1111-1111-111111111111", "hello", nil)
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

	sess, err := mgr.Create(session.CreateSpec{SessionID: "11111111-1111-1111-1111-111111111111", ProjectID: "", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	if err := mgr.SendMessage("11111111-1111-1111-1111-111111111111", "hello", nil); err != nil {
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
		ID:            "11111111-1111-1111-1111-111111111111",
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
		SessionID: "11111111-1111-1111-1111-111111111111",
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

	_, err := mgr.Create(session.CreateSpec{SessionID: "99999999-9999-9999-9999-999999999999", ProjectID: "proj-1", Kind: session.KindClaude, Resume: true})
	if err == nil {
		t.Fatal("resume of a session with no persisted record: want an error, got nil")
	}
}

func TestManager_Get_LazyLoadsFromDisk(t *testing.T) {
	store := session.NewStore(t.TempDir())
	persisted := &sessionstypes.Session{ID: "44444444-4444-4444-4444-444444444444", ProjectID: "proj-1", ProviderType: session.KindClaude}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr := session.NewManager(session.Config{}, store, nil)

	sess, ok := mgr.Get("44444444-4444-4444-4444-444444444444")
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

	sess, err := mgr.Create(session.CreateSpec{SessionID: "11111111-1111-1111-1111-111111111111", ProjectID: "proj-1", Kind: session.KindClaude})
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
	_, err := mgr.Create(session.CreateSpec{SessionID: "11111111-1111-1111-1111-111111111111", ProjectID: "proj-1", Kind: "bogus"})
	if err == nil {
		t.Fatal("unsupported kind: want an explicit error, got nil")
	}
}

// TestManager_Get_DuringInFlightResume_NoClobberOrOrphan is B1's regression
// test: a Get racing an in-flight Create{Resume:true} for the same id must
// never lazy-load from disk over the reservation Create still owns. Gates
// the resume mid-spawn, calls Get concurrently, and asserts Get can only
// observe the in-flight reservation resolve — never a stale/nil-provider
// session — and that StopAll afterward reaches exactly the one provider
// that was actually spawned.
func TestManager_Get_DuringInFlightResume_NoClobberOrOrphan(t *testing.T) {
	store := session.NewStore(t.TempDir())
	persisted := &sessionstypes.Session{
		ID:           "55555555-5555-5555-5555-555555555555",
		ProjectID:    "proj-1",
		ProviderType: session.KindClaude,
	}
	if err := store.Save(persisted); err != nil {
		t.Fatalf("Save: %v", err)
	}

	mgr := session.NewManager(session.Config{}, store, nil)
	gate := make(chan struct{})
	fp := &fakeProvider{startGate: gate}
	mgr.SetProviderFactory(factoryReturning(fp))

	createDone := make(chan error, 1)
	go func() {
		_, err := mgr.Create(session.CreateSpec{
			SessionID: persisted.ID,
			ProjectID: "proj-1",
			Kind:      session.KindClaude,
			Resume:    true,
		})
		createDone <- err
	}()

	// Let Create reserve the slot and block inside Start on the gate.
	time.Sleep(50 * time.Millisecond)

	getDone := make(chan *sessionstypes.Session, 1)
	go func() {
		sess, ok := mgr.Get(persisted.ID)
		if !ok {
			getDone <- nil
			return
		}
		getDone <- sess
	}()

	select {
	case <-getDone:
		t.Fatal("Get returned before the in-flight resume finished spawning — it lazy-loaded over the reservation instead of waiting for it")
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)

	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create never returned after the gate was released")
	}

	var got *sessionstypes.Session
	select {
	case got = <-getDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Get never returned after the in-flight resume finished")
	}
	if got == nil {
		t.Fatal("Get: session not found after the resume it raced completed")
	}
	if p := got.Provider(); p == nil || !p.Alive() {
		t.Fatal("Get returned a session whose provider is not the live one Create just spawned")
	}

	mgr.StopAll()
	if got := fp.Starts(); got != 1 {
		t.Fatalf("provider Start count = %d, want exactly 1 (a second value means an orphaned spawn)", got)
	}
	if got := fp.Kills(); got != 1 {
		t.Fatalf("provider Kill count = %d, want exactly 1 (StopAll must reach the one real provider, not a stale disk-loaded stand-in)", got)
	}
}

// TestManager_ConcurrentSendMessageAndResume_NoOrphan is the fix-review's
// Door 1 regression test: SendMessage (which calls Get) and
// Create{Resume:true} racing for the same persisted-but-not-yet-live
// session, over many iterations with no gate or sleep forcing any
// particular interleaving. The bug this catches lives in the window
// between Get committing to a disk load and that load completing — a
// reservation Create makes during that window must never be overwritten by
// Get's own disk-loaded object, on pain of two providers ending up spawned
// for one session with only one of them reachable through the slot table.
func TestManager_ConcurrentSendMessageAndResume_NoOrphan(t *testing.T) {
	const iterations = 300
	store := session.NewStore(t.TempDir())

	for i := 0; i < iterations; i++ {
		id := fmt.Sprintf("88888888-8888-8888-0000-%012d", i)
		if err := store.Save(&sessionstypes.Session{ID: id, ProjectID: "", ProviderType: session.KindClaude}); err != nil {
			t.Fatalf("iteration %d: Save: %v", i, err)
		}

		mgr := session.NewManager(session.Config{}, store, nil)
		var mu sync.Mutex
		var built []*fakeProvider
		mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
			p := &fakeProvider{}
			mu.Lock()
			built = append(built, p)
			mu.Unlock()
			return p, nil
		})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = mgr.SendMessage(id, "hello", nil)
		}()
		go func() {
			defer wg.Done()
			_, _ = mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "", Kind: session.KindClaude, Resume: true})
		}()
		wg.Wait()

		mgr.StopAll()

		mu.Lock()
		snapshot := append([]*fakeProvider(nil), built...)
		mu.Unlock()
		for j, p := range snapshot {
			if p.Alive() {
				t.Fatalf("iteration %d: provider #%d of %d is still alive after StopAll — orphaned outside the slot table", i, j, len(snapshot))
			}
		}
	}
}

// TestManager_ConcurrentAdHocRespawnAndResume_NoOrphan is the fix-review's
// Door 2 regression test: startProvider must Kill whatever provider it
// displaces, even one still inside its own Start() and therefore reporting
// Alive() == false. Gates the ad-hoc respawn's Start() so it is still in
// flight when a concurrent Create{Resume:true} spawns and installs its own
// provider, then releases the gate and asserts every provider this test
// built — including the one that only finishes Start() after being
// displaced — ends up dead once StopAll runs.
func TestManager_ConcurrentAdHocRespawnAndResume_NoOrphan(t *testing.T) {
	mgr, _ := newTestManager(t)

	var mu sync.Mutex
	var built []*fakeProvider
	var buildCount int32
	gate := make(chan struct{})
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		p := &fakeProvider{}
		if atomic.AddInt32(&buildCount, 1) == 2 {
			// The ad-hoc respawn's own provider (the second one built):
			// gated so it is still inside Start() when the resume below
			// installs its replacement.
			p.startGate = gate
		}
		mu.Lock()
		built = append(built, p)
		mu.Unlock()
		return p, nil
	})

	const id = "99999999-9999-9999-9999-999999999999"
	sess, err := mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("initial Create: %v", err)
	}
	sess.Provider().Kill()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- mgr.SendMessage(id, "hello", nil)
	}()

	// Let SendMessage observe the dead provider, build the ad-hoc respawn's
	// replacement, swap it in, and block inside its Start().
	time.Sleep(50 * time.Millisecond)

	if _, err := mgr.Create(session.CreateSpec{SessionID: id, ProjectID: "", Kind: session.KindClaude, Resume: true}); err != nil {
		t.Fatalf("resume Create: %v", err)
	}

	close(gate)

	select {
	case <-sendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SendMessage never returned after the gate was released")
	}

	mgr.StopAll()

	mu.Lock()
	snapshot := append([]*fakeProvider(nil), built...)
	mu.Unlock()
	if len(snapshot) != 3 {
		t.Fatalf("providers built = %d, want 3 (initial, ad-hoc respawn, resume)", len(snapshot))
	}
	for j, p := range snapshot {
		if p.Alive() {
			t.Fatalf("provider #%d of %d is still alive after StopAll — orphaned outside the slot table", j, len(snapshot))
		}
	}
}

// TestManager_ClearSession_ProjectBound_RefusesRespawn is B2's regression
// test for the ClearSession fix: a project-bound session is left dead
// after clear rather than manager-internally respawned with a degraded
// identity, matching the same ErrResumeRequired contract SendMessage
// already enforces.
func TestManager_ClearSession_ProjectBound_RefusesRespawn(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_original",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = mgr.ClearSession(sess.ID)
	if !errors.Is(err, session.ErrResumeRequired) {
		t.Fatalf("ClearSession error = %v, want ErrResumeRequired", err)
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("provider Start count = %d, want 1 (project-bound ClearSession must not respawn)", got)
	}
	if p := sess.Provider(); p != nil && p.Alive() {
		t.Fatal("session left with a live provider after a refused ClearSession")
	}
	if len(sess.Messages) != 0 {
		t.Fatal("ClearSession should still clear history even when refusing to respawn")
	}
}

// TestManager_ClearSession_AdHoc_CarriesModelKeyForward is B2's regression
// test for the other ClearSession branch: an ad-hoc session still restarts,
// and the restarted provider gets the same ModelKey Create originally
// authorized it with, not a zero-value CreateSpec.
func TestManager_ClearSession_AdHoc_CarriesModelKeyForward(t *testing.T) {
	mgr, _ := newTestManager(t)
	var gotKeys []string
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, spec session.CreateSpec, _ sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		gotKeys = append(gotKeys, spec.ModelKey)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "",
		Kind:      session.KindPi,
		ModelKey:  "rmk_original",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.ClearSession(sess.ID); err != nil {
		t.Fatalf("ClearSession: %v", err)
	}

	if len(gotKeys) != 2 {
		t.Fatalf("provider factory called %d times, want 2 (initial + respawn)", len(gotKeys))
	}
	if gotKeys[1] != "rmk_original" {
		t.Fatalf("respawned provider ModelKey = %q, want %q (carried forward from Create)", gotKeys[1], "rmk_original")
	}
	if !sess.Provider().Alive() {
		t.Fatal("respawned provider should be alive")
	}
}

// TestManager_SendMessage_AdHocRespawn_CarriesModelKeyForward is B2's
// regression test for SendMessage's own ad-hoc respawn path: the same
// ModelKey carry-forward requirement, exercised through SendMessage rather
// than ClearSession.
func TestManager_SendMessage_AdHocRespawn_CarriesModelKeyForward(t *testing.T) {
	mgr, _ := newTestManager(t)
	var gotKeys []string
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, spec session.CreateSpec, _ sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		gotKeys = append(gotKeys, spec.ModelKey)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "",
		Kind:      session.KindPi,
		ModelKey:  "rmk_original",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	if err := mgr.SendMessage(sess.ID, "hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if len(gotKeys) != 2 {
		t.Fatalf("provider factory called %d times, want 2 (initial + respawn)", len(gotKeys))
	}
	if gotKeys[1] != "rmk_original" {
		t.Fatalf("respawned provider ModelKey = %q, want %q (carried forward from Create)", gotKeys[1], "rmk_original")
	}
}

// TestManager_Create_ResumeWhileAlive_SameModelKey_NoOp pins the fast path
// B3's fix must not regress: resuming a session that is still alive on the
// same key it was already launched with stays a no-op.
func TestManager_Create_ResumeWhileAlive_SameModelKey_NoOp(t *testing.T) {
	mgr, _ := newTestManager(t)
	var starts int32
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		atomic.AddInt32(&starts, 1)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_same",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	resumed, err := mgr.Create(session.CreateSpec{
		SessionID: sess.ID,
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_same",
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("Create resume with same key: %v", err)
	}
	if resumed != sess {
		t.Fatal("same-key resume-while-alive should return the same session")
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("provider Start count = %d, want 1 (same-key resume-while-alive is a no-op)", got)
	}
}

// TestManager_Create_ResumeWhileAlive_DifferentModelKey_Relaunches is B3's
// regression test: resuming a still-live session with a changed ModelKey
// must not silently succeed on the stale key. It must kill the old provider
// and relaunch with the new key instead.
func TestManager_Create_ResumeWhileAlive_DifferentModelKey_Relaunches(t *testing.T) {
	mgr, _ := newTestManager(t)
	var gotKeys []string
	var providers []*fakeProvider
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, spec session.CreateSpec, _ sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		gotKeys = append(gotKeys, spec.ModelKey)
		p := &fakeProvider{}
		providers = append(providers, p)
		return p, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_old",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldProvider := providers[0]
	if !oldProvider.Alive() {
		t.Fatal("initial provider should be alive")
	}

	resumed, err := mgr.Create(session.CreateSpec{
		SessionID: sess.ID,
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_new",
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("Create resume with changed key: %v", err)
	}

	if len(gotKeys) != 2 {
		t.Fatalf("provider factory called %d times, want 2 (a same-key resume-while-alive would stay at 1)", len(gotKeys))
	}
	if gotKeys[1] != "rmk_new" {
		t.Fatalf("relaunched provider ModelKey = %q, want %q", gotKeys[1], "rmk_new")
	}
	if oldProvider.Alive() {
		t.Fatal("old provider on the stale key should have been killed")
	}
	if oldProvider.Kills() != 1 {
		t.Fatalf("old provider kill count = %d, want 1", oldProvider.Kills())
	}
	if resumed != sess {
		t.Fatal("resume should reuse the same in-memory session object (message history)")
	}
	if p := resumed.Provider(); p == nil || !p.Alive() {
		t.Fatal("resumed session should have a new live provider")
	}
}

// TestManager_Create_Resume_DeadProvider_DifferentModelKey_NewProviderReceivesIt
// is R4's pin on C8's identity-continuity property: resuming a dead
// provider with a different ModelKey than the session's original launch
// must hand the new provider instance the new key, not a cached reference
// to the old one.
func TestManager_Create_Resume_DeadProvider_DifferentModelKey_NewProviderReceivesIt(t *testing.T) {
	mgr, _ := newTestManager(t)
	var gotKeys []string
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, spec session.CreateSpec, _ sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		gotKeys = append(gotKeys, spec.ModelKey)
		return &fakeProvider{}, nil
	})

	sess, err := mgr.Create(session.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_old",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().Kill()

	resumed, err := mgr.Create(session.CreateSpec{
		SessionID: sess.ID,
		ProjectID: "proj-1",
		Kind:      session.KindPi,
		ModelKey:  "rmk_new",
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("Create resume: %v", err)
	}
	if len(gotKeys) != 2 || gotKeys[1] != "rmk_new" {
		t.Fatalf("resumed provider received keys %v, want second entry %q", gotKeys, "rmk_new")
	}
	if p := resumed.Provider(); p == nil || !p.Alive() {
		t.Fatal("resumed session should have a live provider")
	}
}
