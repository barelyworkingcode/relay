package session

import (
	"sync"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// Under -race this fails whenever respawnSpec reads slot.spec outside m.mu:
// the detector flags an unlocked read beside a locked write regardless of
// how the two goroutines happen to interleave.
func TestManager_RespawnSpec_ConcurrentLaunchWrite(t *testing.T) {
	const (
		sessionID  = "11111111-1111-1111-1111-111111111111"
		iterations = 2000
	)
	m := NewManager(Config{}, NewStore(t.TempDir()), nil)
	launched := CreateSpec{SessionID: sessionID, ProjectID: "p1", Kind: KindPi, ModelKey: "acme-key"}
	sess := &sessionstypes.Session{ID: sessionID, ProviderType: KindChat}
	slot := &sessionSlot{spec: launched, sess: sess, done: make(chan struct{})}
	m.slots[sessionID] = slot

	started := make(chan struct{})
	readerDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-started
		// Deliberately write before checking readerDone: a write that lands
		// after the reader's last lock is still unordered with its last read,
		// so the detector sees the race even if this goroutine is scheduled late.
		for {
			m.mu.Lock()
			slot.spec = launched
			m.mu.Unlock()
			select {
			case <-readerDone:
				return
			default:
			}
		}
	}()
	var got CreateSpec
	var err error
	go func() {
		defer wg.Done()
		close(started)
		for range iterations {
			got, err = m.respawnSpec(sess)
		}
		close(readerDone)
	}()
	wg.Wait()

	if err != nil {
		t.Fatalf("respawnSpec error = %v, want nil", err)
	}
	if got.SessionID != sessionID || got.ModelKey != "acme-key" {
		t.Errorf("respawnSpec = %+v, want the launched spec's SessionID and ModelKey", got)
	}
	if got.Kind != KindChat {
		t.Errorf("respawnSpec Kind = %q, want the session's ProviderType %q", got.Kind, KindChat)
	}
}
