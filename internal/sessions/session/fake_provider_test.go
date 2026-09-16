package session_test

import (
	"encoding/json"
	"sync"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// fakeProvider is a minimal, fully controllable sessionstypes.Provider for
// exercising Manager's own lifecycle logic (atomic reservation, SH-6
// branching, StopAll completeness) without spawning a real claude/pi
// binary. Distinct from internal/sessions/testutil.FakeProvider (which
// scripts canonical *events* for translator-shaped tests): this fake never
// emits an event on its own — Manager's business logic under test here
// doesn't depend on the wire format a provider produces.
type fakeProvider struct {
	mu    sync.Mutex
	alive bool

	// killed once true stays true — a real provider's underlying process,
	// once signalled, does not un-die because Start() happens to still be
	// in flight on another goroutine. Start checks this after any startGate
	// wait so a Kill that lands while this instance is mid-spawn (the
	// window a caller displacing it via SwapProvider must cover) can never
	// be followed by this instance quietly reporting itself alive anyway.
	killed bool

	startGate chan struct{} // if non-nil, Start blocks here before completing
	startErr  error

	startCount int
	killCount  int
	sent       []string
	state      json.RawMessage
}

func (p *fakeProvider) Start() error {
	if p.startGate != nil {
		<-p.startGate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startCount++
	if p.killed {
		return errKilledDuringStart
	}
	if p.startErr != nil {
		return p.startErr
	}
	p.alive = true
	return nil
}

func (p *fakeProvider) SendMessage(text string, _ []sessionstypes.FileAttachment) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.alive {
		return errNotRunning
	}
	p.sent = append(p.sent, text)
	return nil
}

func (p *fakeProvider) StopGeneration() {}

func (p *fakeProvider) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = true
	p.alive = false
	p.killCount++
}

func (p *fakeProvider) DeleteSession() error { return nil }

func (p *fakeProvider) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

func (p *fakeProvider) GetState() json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *fakeProvider) RestoreState(s json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = append(json.RawMessage(nil), s...)
}

func (p *fakeProvider) Starts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCount
}

func (p *fakeProvider) Kills() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killCount
}

func (p *fakeProvider) Sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.sent))
	copy(out, p.sent)
	return out
}

type notRunningError struct{}

func (notRunningError) Error() string { return "fakeProvider: not running" }

var errNotRunning = notRunningError{}

type killedDuringStartError struct{}

func (killedDuringStartError) Error() string { return "fakeProvider: killed during start" }

var errKilledDuringStart = killedDuringStartError{}
