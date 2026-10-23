package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

// The purge tests plant transcripts under the console's HOME (and the stub
// host's root), let one scan record them, remove the file, and then move the
// server clock past the grace period with ClockAdvance. A clock move starts a
// scan and a probe at once, so a jump is followed by a probe that completes
// after it (the held set is fresh) and by scans; a purge is awaited on its
// session.history.purge event, then read back from the audit log.
//
// A test that expects "nothing purged" also holds a control: a record on the
// same machine, or on a second machine, that is absent the same way and is
// purged in the same step. The control's purge event proves the pass ran
// after the state the test set up, so a kept record is not a skipped pass.
// Restarts are avoided after the clock moves, because a restart returns the
// test clock to wall time.

const (
	hpDay       = 24 * time.Hour
	hpPurgeKey  = "session.history.purge"
	hpScanKey   = "session.history.scan"
	hpMaxTicks  = 8
	hpBulkCount = 4097 // one past the listing's cap of 4096
)

type hpD struct{ conv, dir, path string }

func (d hpD) remove(t *testing.T) {
	t.Helper()
	if err := os.Remove(d.path); err != nil {
		t.Fatalf("removing the transcript %s: %v", d.path, err)
	}
}

// hpPlant writes an interactive one-prompt transcript of conversation n in dir under home.
func (h *hx) hpPlant(home, dir string, n int) hpD {
	h.t.Helper()
	conv := hxConv(n)
	hxSimple(h.t, home, dir, conv, hxBase.Add(time.Duration(n)*time.Minute), "acme purge")
	return hpD{conv: conv, dir: dir, path: osTranscriptPath(home, dir, conv)}
}

// hpConsole plants a console transcript in a folder of its own.
func (h *hx) hpConsole(n int, name string) hpD {
	h.t.Helper()
	return h.hpPlant(h.i.Home, osDir(h.t, h.t.TempDir(), name), n)
}

// hpID is the ledger id of the row for d; the row must exist.
func (h *hx) hpID(d hpD) string {
	h.t.Helper()
	r := h.rows().byConv(d.conv)
	if r == nil {
		h.t.Fatalf("no history row for conversation %s", d.conv)
	}
	return r.ID
}

func (h *hx) hpRecs() []map[string]any {
	h.t.Helper()
	var recs []map[string]any
	if err := json.Unmarshal(mustRead(h.t, filepath.Join(h.i.ConfigDir, g1rLedger)), &recs); err != nil {
		h.t.Fatalf("ledger does not decode: %v", err)
	}
	return recs
}

func (h *hx) hpRec(id string) map[string]any {
	h.t.Helper()
	for _, rec := range h.hpRecs() {
		if rec["session_id"] == id {
			return rec
		}
	}
	return nil
}

// hpMissing is the record's missing_since text, empty when unset or no record.
func (h *hx) hpMissing(id string) string {
	h.t.Helper()
	if s, ok := h.hpRec(id)["missing_since"].(string); ok {
		return s
	}
	return ""
}

func (h *hx) hpPurged(id string) bool {
	h.t.Helper()
	return len(h.i.Events(harness.EventQuery{Key: hpPurgeKey, Fields: map[string]any{"session_id": id, "status": "ok"}})) > 0
}

// hpAnyPurge reports a purge event of any status for id.
func (h *hx) hpAnyPurge(id string) bool {
	h.t.Helper()
	return len(h.i.Events(harness.EventQuery{Key: hpPurgeKey, Fields: map[string]any{"session_id": id}})) > 0
}

// hpSettle waits for two scans, so the pass of the first has finished.
func (h *hx) hpSettle() {
	h.t.Helper()
	h.scan(nil)
	h.scan(nil)
}

// hpJump moves the clock by d and waits for a probe that completes after the
// move and two scans after that, so the pass has run with a fresh held set.
func (h *hx) hpJump(d time.Duration) {
	h.t.Helper()
	h.i.ClockAdvance(d)
	h.probe()
	h.hpSettle()
}

// hpAwait ticks until every id has a purge event, then waits for the event.
func (h *hx) hpAwait(tick func(), ids ...string) {
	h.t.Helper()
	for k := 0; ; k++ {
		all := true
		for _, id := range ids {
			all = all && h.hpPurged(id)
		}
		if all {
			break
		}
		if k == hpMaxTicks {
			h.t.Fatalf("no session.history.purge event for %v after %d scans past the grace period", ids, hpMaxTicks)
		}
		tick()
	}
	for _, id := range ids {
		h.i.WaitEvent(harness.EventQuery{Key: hpPurgeKey, Fields: map[string]any{"session_id": id, "status": "ok"}}, osBound)
	}
}

// hpPurgeAll jumps past the grace period and waits for the purges of ids.
func (h *hx) hpPurgeAll(d time.Duration, ids ...string) {
	h.t.Helper()
	h.hpJump(d)
	h.hpAwait(func() { h.scan(nil) }, ids...)
}

// hpKept fails unless the ledger and the history list still hold id.
func (h *hx) hpKept(id string) {
	h.t.Helper()
	if h.hpRec(id) == nil {
		h.t.Fatalf("the ledger no longer holds record %s, want it kept", id)
	}
	if h.rows().byID(id) == nil {
		h.t.Fatalf("the history no longer lists %s, want it kept", id)
	}
	if h.hpAnyPurge(id) {
		h.t.Fatalf("a session.history.purge event names %s, want none", id)
	}
}

// hpGone fails unless neither the ledger nor the list holds id.
func (h *hx) hpGone(id string) {
	h.t.Helper()
	if h.hpRec(id) != nil {
		h.t.Fatalf("the ledger still holds record %s, want it purged", id)
	}
	if h.rows().byID(id) != nil {
		h.t.Fatalf("the history still lists %s, want it purged", id)
	}
}

// hpAudit is the one session_purge row naming id.
func hpAudit(t *testing.T, i *harness.Instance, id string) map[string]any {
	t.Helper()
	var got []map[string]any
	for _, r := range i.Audit(harness.AuditQuery{Event: "session_purge", Tail: 5000}) {
		if a, ok := r["args"].(map[string]any); ok && a["session_id"] == id {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d session_purge audit rows name %s, want exactly 1: %v", len(got), id, got)
	}
	return got[0]
}

// hpWitness plants a console transcript that stays, so the listing is never empty.
func (h *hx) hpWitness() hpD { return h.hpConsole(900, "witness") }

// hpBulk plants one transcript more than the listing prints and returns the folder.
func hpBulk(t *testing.T, home string) string {
	t.Helper()
	bulk := filepath.Dir(osTranscriptPath(home, osDir(t, t.TempDir(), "bulk"), "x"))
	if err := os.MkdirAll(bulk, 0o755); err != nil {
		t.Fatalf("creating %s: %v", bulk, err)
	}
	for n := 1; n <= hpBulkCount; n++ {
		if err := os.WriteFile(filepath.Join(bulk, hxConv(5000+n)+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("writing bulk transcript %d: %v", n, err)
		}
	}
	return bulk
}

// hpBoot boots a console-only rig with these settings.json keys (raw JSON).
func hpBoot(t *testing.T, settings map[string]string) *hx {
	t.Helper()
	return hxReady(t, osStart(t, false, hpSettings(settings)), false)
}

func hpSettings(settings map[string]string) func(*harness.Options) {
	return func(o *harness.Options) {
		if o.Settings == nil {
			o.Settings = map[string]json.RawMessage{}
		}
		for k, v := range settings {
			o.Settings[k] = json.RawMessage(v)
		}
	}
}

// hpSetDays hand-edits session_history.purge_after_days while relay runs.
func hpSetDays(t *testing.T, i *harness.Instance, raw string) {
	t.Helper()
	path := filepath.Join(i.ConfigDir, "settings.json")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatalf("settings.json does not decode: %v", err)
	}
	doc["session_history"] = json.RawMessage(`{"purge_after_days":` + raw + `}`)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding settings.json: %v", err)
	}
	osWrite(t, path, out)
}

// hpStartTerm boots the terminal rig with a witness transcript already listed.
func hpStartTerm(t *testing.T) *htRig {
	t.Helper()
	r := htStart(t, false)
	r.hpWitness()
	r.scan(nil)
	return r
}

// A launched terminal record (with and without a conversation) and a discovered
// record whose transcript is gone are purged once they stay absent for the
// default 7 days, each with one session_purge row written before it goes.
func TestHistoryPurgeAfterGrace(t *testing.T) {
	t.Parallel()
	r := hpStartTerm(t)
	witness := hxDiscoveredID(hxConv(900))

	linkedConv := hxConv(11)
	linked, _ := r.dormant(linkedConv, "Acme linked")
	bare := r.start("claude-code")
	r.closeTerm(bare)
	found := r.hpConsole(12, "found")
	r.scan(nil)
	foundID := r.hpID(found)

	hpD{path: osTranscriptPath(r.i.Home, r.dir, linkedConv)}.remove(t)
	found.remove(t)
	r.hpSettle()

	b := r.rows()
	for _, id := range []string{linked, bare, foundID} {
		if r.hpMissing(id) == "" {
			t.Fatalf("record %s has no missing_since after two scans without its transcript", id)
		}
	}
	if row := b.byID(linked); row == nil || row.Transcript != "absent" || row.Resume != "none" || hxStr(row.ResumeReason) != "transcript_absent" {
		t.Fatalf("linked terminal row = %+v, want transcript absent and resume none/transcript_absent", row)
	}
	if row := b.byID(foundID); row == nil || row.Transcript != "absent" {
		t.Fatalf("discovered row = %+v, want transcript absent", row)
	}
	if r.hpMissing(witness) != "" {
		t.Fatalf("the witness record has missing_since %s, want none: its transcript is present", r.hpMissing(witness))
	}

	// Inside the grace period nothing goes.
	r.hpJump(6 * hpDay)
	for _, id := range []string{linked, bare, foundID} {
		r.hpKept(id)
	}

	r.hpPurgeAll(2*hpDay, linked, bare, foundID)
	for _, id := range []string{linked, bare, foundID} {
		r.hpGone(id)
	}
	r.hpKept(witness)

	for _, c := range []struct {
		id, record, kind, conv string
	}{{linked, "launched", "pty", linkedConv}, {bare, "launched", "pty", ""}, {foundID, "discovered", "claude", found.conv}} {
		row := hpAudit(t, r.i, c.id)
		args, _ := row["args"].(map[string]any)
		actor, _ := row["actor"].(map[string]any)
		if row["outcome"] != "ok" || actor["kind"] != "service" {
			t.Fatalf("session_purge row for %s = %v, want outcome ok and a service actor", c.id, row)
		}
		if args["record"] != c.record || args["kind"] != c.kind || args["agent"] != "claude" || args["grace_days"] != float64(7) {
			t.Fatalf("session_purge args for %s = %v, want record %s kind %s agent claude grace_days 7", c.id, args, c.record, c.kind)
		}
		if got, _ := args["conversation_id"].(string); got != c.conv {
			t.Fatalf("session_purge conversation_id for %s = %q, want %q", c.id, got, c.conv)
		}
		if h, _ := args["host_id"].(string); h != "" {
			t.Fatalf("session_purge host_id for a console record = %q, want none", h)
		}
		if ms, _ := args["missing_since"].(string); ms == "" {
			t.Fatalf("session_purge args for %s = %v, want missing_since", c.id, args)
		} else {
			osParseTime(t, "missing_since", ms)
		}
	}
}

// A transcript that returns within the grace period clears missing_since, and
// the grace period then counts from the next absence.
func TestHistoryPurgeClearsWhenBack(t *testing.T) {
	t.Parallel()
	r := hpStartTerm(t)

	conv := hxConv(21)
	linked, _ := r.dormant(conv, "Acme back")
	found := r.hpConsole(22, "back")
	r.scan(nil)
	foundID := r.hpID(found)
	linkedPath := osTranscriptPath(r.i.Home, r.dir, conv)
	ids := []string{linked, foundID}

	vanish := func() {
		hpD{path: linkedPath}.remove(t)
		found.remove(t)
		r.hpSettle()
	}
	vanish()
	var first time.Time
	for _, id := range ids {
		ms := r.hpMissing(id)
		if ms == "" {
			t.Fatalf("record %s has no missing_since after its transcript went", id)
		}
		first = osParseTime(t, "missing_since", ms)
	}

	r.hpJump(5 * hpDay)
	r.transcript(r.dir, conv, "Acme back")
	hxSimple(t, r.i.Home, found.dir, found.conv, hxBase.Add(22*time.Minute), "acme purge")
	r.hpSettle()
	for _, id := range ids {
		if ms := r.hpMissing(id); ms != "" {
			t.Fatalf("record %s has missing_since %s after its transcript came back, want it cleared", id, ms)
		}
		if row := r.rows().byID(id); row == nil || row.Transcript != "present" {
			t.Fatalf("row %s = %+v after the transcript came back, want transcript present", id, row)
		}
	}

	// Eight days after the first absence, but present: nothing goes.
	r.hpJump(3 * hpDay)
	for _, id := range ids {
		r.hpKept(id)
	}

	// Absent again: the grace period counts from now, not from the first absence.
	vanish()
	for _, id := range ids {
		ms := r.hpMissing(id)
		if ms == "" {
			t.Fatalf("record %s has no missing_since after a second absence", id)
		}
		if at := osParseTime(t, "missing_since", ms); at.Sub(first) < 7*hpDay {
			t.Fatalf("record %s missing_since %s is not at least 7 days after the first %s, want the grace restarted", id, at, first)
		}
	}
	r.hpJump(6 * hpDay)
	for _, id := range ids {
		r.hpKept(id)
	}
	r.hpPurgeAll(2*hpDay, ids...)
	for _, id := range ids {
		r.hpGone(id)
	}
}

// Records the pass must skip, whatever the listing says, and an agent record,
// which is never a candidate. Each subtest holds a control that is purged in
// the same step.
func TestHistoryPurgeGuards(t *testing.T) {
	t.Parallel()

	// finish removes the control's transcript, passes the grace period and
	// checks that the guarded record stayed.
	finish := func(t *testing.T, h *hx, guarded string, unmarked bool) {
		t.Helper()
		control := h.hpConsole(31, "control")
		h.scan(nil)
		controlID := h.hpID(control)
		control.remove(t)
		h.hpSettle()
		if unmarked && h.hpMissing(guarded) != "" {
			t.Fatalf("guarded record %s has missing_since %s, want a skipped record left untouched", guarded, h.hpMissing(guarded))
		}
		h.hpPurgeAll(8*hpDay, controlID)
		h.hpSettle()
		h.hpKept(guarded)
		if unmarked && h.hpMissing(guarded) != "" {
			t.Fatalf("guarded record %s has missing_since %s after the grace period, want none", guarded, h.hpMissing(guarded))
		}
	}

	t.Run("live", func(t *testing.T) {
		t.Parallel()
		r := hpStartTerm(t)
		live := r.start("claude-code") // no conversation: absent by the pass's own rule
		r.hpSettle()
		finish(t, r.hx, live, true)
		if row := r.rows().byID(live); row == nil || row.State != "live" {
			t.Fatalf("live terminal row = %+v, want state live", row)
		}
	})

	t.Run("held", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		h.hpWitness()
		dir := osDir(t, t.TempDir(), "held")
		conv := hxConv(41)
		out := htOutsider(t, dir, conv, "acme-held-marker")
		out.register(h.i.Home, "idle", time.Now())
		d := h.hpPlant(h.i.Home, dir, 41)
		h.probe()
		h.hpSettle()
		row := h.rows().byConv(conv)
		if row == nil || row.Record != "discovered" || row.Resume != "none" || hxStr(row.ResumeReason) != "held" {
			t.Fatalf("row of a conversation a live process holds = %+v, want a discovered row held", row)
		}
		d.remove(t)
		h.hpSettle()
		finish(t, h, row.ID, true)
	})

	t.Run("managed", func(t *testing.T) {
		t.Parallel()
		r := hpStartTerm(t)
		sid := startAgentSession(t, r.i, "claude-code", "sonnet")
		// Signal: the reply, so the session has started its conversation.
		r.i.MustCLI("session", "message", "--id", sid, "--text", "hi", "--json")
		managed := ""
		for _, argv := range g1lAgentStarts(r.i) {
			for n, a := range argv {
				if a == "--session-id" && n+1 < len(argv) {
					managed = argv[n+1]
				}
			}
		}
		if managed == "" {
			t.Fatalf("no start of the fake agent for session %s carries --session-id", sid)
		}
		tid := r.start("claude-code")
		pid := r.pidOf(tid)
		r.transcript(r.dir, managed, "Acme managed")
		r.link(tid, pid, managed)
		r.closeTerm(tid)
		if err := os.Remove(r.registryPath(pid)); err != nil {
			t.Fatalf("removing the registry entry: %v", err)
		}
		r.probe()
		r.scan(nil)
		if row := r.rows().byID(tid); row == nil || row.ConversationID == nil || *row.ConversationID != managed || row.State != "dormant" {
			t.Fatalf("terminal row = %+v, want a dormant record on the managed conversation %s", row, managed)
		}
		hpD{path: osTranscriptPath(r.i.Home, r.dir, managed)}.remove(t)
		r.hpSettle()
		finish(t, r.hx, tid, true)
	})

	t.Run("agent", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		h.hpWitness()
		h.scan(nil)
		sid := startAgentSession(t, h.i, "claude-code", "sonnet")
		conv := hxConv(61)
		path := filepath.Join(h.i.ConfigDir, g1rLedger)
		var template map[string]any
		for _, rec := range h.hpRecs() {
			if rec["session_id"] == sid {
				template = rec
			}
		}
		if template == nil {
			t.Fatalf("ledger holds no record for session %s", sid)
		}
		// A stopped session leaves no record, and a live one survives a restart,
		// so the dormant agent record with a conversation (no probe links an
		// agent record in this release) is written while relay is down, from the
		// live record's own fields. The restart is before the clock moves.
		h.i.MustCLI("session", "stop", "--id", sid, "--json")
		h.i.Stop()
		var recs []map[string]any
		if err := json.Unmarshal(mustRead(t, path), &recs); err != nil {
			t.Fatalf("ledger does not decode: %v", err)
		}
		sid = "acme-agent-record"
		template["session_id"], template["state"], template["conversation_id"] = sid, "dormant", conv
		out, _ := json.Marshal(append(recs, template))
		osWrite(t, path, out)
		since := time.Now()
		h.i.Restart()
		h.i.WaitSessionHost(osBound)
		h.i.WaitEvent(harness.EventQuery{Key: hpScanKey, Since: since, Fields: map[string]any{"machine": "console"}}, osBound)
		h.hpSettle()

		row := h.rows().byID(sid)
		if row == nil || row.Record != "launched" || row.ConversationID == nil || *row.ConversationID != conv {
			t.Fatalf("agent row = %+v, want a launched row on conversation %s", row, conv)
		}
		if row.Transcript != "absent" {
			t.Fatalf("agent row = %+v, want transcript absent", row)
		}
		// The criterion names the resume answer; Errorf lets the kept check below run too.
		if row.Resume != "none" || hxStr(row.ResumeReason) != "transcript_absent" {
			t.Errorf("agent row = %+v, want resume none/transcript_absent", row)
		}
		finish(t, h, sid, false)
		if row := h.rows().byID(sid); row == nil || row.Resume != "none" || hxStr(row.ResumeReason) != "transcript_absent" {
			t.Errorf("agent row after the grace period = %+v, want resume none/transcript_absent", row)
		}
	})
}

// A machine that did not answer is never passed: an unreachable host, a
// missing store root, an answered listing that holds no transcript, and a
// listing at its cap.
func TestHistoryPurgeNothingWhenMachineSilent(t *testing.T) {
	t.Parallel()

	// hostControl plants a host record that goes absent, as the control for a
	// console scenario.
	hostControl := func(t *testing.T, h *hx) (hpD, string) {
		t.Helper()
		in := osDir(t, filepath.Join(h.remote, "boxproj"), "in")
		h.hpPlant(h.remote, osDir(t, filepath.Join(h.remote, "boxproj"), "witness"), 901)
		c := h.hpPlant(h.remote, in, 71)
		h.scan(nil)
		return c, h.hpID(c)
	}

	t.Run("unreachable host", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, true)
		h.hpWitness()
		in := osDir(t, filepath.Join(h.remote, "boxproj"), "in")
		h.hpPlant(h.remote, osDir(t, filepath.Join(h.remote, "boxproj"), "witness"), 901)
		onHost := h.hpPlant(h.remote, in, 72)
		control := h.hpConsole(73, "control")
		h.scan(nil)
		hostID, controlID := h.hpID(onHost), h.hpID(control)

		onHost.remove(t)
		control.remove(t)
		h.hpSettle()
		if h.hpMissing(hostID) == "" || h.hpMissing(controlID) == "" {
			t.Fatalf("missing_since of the host record %q and the control %q, want both set while the host answered", h.hpMissing(hostID), h.hpMissing(controlID))
		}

		h.i.SSHDown(haTarget)
		tick := func() {
			since := time.Now()
			h.i.ClockAdvance(osProbeStep)
			h.i.WaitEvent(harness.EventQuery{Key: hpScanKey, Since: since, Fields: map[string]any{"machine": "console"}}, osBound)
			h.i.WaitEvent(harness.EventQuery{Key: hpScanKey, Since: since, Fields: map[string]any{"machine": h.hostID, "status": "error"}}, osBound)
		}
		tick()
		h.i.ClockAdvance(8 * hpDay)
		h.hpAwait(tick, controlID)
		tick()
		tick()
		h.hpKept(hostID)
	})

	t.Run("store root missing", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, true)
		console := h.hpConsole(74, "console")
		control, controlID := hostControl(t, h)
		h.scan(nil)
		consoleID := h.hpID(console)

		root := filepath.Join(h.i.Home, ".claude", "projects")
		if err := os.Rename(root, filepath.Join(t.TempDir(), "projects")); err != nil {
			t.Fatalf("moving the console store aside: %v", err)
		}
		control.remove(t)
		h.hpSettle()
		if got := h.i.Events(harness.EventQuery{Key: hpScanKey, Fields: map[string]any{"machine": "console", "answered": false}}); len(got) == 0 {
			t.Fatalf("no session.history.scan line with answered false for the missing console root")
		}

		h.hpPurgeAll(8*hpDay, controlID)
		h.hpSettle()
		h.hpKept(consoleID)
	})

	t.Run("empty listing", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, true)
		console := h.hpConsole(75, "console")
		control, controlID := hostControl(t, h)
		h.scan(nil)
		consoleID := h.hpID(console)

		console.remove(t) // the console's listing answers and holds no transcript
		control.remove(t)
		h.hpSettle()
		h.hpPurgeAll(8*hpDay, controlID)
		h.hpSettle()
		h.hpKept(consoleID)
	})

	t.Run("listing at its cap", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		h.hpWitness()
		a := h.hpConsole(76, "capped")
		h.scan(nil)
		id := h.hpID(a)

		a.remove(t)
		bulk := hpBulk(t, h.i.Home)
		h.scan(map[string]any{"answered": true, "complete": false})
		h.hpSettle()
		if ms := h.hpMissing(id); ms != "" {
			t.Fatalf("record %s has missing_since %s under a capped listing, want none", id, ms)
		}
		h.hpJump(8 * hpDay)
		h.hpKept(id)
		if ms := h.hpMissing(id); ms != "" {
			t.Fatalf("record %s has missing_since %s after the grace period under a capped listing, want none", id, ms)
		}

		// Control: with the cap gone the same record is absent and goes.
		if err := os.RemoveAll(bulk); err != nil {
			t.Fatalf("removing the bulk folder: %v", err)
		}
		h.hpSettle()
		if h.hpMissing(id) == "" {
			t.Fatalf("record %s has no missing_since once the listing is complete again", id)
		}
		h.hpPurgeAll(8*hpDay, id)
		h.hpGone(id)
	})
}

// A stretch in which the transcript's state is unknown (here a listing at its
// cap) breaks "stayed absent": missing_since clears and the grace restarts.
func TestHistoryPurgeUnknownRestartsGrace(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	h.hpWitness()
	a := h.hpConsole(81, "unknown")
	h.scan(nil)
	id := h.hpID(a)

	a.remove(t)
	h.hpSettle()
	ms := h.hpMissing(id)
	if ms == "" {
		t.Fatalf("record %s has no missing_since after its transcript went", id)
	}
	first := osParseTime(t, "missing_since", ms)

	h.hpJump(5 * hpDay)
	bulk := hpBulk(t, h.i.Home)
	h.scan(map[string]any{"complete": false})
	h.hpSettle()
	if ms := h.hpMissing(id); ms != "" {
		t.Fatalf("record %s has missing_since %s after an unknown stretch, want it cleared", id, ms)
	}

	if err := os.RemoveAll(bulk); err != nil {
		t.Fatalf("removing the bulk folder: %v", err)
	}
	h.hpSettle()
	ms = h.hpMissing(id)
	if ms == "" {
		t.Fatalf("record %s has no missing_since once the listing is complete again", id)
	}
	if second := osParseTime(t, "missing_since", ms); second.Sub(first) < 5*hpDay {
		t.Fatalf("missing_since %s is not at least 5 days after the first %s, want the grace restarted", second, first)
	}

	// Ten days after the first absence, five after the restart: kept.
	h.hpJump(5 * hpDay)
	h.hpKept(id)
	h.hpPurgeAll(3*hpDay, id)
	h.hpGone(id)
}

// A record whose folder the listing does not cover or refused is unknown, not
// absent: no missing_since, no purge.
func TestHistoryPurgeOutOfScope(t *testing.T) {
	t.Parallel()

	t.Run("host project removed", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, true)
		two := osDir(t, h.remote, "boxtwo")
		body, _ := json.Marshal(map[string]any{"name": "Acme Two", "path": two, "host_id": h.hostID, "allowed_templates": []string{"claude-code"}})
		if res := h.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json"); res.Code != 0 {
			t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
		}
		proj := filepath.Join(h.remote, "boxproj")
		present := h.hpPlant(h.remote, osDir(t, proj, "present"), 91)
		gone := h.hpPlant(h.remote, osDir(t, proj, "gone"), 92)
		control := h.hpPlant(h.remote, osDir(t, two, "control"), 93)
		h.hpPlant(h.remote, osDir(t, two, "witness"), 94)
		h.scan(nil)
		row := h.rows().byConv(present.conv)
		if row == nil || row.ProjectID == nil {
			t.Fatalf("host row = %+v, want a row in the host project", row)
		}
		presentID, goneID, controlID := h.hpID(present), h.hpID(gone), h.hpID(control)
		if res := h.i.CLI("project", "remove", "--id", *row.ProjectID, "--json"); res.Code != 0 {
			t.Fatalf("project remove exited %d\nstderr: %s", res.Code, res.Stderr)
		}
		h.scan(nil)

		gone.remove(t) // its folder is out of scope, so this file is not known to be absent
		control.remove(t)
		h.hpSettle()
		for _, id := range []string{presentID, goneID} {
			if r := h.rows().byID(id); r == nil || r.Transcript != "unknown" {
				t.Fatalf("row %s = %+v after its project went, want transcript unknown", id, r)
			}
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s, want none outside the listing's scope", id, ms)
			}
		}
		h.hpPurgeAll(8*hpDay, controlID)
		h.hpSettle()
		for _, id := range []string{presentID, goneID} {
			h.hpKept(id)
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s after the grace period, want none", id, ms)
			}
		}
	})

	t.Run("symbolic links", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		h.hpWitness()
		folder := h.hpConsole(95, "linkedfolder")
		file := h.hpConsole(96, "linkedfile")
		control := h.hpConsole(97, "control")
		h.scan(nil)
		folderID, fileID, controlID := h.hpID(folder), h.hpID(file), h.hpID(control)

		stash := t.TempDir()
		moved := filepath.Join(stash, "folder")
		if err := os.Rename(filepath.Dir(folder.path), moved); err != nil {
			t.Fatalf("moving the folder: %v", err)
		}
		if err := os.Symlink(moved, filepath.Dir(folder.path)); err != nil {
			t.Fatalf("linking the folder: %v", err)
		}
		real := filepath.Join(stash, "file.jsonl")
		if err := os.Rename(file.path, real); err != nil {
			t.Fatalf("moving the transcript: %v", err)
		}
		if err := os.Symlink(real, file.path); err != nil {
			t.Fatalf("linking the transcript: %v", err)
		}
		control.remove(t)
		h.hpSettle()
		for _, id := range []string{folderID, fileID} {
			if r := h.rows().byID(id); r == nil || r.Transcript != "unknown" {
				t.Fatalf("row %s = %+v behind a link, want transcript unknown", id, r)
			}
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s behind a link, want none", id, ms)
			}
		}
		h.hpPurgeAll(8*hpDay, controlID)
		h.hpSettle()
		for _, id := range []string{folderID, fileID} {
			h.hpKept(id)
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s behind a link after the grace period, want none", id, ms)
			}
		}
	})
}

// With auditing off nothing is marked or purged. The control is the same
// scenario with auditing on.
func TestHistoryPurgeNothingWithAuditingOff(t *testing.T) {
	t.Parallel()
	for _, auditOn := range []bool{false, true} {
		name := "auditing off"
		settings := map[string]string{"audit": `{"enabled":false}`}
		if auditOn {
			name, settings = "auditing on", nil
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := hpBoot(t, settings)
			h.hpWitness()
			d := h.hpConsole(101, "found")
			h.scan(nil)
			id := h.hpID(d)
			d.remove(t)
			h.hpSettle()
			if auditOn {
				if h.hpMissing(id) == "" {
					t.Fatalf("record %s has no missing_since with auditing on", id)
				}
				h.hpPurgeAll(8*hpDay, id)
				h.hpGone(id)
				hpAudit(t, h.i, id)
				return
			}
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s with auditing off, want none", id, ms)
			}
			h.hpJump(8 * hpDay)
			h.hpSettle()
			h.hpKept(id)
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s with auditing off after the grace period, want none", id, ms)
			}
		})
	}
}

// session_history.purge_after_days: default 7 (TestHistoryPurgeAfterGrace), 0
// off, a value outside 0 to 90 off with one Warn per change, a set value used.
func TestHistoryPurgeSetting(t *testing.T) {
	t.Parallel()

	// absentRecord boots with settings, lists one discovered record and removes its transcript.
	absentRecord := func(t *testing.T, settings map[string]string) (*hx, string) {
		t.Helper()
		h := hpBoot(t, settings)
		h.hpWitness()
		d := h.hpConsole(111, "found")
		h.scan(nil)
		id := h.hpID(d)
		d.remove(t)
		h.hpSettle()
		return h, id
	}

	for _, c := range []struct{ name, raw string }{{"zero turns purge off", "0"}, {"above 90 turns purge off", "91"}, {"negative turns purge off", "-1"}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, id := absentRecord(t, map[string]string{"session_history": `{"purge_after_days":` + c.raw + `}`})
			if ms := h.hpMissing(id); ms != "" {
				t.Fatalf("record %s has missing_since %s with purge_after_days %s, want none", id, ms, c.raw)
			}
			h.hpJump(10 * hpDay)
			h.hpSettle()
			h.hpKept(id)
		})
	}

	t.Run("a set value is the grace period", func(t *testing.T) {
		t.Parallel()
		h, id := absentRecord(t, map[string]string{"session_history": `{"purge_after_days":1}`})
		if h.hpMissing(id) == "" {
			t.Fatalf("record %s has no missing_since with purge_after_days 1", id)
		}
		h.hpJump(12 * time.Hour)
		h.hpKept(id)
		h.hpPurgeAll(2*hpDay, id)
		h.hpGone(id)
	})

	t.Run("a hand edit applies and an out-of-range value warns once per change", func(t *testing.T) {
		t.Parallel()
		h, id := absentRecord(t, nil)
		warns := func() int {
			return len(h.i.Events(harness.EventQuery{Fields: map[string]any{"level": "warn"}}))
		}
		n0 := warns()
		for k, raw := range []string{"91", "100"} {
			hpSetDays(t, h.i, raw)
			// A plain log line has no event key to wait on, so the count is polled to its bound.
			g11dUntil(t, "the warn line for purge_after_days "+raw, func() bool { return warns() > n0+k })
			h.hpSettle()
			if got := warns(); got != n0+k+1 {
				t.Fatalf("%d warn lines after setting purge_after_days %s, want %d: one per change, not one per scan", got, raw, n0+k+1)
			}
		}
		// The record was marked under the default; the edit turned purge off.
		h.hpJump(10 * hpDay)
		h.hpSettle()
		h.hpKept(id)
	})
}
