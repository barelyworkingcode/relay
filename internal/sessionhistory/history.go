// Package sessionhistory lists every agent session relay knows of: the ledger's
// records and the transcripts it finds on each machine. Titles and times are
// read from the transcripts and kept in memory; the ledger holds none of them.
// docs/session-history.md has the design.
package sessionhistory

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
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
// the end and the store root is a real folder.
type Listing struct {
	Answered bool
	// Complete is true when the listing printed every transcript, under the cap.
	// Answered does not depend on it.
	Complete bool
	// Refused holds the folder names the listing refused (a symbolic link on the
	// folder or a transcript); what they hold is unknown.
	Refused map[string]bool
	// Scope is the folder prefixes the listing used; nil means every folder.
	Scope []string
	// Present holds every transcript the listing saw, read or not, interactive
	// or not; Entries only the files read and parsed. Ask Has, not Entries,
	// whether a conversation exists.
	Present map[string]bool
	Entries []Entry
}

// Absent reports that a transcript is gone: the listing answered and was
// complete, its scope covers the conversation's folder, it refused no folder
// named like it or starting with it, and it does not hold it. Anything else is
// unknown, never absent.
func (l Listing) Absent(agent, conversationID, directory string) bool {
	folder := outsidesessions.FolderName(directory)
	for name := range l.Refused {
		if strings.HasPrefix(name, folder) {
			return false
		}
	}
	return l.Answered && l.Complete && !l.Has(agent, conversationID) && hasPrefix(folder, l.Scope)
}

// PresentKey is the key of Listing.Present.
func PresentKey(agent, conversationID string) string { return agent + "\x00" + conversationID }

// Has reports whether the listing saw the conversation's transcript.
func (l Listing) Has(agent, conversationID string) bool {
	return l.Present[PresentKey(agent, conversationID)]
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

// ErrTranscriptAbsent and ErrNoFolder are Verify's two answers about the
// transcript itself, as opposed to a failure to read it.
var (
	ErrTranscriptAbsent = errors.New("session history: transcript absent")
	ErrNoFolder         = errors.New("session history: transcript has no folder of its own")
)

// verifier is a reader that can re-read one transcript now.
type verifier interface {
	Verify(ctx context.Context, m Machine, conversationID string) (Entry, error)
}

// Verify re-reads the transcript of one ledger record now, on its machine,
// bypassing the scan cache, and returns the entry with the folder derived by
// the ended-row rule. ErrTranscriptAbsent: no such transcript. ErrNoFolder: the
// transcript is not an interactive one or no record maps back to its file.
func (x *Index) Verify(ctx context.Context, recordID string) (Entry, error) {
	if x.o.Ledger == nil {
		return Entry{}, errors.New("session history: no ledger")
	}
	rec, ok := x.o.Ledger.Get(recordID)
	if !ok || rec.ConversationID == "" {
		return Entry{}, errors.New("session history: record has no conversation")
	}
	m, ok := x.machine(x.recordMachine(x.view(), rec))
	if !ok {
		return Entry{}, errors.New("session history: machine not listed")
	}
	agent := recordAgent(rec)
	for _, r := range x.o.Readers {
		if v, ok := r.(verifier); ok && r.Agent() == agent {
			return v.Verify(ctx, m, rec.ConversationID)
		}
	}
	return Entry{}, errors.New("session history: no reader for " + agent)
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
	View          func() View
	// Purge removes records whose transcript is gone; the zero value purges nothing.
	Purge PurgeDeps
}

// View is what one Rows call or one scan asks of relay's settings and probes.
// Options.View, when set, builds it once per call, so a call takes one settings
// snapshot and copies each held set once; the three Options funcs are the
// per-question fallback.
type View struct {
	Project       func(machineKey, directory string) *string
	RecordMachine func(r ledger.Record) string
	Held          func(machineKey string) (map[string]bool, bool)
}

func (x *Index) view() View {
	if x.o.View != nil {
		return x.o.View()
	}
	return View{Project: x.o.Project, RecordMachine: x.o.RecordMachine, Held: x.o.Held}
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
	// purgeRetry holds, in memory only, the time before which a record whose
	// purge failed to save is not tried again.
	purgeMu    sync.Mutex
	purgeRetry map[string]time.Time
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

func convKey(agent, conversationID string) string { return PresentKey(agent, conversationID) }

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
	superseded, evicted := x.reconcile(ctx, m, listing.Entries, err == nil && listing.Answered)
	if err == nil && listing.Answered {
		x.purge(ctx, m, listing)
	}
	now := x.o.Clock.Now()
	x.mu.Lock()
	// Checked under the lock: forget cancels first, then deletes, so a scan
	// that loses the race cannot bring a forgotten machine back.
	if ctx.Err() != nil {
		x.mu.Unlock()
		return
	}
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
	ev.Set("answered", answered).Set("complete", answered && listing.Complete).Set("superseded", superseded).Set("evicted", evicted).Set("count", len(listing.Entries)).Set("revision", int64(revision))
	if err != nil {
		ev.End(logging.OutcomeError, "scan_failed", err)
		return
	}
	ev.End(logging.OutcomeOK, "", nil)
}

func (x *Index) discover(ctx context.Context, m Machine) (Listing, error) {
	out := Listing{Answered: true, Complete: true, Scope: m.Prefixes}
	for _, r := range x.o.Readers {
		l, err := r.Discover(ctx, m)
		if err != nil {
			return Listing{}, err
		}
		out.Answered = out.Answered && l.Answered
		out.Complete = out.Complete && l.Complete
		if out.Present == nil {
			out.Present = map[string]bool{}
		}
		for k := range l.Present {
			out.Present[k] = true
		}
		for k := range l.Refused {
			if out.Refused == nil {
				out.Refused = map[string]bool{}
			}
			out.Refused[k] = true
		}
		out.Entries = append(out.Entries, l.Entries...)
	}
	return out, nil
}

// reconcile removes the machine's discovered records whose conversation
// relay-sessions now manages, and returns how many; when add is set it then
// writes one record per entry that no ledger record links and relay-sessions
// does not manage. A failed managed read changes nothing: without it a managed
// session could appear as a found one.
func (x *Index) reconcile(ctx context.Context, m Machine, entries []Entry, add bool) (superseded, evicted int) {
	if x.o.Ledger == nil || x.o.Managed == nil {
		return 0, 0
	}
	managed, err := x.o.Managed(ctx)
	if err != nil {
		slog.Warn("session history: managed read failed, discovered records unchanged", "machine", m.Key, "error", err)
		return 0, 0
	}
	var gone []string
	for _, r := range x.o.Ledger.All() {
		if r.State == ledger.StateDiscovered && r.Agent == "claude" && managed[r.ConversationID] && r.HostID == hostIDOf(m) {
			gone = append(gone, r.SessionID)
		}
	}
	if len(gone) > 0 {
		n, err := x.o.Ledger.RemoveDiscovered(gone)
		if err != nil {
			slog.Warn("session history: superseded records not removed", "machine", m.Key, "error", err)
		}
		superseded = n
	}
	if add {
		evicted = x.addDiscovered(ctx, m, entries, managed)
	}
	return superseded, evicted
}

func hostIDOf(m Machine) string {
	if m.HostID == nil {
		return ""
	}
	return *m.HostID
}

func (x *Index) addDiscovered(ctx context.Context, m Machine, entries []Entry, managed map[string]bool) int {
	linked := map[string]bool{}
	hostID := hostIDOf(m)
	v := x.view()
	type heldRec struct {
		at time.Time
		id string
	}
	existing := map[string]heldRec{}
	for _, r := range x.o.Ledger.All() {
		if r.ConversationID == "" {
			continue
		}
		if r.State == ledger.StateDiscovered {
			if r.HostID == hostID {
				linked[convKey(r.Agent, r.ConversationID)] = true
				existing[convKey(r.Agent, r.ConversationID)] = heldRec{r.Created, r.SessionID}
			}
			continue
		}
		if x.recordMachine(v, r) == m.Key {
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
	// The newest DiscoveredPerMachineMax win among the records already held and
	// the candidates, ranked by lastActiveAt, else created. The rest of the
	// held records go in the save that adds the new ones; the candidates that
	// lose are not added.
	for _, e := range entries {
		if h, ok := existing[convKey(e.Agent, e.ConversationID)]; ok {
			h.at = e.LastActiveAt
			existing[convKey(e.Agent, e.ConversationID)] = h
		}
	}
	type ranked struct {
		at    time.Time
		id    string // a held record's id
		fresh int    // index into fresh, or -1 for a held record
	}
	all := make([]ranked, 0, len(existing)+len(fresh))
	for _, h := range existing {
		all = append(all, ranked{h.at, h.id, -1})
	}
	for i, e := range fresh {
		all = append(all, ranked{e.LastActiveAt, "", i})
	}
	var evict []string
	if len(all) > DiscoveredPerMachineMax {
		sort.SliceStable(all, func(a, b int) bool {
			if !all[a].at.Equal(all[b].at) {
				return all[a].at.After(all[b].at)
			}
			return all[a].fresh < all[b].fresh
		})
		for _, r := range all[DiscoveredPerMachineMax:] {
			if r.fresh < 0 {
				evict = append(evict, r.id)
			}
		}
		all = all[:DiscoveredPerMachineMax]
		keep := make([]Entry, 0, len(fresh))
		for _, r := range all {
			if r.fresh >= 0 {
				keep = append(keep, fresh[r.fresh])
			}
		}
		fresh = keep
	}
	recs := make([]ledger.Record, 0, len(fresh))
	for _, e := range fresh {
		recs = append(recs, ledger.Record{
			SessionID: ledger.DiscoveredID(m.Key, e.Agent, e.ConversationID),
			Kind:      e.Agent, Agent: e.Agent, State: ledger.StateDiscovered,
			Directory: e.Directory, HostID: hostID, ConversationID: e.ConversationID, Created: e.CreatedAt,
		})
	}
	_, evicted, err := x.o.Ledger.ReplaceDiscovered(recs, evict)
	if err != nil {
		slog.Warn("session history: discovered records not saved", "machine", m.Key, "error", err)
	}
	return evicted
}

func (x *Index) recordMachine(v View, r ledger.Record) string {
	if r.State == ledger.StateDiscovered {
		if r.HostID != "" {
			return r.HostID
		}
		return consoleKey
	}
	if v.RecordMachine == nil {
		return consoleKey
	}
	return v.RecordMachine(r)
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
	present := make(map[string]bool, len(st.listing.Present))
	for k := range st.listing.Present {
		present[k] = true
	}
	refused := make(map[string]bool, len(st.listing.Refused))
	for k := range st.listing.Refused {
		refused[k] = true
	}
	return Listing{Answered: true, Refused: refused, Complete: st.listing.Complete, Scope: append([]string(nil), st.listing.Scope...), Present: present, Entries: append([]Entry(nil), st.listing.Entries...)}, st.listingAt, true
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
		listing Listing
		byConv  map[string]Entry
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
			snaps[m.Key] = snap{st.listing, st.byConv}
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
	v := x.view()
	type heldSet struct {
		set map[string]bool
		ok  bool
	}
	heldBy := map[string]heldSet{}
	if held := v.Held; held != nil {
		v.Held = func(key string) (map[string]bool, bool) {
			if h, ok := heldBy[key]; ok {
				return h.set, h.ok
			}
			set, ok := held(key)
			heldBy[key] = heldSet{set, ok}
			return set, ok
		}
	}
	// A discovered row hides once a launched record holds the same conversation.
	linked := map[string]bool{}
	for _, r := range records {
		if r.State != ledger.StateDiscovered && r.ConversationID != "" {
			linked[x.recordMachine(v, r)+"\x00"+convKey(recordAgent(r), r.ConversationID)] = true
		}
	}
	sortAt := map[string]time.Time{}
	for _, r := range records {
		key := x.recordMachine(v, r)
		agent := recordAgent(r)
		if r.State == ledger.StateDiscovered && linked[key+"\x00"+convKey(agent, r.ConversationID)] {
			continue
		}
		row := x.row(v, r, key, agent, snaps[key].byConv, snaps[key].listing)
		if projectID != "" && (row.ProjectID == nil || *row.ProjectID != projectID) {
			continue
		}
		// A row with no parsed entry sorts by when it was created, so it stays
		// inside the cap; its wire lastActiveAt stays null.
		at := r.Created
		if row.LastActiveAt != nil {
			at, _ = time.Parse(time.RFC3339Nano, *row.LastActiveAt)
		}
		sortAt[row.ID] = at
		resp.Rows = append(resp.Rows, row)
	}
	sort.SliceStable(resp.Rows, func(a, b int) bool {
		ra, rb := resp.Rows[a], resp.Rows[b]
		if !sortAt[ra.ID].Equal(sortAt[rb.ID]) {
			return sortAt[ra.ID].After(sortAt[rb.ID])
		}
		return ra.ID < rb.ID
	})
	if len(resp.Rows) > RowsMax {
		resp.Rows = resp.Rows[:RowsMax]
	}
	return resp
}

func (x *Index) row(v View, r ledger.Record, machineKey, agent string, byConv map[string]Entry, listing Listing) Row {
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
		if v.Project != nil {
			row.ProjectID = v.Project(machineKey, r.Directory)
		}
	} else {
		row.ProjectID = strPtr(r.ProjectID)
	}
	if r.ConversationID != "" && agent != "" {
		if listing.Has(agent, r.ConversationID) {
			row.Transcript = "present"
		} else if listing.Absent(agent, r.ConversationID, r.Directory) {
			row.Transcript = "absent"
		}
		if e, ok := byConv[convKey(agent, r.ConversationID)]; ok {
			row.Title, row.TitleSource = strPtr(e.Title), strPtr(e.TitleSource)
			row.LastActiveAt = strPtr(stamp(e.LastActiveAt))
		}
	}
	row.Resume, row.ResumeReason = x.resume(v, r, row, machineKey)
	return row
}

// resume applies the table in docs/session-history.md: the first rule that
// applies decides.
func (x *Index) resume(v View, r ledger.Record, row Row, machineKey string) (string, string) {
	switch {
	case r.State == ledger.StateLive:
		return "none", "live"
	case row.Record == "launched" && r.State == ledger.StateDormant && (r.Kind == "claude" || r.Kind == "pi" || r.Kind == "codex" || r.Kind == "chat"):
		if row.Transcript == "absent" {
			return "none", "transcript_absent"
		}
		return "session", ""
	case row.Kind != "terminal":
		return "none", "kind"
	case machineKey != consoleKey:
		return "none", "host_unsupported"
	case r.ConversationID == "":
		return "none", "no_conversation"
	}
	if v.Held != nil {
		if held, ok := v.Held(machineKey); ok && held[r.ConversationID] {
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
