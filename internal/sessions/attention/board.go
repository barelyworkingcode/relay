package attention

import (
	"log/slog"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/clock"
)

type Attention struct {
	State State  `json:"state"`
	Since string `json:"since"`
}

type Change struct {
	SessionID string
	State     State
	Since     time.Time
}

type TurnDone struct {
	SessionID, Excerpt string
	At                 time.Time
}

// Sink receives changes under the session's lock, so it must not call back
// into the Board.
type Sink interface {
	StateChanged(Change)
	TurnDone(TurnDone)
}

func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type entry struct {
	mu      sync.Mutex
	id      string
	state   State
	since   time.Time
	last    time.Time
	excerpt []rune
	dead    bool
}

type Board struct {
	clk  clock.Clock
	sink Sink

	mu       sync.Mutex
	entries  map[string]*entry
	sweeping bool
}

func NewBoard(clk clock.Clock, sink Sink) *Board {
	return &Board{clk: clk, sink: sink, entries: map[string]*entry{}}
}

func (b *Board) Signal(sessionID string, sig Signal) {
	var e *entry
	for {
		e = b.lookup(sessionID, sig == Launching)
		if e == nil {
			return
		}
		e.mu.Lock()
		if !e.dead {
			break
		}
		e.mu.Unlock()
		if sig != Launching {
			return
		}
	}
	defer e.mu.Unlock()
	b.apply(e, sig, time.Time{})
}

// Reply appends to the current turn's excerpt and counts as activity.
func (b *Board) Reply(sessionID, text string) {
	e := b.lookup(sessionID, false)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return
	}
	e.last = b.clk.Now()
	e.excerpt = append(e.excerpt, []rune(text)...)
	if len(e.excerpt) > 2*ExcerptRunes {
		e.excerpt = append([]rune(nil), e.excerpt[len(e.excerpt)-ExcerptRunes:]...)
	}
}

func (b *Board) Get(sessionID string) (Attention, bool) {
	e := b.lookup(sessionID, false)
	if e == nil {
		return Attention{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return Attention{}, false
	}
	return Attention{State: e.state, Since: FormatTime(e.since)}, true
}

// Sweep moves every running session whose last event is StallAfter old to
// stalled. Asking sessions wait on a person and never stall.
func (b *Board) Sweep() { b.sweep() }

func (b *Board) sweep() int {
	b.mu.Lock()
	list := make([]*entry, 0, len(b.entries))
	for _, e := range b.entries {
		list = append(list, e)
	}
	b.mu.Unlock()
	now := b.clk.Now()
	for _, e := range list {
		e.mu.Lock()
		if !e.dead && e.state == Running && now.Sub(e.last) >= StallAfter {
			b.apply(e, StallTimeout, e.last.Add(StallAfter))
		}
		e.mu.Unlock()
	}
	return len(list)
}

func (b *Board) lookup(id string, create bool) *entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[id]
	if e == nil && create {
		e = &entry{id: id}
		b.entries[id] = e
		if !b.sweeping {
			b.sweeping = true
			go b.sweepLoop()
		}
	}
	return e
}

// sweepLoop exits after a sweep that finds no entries; the flag is cleared
// under b.mu, the same lock lookup uses to start a new loop, so no entry is
// left without one.
func (b *Board) sweepLoop() {
	for {
		<-b.clk.After(SweepInterval)
		if b.sweep() == 0 {
			b.mu.Lock()
			if len(b.entries) == 0 {
				b.sweeping = false
				b.mu.Unlock()
				return
			}
			b.mu.Unlock()
		}
	}
}

// apply runs under e.mu. since overrides the transition time when non-zero.
func (b *Board) apply(e *entry, sig Signal, since time.Time) {
	now := b.clk.Now()
	prev := e.state
	next := Next(prev, sig)
	if sig != StallTimeout {
		e.last = now
	}
	inTurn := prev == Running || prev == Asking || prev == Stalled
	if inTurn && next != prev && (sig == TurnEnded || sig == TurnFailed || sig == TurnStopped) && b.sink != nil {
		b.sink.TurnDone(TurnDone{SessionID: e.id, Excerpt: string(tail(e.excerpt)), At: now})
	}
	if sig == TurnStarted {
		e.excerpt = nil
	}
	if next != prev {
		if since.IsZero() {
			since = now
		}
		e.state, e.since = next, since
		slog.Info("session state", "op", "session.state", "status", "ok", "duration_ms", 0,
			"session_id", e.id, "from", string(prev), "to", string(next))
		if b.sink != nil {
			b.sink.StateChanged(Change{SessionID: e.id, State: next, Since: since})
		}
	}
	if next == Ended || (sig == LaunchFailed && prev == Starting) {
		e.dead = true
		b.mu.Lock()
		if b.entries[e.id] == e {
			delete(b.entries, e.id)
		}
		b.mu.Unlock()
	}
}

func tail(r []rune) []rune {
	if len(r) > ExcerptRunes {
		return r[len(r)-ExcerptRunes:]
	}
	return r
}
