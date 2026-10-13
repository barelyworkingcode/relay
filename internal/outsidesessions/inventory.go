package outsidesessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

type entry struct {
	observation Observation
	machine     Machine
	liveAt      time.Time
	hookAt      time.Time
}
type Inventory struct {
	mu       sync.Mutex
	opts     Options
	entries  map[string]entry
	machines map[string]MachineStatus
	revision uint64
	subs     map[chan Snapshot]bool
}

func New(opts Options) *Inventory {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	return &Inventory{opts: opts, entries: map[string]entry{}, machines: map[string]MachineStatus{}, subs: map[chan Snapshot]bool{}}
}
func identity(m Machine, o Observation) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", m.Key, o.ClaudeID, o.PID, o.Lifetime)))
	return "outside:" + hex.EncodeToString(h[:16])
}
func (i *Inventory) snapshotLocked() Snapshot {
	s := Snapshot{Type: "outside_sessions", Revision: i.revision, ProbeIntervalMs: 15000, Sessions: []Session{}, Machines: []MachineStatus{}}
	for _, e := range i.entries {
		s.Sessions = append(s.Sessions, e.observation.Session)
	}
	for _, m := range i.machines {
		s.Machines = append(s.Machines, m)
	}
	sort.Slice(s.Sessions, func(a, b int) bool { return s.Sessions[a].ID < s.Sessions[b].ID })
	sort.Slice(s.Machines, func(a, b int) bool { return s.Machines[a].Machine < s.Machines[b].Machine })
	return s
}
func (i *Inventory) Snapshot() Snapshot { i.mu.Lock(); defer i.mu.Unlock(); return i.snapshotLocked() }
func (i *Inventory) publishLocked() {
	i.revision++
	s := i.snapshotLocked()
	for ch := range i.subs {
		select {
		case ch <- s:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- s
		}
	}
}
func (i *Inventory) Subscribe() (chan Snapshot, func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ch := make(chan Snapshot, 1)
	i.subs[ch] = true
	ch <- i.snapshotLocked()
	return ch, func() { i.mu.Lock(); delete(i.subs, ch); i.mu.Unlock() }
}

// Run reconciles configured machines at every baseline, with each machine's work independent.
func (i *Inventory) Run(ctx context.Context) {
	workers := map[string]context.CancelFunc{}
	var stopped sync.WaitGroup
	defer func() {
		for _, cancel := range workers {
			cancel()
		}
		stopped.Wait()
	}()
	for {
		i.expireUnreachable()
		current := map[string]bool{}
		for _, m := range i.opts.Machines() {
			current[m.Key] = true
			if _, ok := workers[m.Key]; !ok {
				mc, cancel := context.WithCancel(ctx)
				workers[m.Key] = cancel
				stopped.Add(1)
				go func() { defer stopped.Done(); i.runMachine(mc, m) }()
			}
		}
		for key, cancel := range workers {
			if !current[key] {
				cancel()
				delete(workers, key)
				i.mu.Lock()
				delete(i.machines, key)
				for id, e := range i.entries {
					if e.machine.Key == key {
						delete(i.entries, id)
					}
				}
				i.publishLocked()
				i.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-i.opts.Clock.After(ReportInterval):
		}
	}
}
func (i *Inventory) runMachine(ctx context.Context, m Machine) {
	// A slow baseline must not delay the two-second report schedule.
	var reports sync.WaitGroup
	reports.Add(1)
	go func() {
		defer reports.Done()
		for {
			next := i.opts.Clock.After(ReportInterval)
			select {
			case <-ctx.Done():
				return
			case <-next:
				i.drain(ctx, m)
			}
		}
	}()
	defer reports.Wait()
	for {
		next := i.opts.Clock.After(ProbeInterval)
		i.Probe(ctx, m)
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// Probe replaces one machine's live evidence. Failure never advertises active stale work.
func (i *Inventory) Probe(ctx context.Context, m Machine) bool {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	observations, err := m.Probe(bounded)
	if ctx.Err() != nil {
		return false
	}
	now := i.opts.Clock.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	status := MachineStatus{HostID: m.HostID, Machine: m.Name, Status: "reachable", LastProbeAt: stamp(now)}
	if err != nil {
		status.Status = "unreachable"
		status.Error = "outside probe unavailable"
		for id, e := range i.entries {
			if e.machine.Key == m.Key {
				if now.Sub(e.liveAt) >= time.Minute {
					delete(i.entries, id)
				} else {
					e.observation.Session.State = "idle"
					e.hookAt = time.Time{}
					e.observation.Session.Viewable = false
					e.observation.Session.ViewReason = "host_unreachable"
					i.entries[id] = e
				}
			}
		}
	} else {
		seen := map[string]bool{}
		for _, o := range observations {
			id := identity(m, o)
			seen[id] = true
			o.Session.ID = id
			o.Session.Outside = true
			o.Session.ReadOnly = true
			o.Session.HostID = m.HostID
			o.Session.Machine = m.Name
			o.Session.Source = "probe"
			o.Session.UpdatedAt = stamp(now)
			previous := i.entries[id]
			if !previous.hookAt.IsZero() && !o.StatusAt.After(previous.hookAt) {
				o.Session.State = previous.observation.Session.State
				o.Session.Source = "hook"
			}
			i.entries[id] = entry{observation: o, machine: m, liveAt: now, hookAt: previous.hookAt}
		}
		for id, e := range i.entries {
			if e.machine.Key == m.Key && !seen[id] {
				delete(i.entries, id)
			}
		}
	}
	i.machines[m.Key] = status
	i.publishLocked()
	return err == nil
}
func (i *Inventory) Detail(ctx context.Context, id string) (Detail, error) {
	i.mu.Lock()
	e, ok := i.entries[id]
	i.mu.Unlock()
	if !ok {
		return Detail{}, ErrNotFound
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	observations, err := e.machine.Probe(bounded)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	var live *Observation
	for j := range observations {
		if identity(e.machine, observations[j]) == id {
			live = &observations[j]
			break
		}
	}
	if live == nil {
		i.mu.Lock()
		delete(i.entries, id)
		i.publishLocked()
		i.mu.Unlock()
		return Detail{}, ErrNotFound
	}
	data, err := e.machine.Read(bounded, *live)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	turn := LatestTurn(data)
	s := live.Session
	s.ID = id
	s.HostID = e.machine.HostID
	s.Machine = e.machine.Name
	s.Outside = true
	s.ReadOnly = true
	s.Source = "probe"
	if !e.hookAt.IsZero() && !live.StatusAt.After(e.hookAt) {
		s.Source = "hook"
		s.State = e.observation.Session.State
	}
	s.UpdatedAt = stamp(i.opts.Clock.Now())
	return Detail{Session: s, LastTurn: turn, LastExcerpt: tailCharacters(turn.Text, 500)}, nil
}

func (i *Inventory) expireUnreachable() {
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := false
	for id, e := range i.entries {
		if i.machines[e.machine.Key].Status == "unreachable" && i.opts.Clock.Now().Sub(e.liveAt) >= time.Minute-ReportInterval {
			delete(i.entries, id)
			changed = true
		}
	}
	if changed {
		i.publishLocked()
	}
}
