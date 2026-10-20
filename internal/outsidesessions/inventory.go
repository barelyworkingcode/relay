package outsidesessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

// endedEntry is one ended row. at is the transcript's modification time.
type endedEntry struct {
	observation Observation
	machine     Machine
	at          time.Time
}
type Inventory struct {
	mu       sync.Mutex
	opts     Options
	entries  map[string]entry
	ended    map[string]endedEntry
	failed   map[string]bool
	machines map[string]MachineStatus
	spool    map[string]bool
	revision uint64
	subs     map[chan Snapshot]bool
}

func New(opts Options) *Inventory {
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	return &Inventory{opts: opts, entries: map[string]entry{}, ended: map[string]endedEntry{}, failed: map[string]bool{}, machines: map[string]MachineStatus{}, spool: map[string]bool{}, subs: map[chan Snapshot]bool{}}
}
func hostIDValue(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}
func identity(m Machine, o Observation) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", m.Key, o.ClaudeID, o.PID, o.Lifetime)))
	return "outside:" + hex.EncodeToString(h[:16])
}

// endedIdentity never equals the live identity of the same session.
func endedIdentity(m Machine, claudeID string) string {
	h := sha256.Sum256([]byte(m.Key + "\x00" + claudeID + "\x00ended"))
	return "outside:" + hex.EncodeToString(h[:16])
}

// liveClaudeLocked reports whether a live row holds the machine's session id.
func (i *Inventory) liveClaudeLocked(machineKey, claudeID string) bool {
	for _, e := range i.entries {
		if e.machine.Key == machineKey && e.observation.ClaudeID == claudeID {
			return true
		}
	}
	return false
}
func (i *Inventory) clearEndedLocked(machineKey string) {
	for id, e := range i.ended {
		if e.machine.Key == machineKey {
			delete(i.ended, id)
		}
	}
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
	// A resumed session shows once, as live: the scan's held set can be a probe old.
	var ended []endedEntry
	for _, e := range i.ended {
		if !i.liveClaudeLocked(e.machine.Key, e.observation.ClaudeID) {
			ended = append(ended, e)
		}
	}
	sort.Slice(ended, func(a, b int) bool {
		if !ended[a].at.Equal(ended[b].at) {
			return ended[a].at.After(ended[b].at)
		}
		return ended[a].observation.Session.ID < ended[b].observation.Session.ID
	})
	if len(ended) > EndedRowsMax {
		ended = ended[:EndedRowsMax]
	}
	for _, e := range ended {
		s.Sessions = append(s.Sessions, e.observation.Session)
	}
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
		i.expireEnded()
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
				delete(i.spool, key)
				for id, e := range i.entries {
					if e.machine.Key == key {
						delete(i.entries, id)
					}
				}
				i.clearEndedLocked(key)
				delete(i.failed, key)
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
		i.probe(ctx, m, true)
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

// Probe replaces one machine's live evidence. Failure never advertises active stale work.
func (i *Inventory) Probe(ctx context.Context, m Machine) bool { return i.probe(ctx, m, false) }

// probe is Probe, and with scan also the ended scan of the same baseline. Both
// reach the snapshot in one publish, so a reader that sees the probe's time
// sees its ended rows.
func (i *Inventory) probe(ctx context.Context, m Machine, scan bool) bool {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := m.Probe(bounded)
	observations := result.Observations
	if ctx.Err() != nil {
		return false
	}
	var ended []Observation
	var endedErr error
	scanned := false
	if scan && err == nil && m.Ended != nil && i.opts.EndedWindow != nil {
		if window := i.opts.EndedWindow(); window > 0 {
			scanned = true
			endedCtx, endedCancel := context.WithTimeout(ctx, 10*time.Second)
			ended, endedErr = m.Ended(endedCtx, result.Held, i.opts.Clock.Now().Add(-window))
			endedCancel()
			if ctx.Err() != nil {
				return false
			}
		}
	}
	now := i.opts.Clock.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	status := MachineStatus{HostID: m.HostID, Machine: m.Name, Status: "reachable", LastProbeAt: stamp(now)}
	i.spool[m.Key] = err == nil && result.SawSpool
	if errors.Is(err, ErrUnsupported) {
		// Unsupported is stable, so there is no stale evidence to age out.
		status.Status = "unsupported"
		for id, e := range i.entries {
			if e.machine.Key == m.Key {
				delete(i.entries, id)
			}
		}
	} else if err != nil {
		status.Status = "unreachable"
		status.Error = "outside probe unavailable"
		for id, e := range i.entries {
			if e.machine.Key == m.Key {
				if now.Sub(e.liveAt) >= time.Minute {
					delete(i.entries, id)
				} else {
					e.observation.Session.State = "idle"
					e.hookAt = time.Time{}
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
	switch {
	case errors.Is(err, ErrUnsupported):
		i.clearEndedLocked(m.Key)
		delete(i.failed, m.Key)
	case err == nil && scan && !scanned:
		i.clearEndedLocked(m.Key)
		delete(i.failed, m.Key)
	case err == nil && scanned && endedErr != nil:
		// The rows stay as they were; only a change is logged.
		if !i.failed[m.Key] {
			slog.Log(ctx, slog.LevelWarn, "outside ended scan failed", "machine", m.Name, "host_id", hostIDValue(m.HostID))
		}
		i.failed[m.Key] = true
	case err == nil && scanned:
		delete(i.failed, m.Key)
		i.replaceEndedLocked(m, ended, result.Held, now)
	}
	if previous, known := i.machines[m.Key]; !known || previous.Status != status.Status {
		// Only a change is logged: the probe repeats every few seconds.
		level := slog.LevelInfo
		if status.Status == "unreachable" {
			level = slog.LevelWarn
		}
		slog.Log(ctx, level, "outside machine status changed", "machine", m.Name, "host_id", hostIDValue(m.HostID), "machine_status", status.Status)
	}
	i.machines[m.Key] = status
	i.publishLocked()
	return err == nil
}

// replaceEndedLocked swaps one machine's ended rows for a completed scan. The
// scan is untrusted input: a row with a held, invalid or unparsable identity is dropped.
func (i *Inventory) replaceEndedLocked(m Machine, scan []Observation, held map[string]bool, now time.Time) {
	i.clearEndedLocked(m.Key)
	for _, o := range scan {
		at, err := time.Parse(time.RFC3339Nano, o.Session.EndedAt)
		if err != nil || !sessionIdentity.MatchString(o.ClaudeID) || held[o.ClaudeID] {
			continue
		}
		id := endedIdentity(m, o.ClaudeID)
		if previous, ok := i.ended[id]; ok && !at.After(previous.at) {
			continue
		}
		o.Session.ID = id
		o.Session.State = "ended"
		o.Session.Outside = true
		o.Session.ReadOnly = true
		o.Session.HostID = m.HostID
		o.Session.Machine = m.Name
		o.Session.Source = "probe"
		o.Session.UpdatedAt = stamp(now)
		o.Session.TmuxSession = nil
		o.Session.Viewable = false
		o.Session.ViewReason = "not_in_tmux"
		o.Session.ClaudeSessionID = o.ClaudeID
		i.ended[id] = endedEntry{observation: o, machine: m, at: at}
	}
	var ids []string
	for id, e := range i.ended {
		if e.machine.Key == m.Key {
			ids = append(ids, id)
		}
	}
	if len(ids) > EndedRowsMax {
		sort.Slice(ids, func(a, b int) bool { return i.ended[ids[a]].at.After(i.ended[ids[b]].at) })
		for _, id := range ids[EndedRowsMax:] {
			delete(i.ended, id)
		}
	}
}

// expireEnded drops ended rows older than the window, by the injected clock.
func (i *Inventory) expireEnded() {
	window := time.Duration(0)
	if i.opts.EndedWindow != nil {
		window = i.opts.EndedWindow()
	}
	cutoff := i.opts.Clock.Now().Add(-window)
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := false
	for id, e := range i.ended {
		if window <= 0 || e.at.Before(cutoff) {
			delete(i.ended, id)
			changed = true
		}
	}
	if changed {
		i.publishLocked()
	}
}

// shouldDrain is true only for a reachable machine whose last good probe saw
// the report spool and which has a live outside row to update.
func (i *Inventory) shouldDrain(m Machine) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.spool[m.Key] || i.machines[m.Key].Status != "reachable" {
		return false
	}
	for _, e := range i.entries {
		if e.machine.Key == m.Key {
			return true
		}
	}
	return false
}
func (i *Inventory) Detail(ctx context.Context, id string) (Detail, error) {
	i.mu.Lock()
	e, ok := i.entries[id]
	ended, isEnded := i.ended[id]
	if isEnded && i.liveClaudeLocked(ended.machine.Key, ended.observation.ClaudeID) {
		isEnded = false
	}
	i.mu.Unlock()
	if !ok && isEnded {
		return i.endedDetail(ctx, ended)
	}
	if !ok {
		return Detail{}, ErrNotFound
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := e.machine.Probe(bounded)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	observations := result.Observations
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

// endedDetail reads the ended row's derived transcript; it runs no probe.
func (i *Inventory) endedDetail(ctx context.Context, e endedEntry) (Detail, error) {
	if e.machine.Read == nil {
		return Detail{}, ErrUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := e.machine.Read(bounded, e.observation)
	if err != nil {
		return Detail{}, ErrUnavailable
	}
	turn := LatestTurn(data)
	s := e.observation.Session
	s.UpdatedAt = stamp(i.opts.Clock.Now())
	return Detail{Session: s, LastTurn: turn, LastExcerpt: tailCharacters(turn.Text, 500)}, nil
}

// Transcript serves one page of an outside session's transcript. The id is
// only a key into the live inventory; the file is the path the last probe
// derived, so no request field names a path. before 0 means the newest page.
func (i *Inventory) Transcript(ctx context.Context, id string, before int64, limit int) (TranscriptPage, error) {
	i.mu.Lock()
	e, ok := i.entries[id]
	unreachable := i.machines[e.machine.Key].Status == "unreachable"
	i.mu.Unlock()
	if !ok {
		return TranscriptPage{}, ErrNotFound
	}
	if unreachable || e.machine.ReadRange == nil || e.observation.Transcript == "" {
		return TranscriptPage{}, ErrUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	end := int64(-1)
	if before > 0 {
		end = before
	}
	window, start, err := e.machine.ReadRange(bounded, e.observation, end, TranscriptPageBytes)
	if errors.Is(err, ErrBadCursor) {
		return TranscriptPage{}, ErrBadCursor
	}
	if err != nil || start < 0 || int64(len(window)) > TranscriptPageBytes {
		return TranscriptPage{}, ErrUnavailable
	}
	return ParseTranscriptPage(window, start, limit), nil
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
