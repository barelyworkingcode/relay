// Package sessionhistory lists every agent session relay knows of: the ledger's
// records and the transcripts it finds on each machine. Titles and times are
// read from the transcripts and kept in memory; the ledger holds none of them.
// docs/session-history.md has the design.
package sessionhistory

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

const (
	// RowsMax caps one answer.
	RowsMax = 500
	// DiscoveredPerMachineMax caps the discovered records one machine holds.
	DiscoveredPerMachineMax = 2000
	scanTimeout             = 30 * time.Second
	consoleKey              = "console"
)

// Entry is one interactive transcript a reader found.
type Entry struct {
	Agent, ConversationID, Directory string
	Title                            string // "" when unknown
	TitleSource                      string // "client", "prompt" or ""
	LastActiveAt, CreatedAt          time.Time
}

// Listing is one machine's answer. Answered is false unless the script ran to
// the end, the store root is a real folder and the listing is under its cap.
type Listing struct {
	Answered bool
	Entries  []Entry
}

// Machine is one place transcripts live.
type Machine struct {
	Key    string // "console" or the host id
	Name   string // shown in the machines list
	HostID *string
	Run    func(ctx context.Context, script string) ([]byte, error)
	// Prefixes are the mangled project folder names to read; nil reads every
	// folder (the console).
	Prefixes []string
	// Scope, when set, recomputes Prefixes at each scan, for names that need a
	// round trip to the machine to resolve.
	Scope func(ctx context.Context) []string
}

// Reader finds one client's transcripts. An error means Answered is false.
type Reader interface {
	Agent() string
	Discover(ctx context.Context, m Machine) (Listing, error)
}

// Clock is the time the scan loop judges by.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// Options wires an Index to relay. Every callback may be nil, which reads as
// "no answer" and fails closed.
type Options struct {
	Clock         Clock
	Ledger        *ledger.Ledger
	Readers       []Reader
	Machines      func() []Machine
	Held          func(machineKey string) (map[string]bool, bool)
	Managed       func(ctx context.Context) (map[string]bool, error)
	Project       func(machineKey, directory string) *string
	RecordMachine func(r ledger.Record) string
}

// MachineStatus is one machine in the answer.
type MachineStatus struct {
	HostID     *string `json:"hostId"`
	Machine    string  `json:"machine"`
	Status     string  `json:"status"`
	LastScanAt *string `json:"lastScanAt"`
}

// Row is one history row. Title is untrusted display text.
type Row struct {
	ID             string  `json:"id"`
	Record         string  `json:"record"`
	Kind           string  `json:"kind"`
	Agent          *string `json:"agent"`
	ProjectID      *string `json:"projectId"`
	HostID         *string `json:"hostId"`
	Directory      string  `json:"directory"`
	ConversationID *string `json:"conversationId"`
	Title          *string `json:"title"`
	TitleSource    *string `json:"titleSource"`
	LastActiveAt   *string `json:"lastActiveAt"`
	CreatedAt      string  `json:"createdAt"`
	State          string  `json:"state"`
	Transcript     string  `json:"transcript"`
	Resume         string  `json:"resume"`
	ResumeReason   string  `json:"resumeReason,omitempty"`
}

// Response is wire 1's body.
type Response struct {
	Ready    bool            `json:"ready"`
	Revision uint64          `json:"revision"`
	Machines []MachineStatus `json:"machines"`
	Rows     []Row           `json:"rows"`
}

type machineState struct {
	scanned   bool
	answered  bool
	scannedAt time.Time
	// listing is the last answered one; a later failed scan leaves it.
	listing    Listing
	hasListing bool
	listingAt  time.Time
	byConv     map[string]Entry
}

// Index owns the scan loop and builds the rows.
type Index struct {
	o        Options
	mu       sync.Mutex
	revision uint64
	states   map[string]*machineState
}

// New returns an Index. Run starts its scans.
func New(o Options) *Index {
	if o.Clock == nil {
		o.Clock = wallClock{}
	}
	return &Index{o: o, states: map[string]*machineState{}}
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func convKey(agent, conversationID string) string { return agent + "\x00" + conversationID }

func (x *Index) machines() []Machine {
	if x.o.Machines == nil {
		return nil
	}
	return x.o.Machines()
}

// Run scans each machine once at start, then once per outsidesessions.ProbeInterval
// on the clock, until ctx ends. Each machine has its own worker, so a slow host
// never delays the console.
func (x *Index) Run(ctx context.Context) {
	workers := map[string]context.CancelFunc{}
	var stopped sync.WaitGroup
	defer func() {
		for _, cancel := range workers {
			cancel()
		}
		stopped.Wait()
	}()
	for {
		current := map[string]bool{}
		for _, m := range x.machines() {
			current[m.Key] = true
			if _, ok := workers[m.Key]; ok {
				continue
			}
			mc, cancel := context.WithCancel(ctx)
			workers[m.Key] = cancel
			stopped.Add(1)
			go func(key string) { defer stopped.Done(); x.runMachine(mc, key) }(m.Key)
		}
		for key, cancel := range workers {
			if !current[key] {
				cancel()
				delete(workers, key)
				x.forget(key)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-x.o.Clock.After(outsidesessions.ReportInterval):
		}
	}
}

func (x *Index) runMachine(ctx context.Context, key string) {
	for {
		next := x.o.Clock.After(outsidesessions.ProbeInterval)
		x.scan(ctx, key)
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
	}
}

func (x *Index) forget(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.states[key]; ok {
		delete(x.states, key)
		x.revision++
	}
}

func (x *Index) machine(key string) (Machine, bool) {
	for _, m := range x.machines() {
		if m.Key == key {
			return m, true
		}
	}
	return Machine{}, false
}

// scan reads one machine, writes its discovered records, then publishes the
// result and the event, in that order: a reader that sees the event sees the rows.
func (x *Index) scan(ctx context.Context, key string) {
	m, ok := x.machine(key)
	if !ok {
		return
	}
	ev := logging.BeginEvent(ctx, "session.history.scan").Set("machine", key)
	bounded, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	if m.Scope != nil {
		m.Prefixes = m.Scope(bounded)
	}
	listing, err := x.discover(bounded, m)
	if ctx.Err() != nil {
		return
	}
	if err == nil && listing.Answered {
		x.addDiscovered(ctx, m, listing.Entries)
	}
	now := x.o.Clock.Now()
	x.mu.Lock()
	st := x.states[key]
	if st == nil {
		st = &machineState{}
		x.states[key] = st
	}
	answered := err == nil && listing.Answered
	st.scanned, st.answered, st.scannedAt = true, answered, now
	if answered {
		st.listing, st.hasListing, st.listingAt = listing, true, now
		st.byConv = make(map[string]Entry, len(listing.Entries))
		for _, e := range listing.Entries {
			st.byConv[convKey(e.Agent, e.ConversationID)] = e
		}
	}
	// lastScanAt is part of the body, so every scan changes it.
	x.revision++
	revision := x.revision
	x.mu.Unlock()
	ev.Set("answered", answered).Set("count", len(listing.Entries)).Set("revision", int64(revision))
	if err != nil {
		ev.End(logging.OutcomeError, "scan_failed", err)
		return
	}
	ev.End(logging.OutcomeOK, "", nil)
}

func (x *Index) discover(ctx context.Context, m Machine) (Listing, error) {
	out := Listing{Answered: true}
	for _, r := range x.o.Readers {
		l, err := r.Discover(ctx, m)
		if err != nil {
			return Listing{}, err
		}
		out.Answered = out.Answered && l.Answered
		out.Entries = append(out.Entries, l.Entries...)
	}
	return out, nil
}

// addDiscovered writes one record per entry that no ledger record links and
// relay-sessions does not manage. A failed managed read adds nothing: without
// it a managed session could appear as a found one.
func (x *Index) addDiscovered(ctx context.Context, m Machine, entries []Entry) {
	if x.o.Ledger == nil || x.o.Managed == nil {
		return
	}
	managed, err := x.o.Managed(ctx)
	if err != nil {
		slog.Warn("session history: managed read failed, no discovered records added", "machine", m.Key, "error", err)
		return
	}
	linked := map[string]bool{}
	held := 0
	hostID := ""
	if m.HostID != nil {
		hostID = *m.HostID
	}
	for _, r := range x.o.Ledger.All() {
		if r.ConversationID == "" {
			continue
		}
		if r.State == ledger.StateDiscovered {
			if r.HostID == hostID {
				held++
				linked[convKey(r.Agent, r.ConversationID)] = true
			}
			continue
		}
		if x.recordMachine(r) == m.Key {
			linked[convKey(recordAgent(r), r.ConversationID)] = true
		}
	}
	var fresh []Entry
	for _, e := range entries {
		if linked[convKey(e.Agent, e.ConversationID)] || managed[e.ConversationID] {
			continue
		}
		fresh = append(fresh, e)
	}
	sort.SliceStable(fresh, func(a, b int) bool { return fresh[a].LastActiveAt.After(fresh[b].LastActiveAt) })
	if room := DiscoveredPerMachineMax - held; len(fresh) > room {
		fresh = fresh[:max(room, 0)]
	}
	recs := make([]ledger.Record, 0, len(fresh))
	for _, e := range fresh {
		recs = append(recs, ledger.Record{
			SessionID: ledger.DiscoveredID(m.Key, e.Agent, e.ConversationID),
			Kind:      e.Agent, Agent: e.Agent, State: ledger.StateDiscovered,
			Directory: e.Directory, HostID: hostID, ConversationID: e.ConversationID, Created: e.CreatedAt,
		})
	}
	if len(recs) == 0 {
		return
	}
	if _, err := x.o.Ledger.PutDiscovered(recs); err != nil {
		slog.Warn("session history: discovered records not saved", "machine", m.Key, "error", err)
	}
}

func (x *Index) recordMachine(r ledger.Record) string {
	if r.State == ledger.StateDiscovered {
		if r.HostID != "" {
			return r.HostID
		}
		return consoleKey
	}
	if x.o.RecordMachine == nil {
		return consoleKey
	}
	return x.o.RecordMachine(r)
}

// recordAgent is the agent a record's conversation belongs to: "" for a kind
// no reader knows.
func recordAgent(r ledger.Record) string {
	switch {
	case r.Agent != "":
		return r.Agent
	case r.Kind == "claude" || r.Kind == "pi" || r.Kind == "codex":
		return r.Kind
	}
	return ""
}

// Listing returns the machine's last answered listing and when it was read.
func (x *Index) Listing(machineKey string) (Listing, time.Time, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st := x.states[machineKey]
	if st == nil || !st.hasListing {
		return Listing{}, time.Time{}, false
	}
	return Listing{Answered: true, Entries: append([]Entry(nil), st.listing.Entries...)}, st.listingAt, true
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Rows is wire 1's body: every ledger record as one row, newest lastActiveAt
// first, filtered to one project when projectID is not empty.
func (x *Index) Rows(projectID string) Response {
	machines := x.machines()
	resp := Response{Ready: true, Machines: []MachineStatus{}, Rows: []Row{}}
	type snap struct {
		answered bool
		byConv   map[string]Entry
	}
	snaps := map[string]snap{}
	x.mu.Lock()
	resp.Revision = x.revision
	for _, m := range machines {
		st := x.states[m.Key]
		status := MachineStatus{HostID: m.HostID, Machine: m.Name, Status: "not_scanned"}
		if st != nil && st.scanned {
			status.Status = "unreachable"
			if st.answered {
				status.Status = "answered"
			}
			status.LastScanAt = strPtr(stamp(st.scannedAt))
		} else {
			resp.Ready = false
		}
		if st != nil && st.hasListing {
			snaps[m.Key] = snap{true, st.byConv}
		}
		resp.Machines = append(resp.Machines, status)
	}
	x.mu.Unlock()
	sort.SliceStable(resp.Machines, func(a, b int) bool {
		ha, hb := resp.Machines[a].HostID == nil, resp.Machines[b].HostID == nil
		if ha != hb {
			return ha
		}
		return resp.Machines[a].Machine < resp.Machines[b].Machine
	})
	if x.o.Ledger == nil {
		return resp
	}
	records := x.o.Ledger.All()
	// A discovered row hides once a launched record holds the same conversation.
	linked := map[string]bool{}
	for _, r := range records {
		if r.State != ledger.StateDiscovered && r.ConversationID != "" {
			linked[x.recordMachine(r)+"\x00"+convKey(recordAgent(r), r.ConversationID)] = true
		}
	}
	for _, r := range records {
		key := x.recordMachine(r)
		agent := recordAgent(r)
		if r.State == ledger.StateDiscovered && linked[key+"\x00"+convKey(agent, r.ConversationID)] {
			continue
		}
		row := x.row(r, key, agent, snaps[key].byConv, snaps[key].answered)
		if projectID != "" && (row.ProjectID == nil || *row.ProjectID != projectID) {
			continue
		}
		resp.Rows = append(resp.Rows, row)
	}
	sort.SliceStable(resp.Rows, func(a, b int) bool {
		ra, rb := resp.Rows[a], resp.Rows[b]
		if (ra.LastActiveAt == nil) != (rb.LastActiveAt == nil) {
			return ra.LastActiveAt != nil
		}
		if ra.LastActiveAt != nil && *ra.LastActiveAt != *rb.LastActiveAt {
			return *ra.LastActiveAt > *rb.LastActiveAt
		}
		return ra.ID < rb.ID
	})
	if len(resp.Rows) > RowsMax {
		resp.Rows = resp.Rows[:RowsMax]
	}
	return resp
}

func (x *Index) row(r ledger.Record, machineKey, agent string, byConv map[string]Entry, answered bool) Row {
	discovered := r.State == ledger.StateDiscovered
	row := Row{
		ID: r.SessionID, Record: "launched", Kind: r.Kind, Agent: strPtr(agent),
		Directory: r.Directory, ConversationID: strPtr(r.ConversationID),
		CreatedAt: stamp(r.Created), State: string(r.State), Transcript: "unknown",
	}
	if discovered {
		row.Record = "discovered"
		row.Kind = "terminal"
	} else if r.Kind == "pty" {
		row.Kind = "terminal"
	}
	if machineKey != consoleKey {
		host := machineKey
		row.HostID = &host
	}
	if discovered {
		if x.o.Project != nil {
			row.ProjectID = x.o.Project(machineKey, r.Directory)
		}
	} else {
		row.ProjectID = strPtr(r.ProjectID)
	}
	if e, ok := byConv[convKey(agent, r.ConversationID)]; ok && r.ConversationID != "" {
		row.Title, row.TitleSource = strPtr(e.Title), strPtr(e.TitleSource)
		row.LastActiveAt = strPtr(stamp(e.LastActiveAt))
		row.Transcript = "present"
	} else if answered && r.ConversationID != "" && agent != "" {
		row.Transcript = "absent"
	}
	row.Resume, row.ResumeReason = x.resume(r, row, machineKey)
	return row
}

// resume applies the table in docs/session-history.md: the first rule that
// applies decides.
func (x *Index) resume(r ledger.Record, row Row, machineKey string) (string, string) {
	switch {
	case r.State == ledger.StateLive:
		return "none", "live"
	case row.Record == "launched" && r.State == ledger.StateDormant && (r.Kind == "claude" || r.Kind == "pi" || r.Kind == "codex" || r.Kind == "chat"):
		return "session", ""
	case row.Kind != "terminal":
		return "none", "kind"
	case machineKey != consoleKey:
		return "none", "host_unsupported"
	case r.ConversationID == "":
		return "none", "no_conversation"
	}
	if x.o.Held != nil {
		if held, ok := x.o.Held(machineKey); ok && held[r.ConversationID] {
			return "none", "held"
		}
	}
	switch {
	case row.Transcript == "absent":
		return "none", "transcript_absent"
	case row.ProjectID == nil:
		return "none", "no_project"
	}
	return "history", ""
}
