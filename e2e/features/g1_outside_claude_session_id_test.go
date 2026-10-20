package features

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These tests read claudeSessionId on outside rows through the list, the
// detail and the /ws/outside-sessions snapshot. A probe round is the signal
// that planted transcripts were scanned: the WS subscription is opened before
// the clock moves, and the frame read is the first one in which every
// machine's lastProbeAt moved.

const osClaudeIDKey = "claudeSessionId"

// probeFrame is probe() that also returns the frame's sessions as untyped
// maps, so a test can see which keys each row carries.
func (r *osRig) probeFrame() (osSnap, []map[string]any) {
	r.t.Helper()
	sub := osSubscribe(r.t, r.i)
	before := map[string]string{}
	for _, m := range sub.until(osBound, func(osSnap) bool { return true }).Machines {
		before[m.Machine] = m.LastProbeAt
	}
	r.i.ClockAdvance(osProbeStep)
	end := time.Now().Add(osBound)
	for {
		frame := sub.ws.Next(time.Until(end))
		var snap osSnap
		if err := json.Unmarshal(frame, &snap); err != nil {
			r.t.Fatalf("decoding a snapshot: %v", err)
		}
		moved := true
		for _, m := range snap.Machines {
			if m.LastProbeAt == before[m.Machine] {
				moved = false
			}
		}
		if !moved {
			continue
		}
		var raw struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if err := json.Unmarshal(frame, &raw); err != nil {
			r.t.Fatalf("decoding snapshot rows: %v", err)
		}
		return snap, raw.Sessions
	}
}

func osRowByID(rows []map[string]any, id string) map[string]any {
	for _, row := range rows {
		if row["id"] == id {
			return row
		}
	}
	return nil
}

// detailSession reads the detail of id and returns its session object.
func (r *osRig) detailSession(id string) map[string]any {
	r.t.Helper()
	resp := r.detail(id)
	if resp.Status != http.StatusOK {
		r.t.Fatalf("GET detail of %s answered %d, want 200: %s", id, resp.Status, resp.Body)
	}
	var body struct {
		Session map[string]any `json:"session"`
	}
	resp.JSON(r.t, &body)
	if body.Session == nil {
		r.t.Fatalf("detail of %s has no session object: %s", id, resp.Body)
	}
	return body.Session
}

// AC1: an ended row in the list carries claudeSessionId, the transcript file
// stem, on a console row and on a host row.
func TestOutsideEndedListCarriesClaudeSessionID(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	cdir := osDir(t, t.TempDir(), "console-done")
	hdir := osDir(t, filepath.Join(r.remote, "boxproj"), "host-done")
	const cStem, hStem = "acme-console-stem", "acme-host-stem"
	r.ended(r.i.Home, cdir, cStem, 5*osEndedHour, "acme console answer")
	r.ended(r.remote, hdir, hStem, 6*osEndedHour, "acme host answer")
	r.probe()

	want := map[string]string{cdir: cStem, hdir: hStem}
	seen := 0
	for _, row := range r.endedList() {
		stem, ok := want[row["directory"].(string)]
		if !ok || row["state"] != "ended" {
			continue
		}
		seen++
		if row[osClaudeIDKey] != stem {
			t.Fatalf("ended row for %v: %s = %v, want %q", row["directory"], osClaudeIDKey, row[osClaudeIDKey], stem)
		}
	}
	if seen != len(want) {
		t.Fatalf("list held %d of the %d planted ended rows", seen, len(want))
	}
}

// AC2: the detail of an ended id and the WS snapshot carry the same value on
// the same row id, console and host.
func TestOutsideEndedDetailAndWSCarryClaudeSessionID(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	cdir := osDir(t, t.TempDir(), "console-done")
	hdir := osDir(t, filepath.Join(r.remote, "boxproj"), "host-done")
	const cStem, hStem = "acme-console-stem", "acme-host-stem"
	r.ended(r.i.Home, cdir, cStem, 5*osEndedHour, "acme console answer")
	r.ended(r.remote, hdir, hStem, 6*osEndedHour, "acme host answer")
	s, frame := r.probeFrame()

	for _, w := range []struct{ dir, stem string }{{cdir, cStem}, {hdir, hStem}} {
		row := s.endedAt(w.dir)
		if row == nil {
			t.Fatalf("no ended row for %s; snapshot %+v", w.dir, s)
		}
		if got := r.detailSession(row.ID)[osClaudeIDKey]; got != w.stem {
			t.Fatalf("detail of %s: session.%s = %v, want %q", row.ID, osClaudeIDKey, got, w.stem)
		}
		wsRow := osRowByID(frame, row.ID)
		if wsRow == nil {
			t.Fatalf("WS frame has no row %s", row.ID)
		}
		if wsRow[osClaudeIDKey] != w.stem {
			t.Fatalf("WS row %s: %s = %v, want %q", row.ID, osClaudeIDKey, wsRow[osClaudeIDKey], w.stem)
		}
	}
}

// AC3: a live outside process row never carries claudeSessionId, on the list,
// the detail and the WS snapshot; an ended row of another session in the same
// list does.
func TestOutsideLiveRowHasNoClaudeSessionID(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	p := r.spawn(r.i.Home, root, "live")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	edir := osDir(t, root, "done")
	r.ended(r.i.Home, edir, "acme-done-stem", 5*osEndedHour, "acme answer")

	s, frame := r.probeFrame()
	live := s.liveAt(p.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", p.dir, s)
	}
	if row := osRowByID(frame, live.ID); row == nil {
		t.Fatalf("WS frame has no live row %s", live.ID)
	} else if _, has := row[osClaudeIDKey]; has {
		t.Fatalf("WS live row carries %s = %v", osClaudeIDKey, row[osClaudeIDKey])
	}
	if got := r.detailSession(live.ID); got["state"] == "ended" {
		t.Fatalf("detail of the live id %s answered an ended session", live.ID)
	} else if _, has := got[osClaudeIDKey]; has {
		t.Fatalf("detail live session carries %s = %v", osClaudeIDKey, got[osClaudeIDKey])
	}
	sawLive, sawEnded := false, false
	for _, row := range r.endedList() {
		_, has := row[osClaudeIDKey]
		switch row["state"] {
		case "ended":
			sawEnded = true
			if !has {
				t.Fatalf("ended row %v has no %s", row["id"], osClaudeIDKey)
			}
		default:
			sawLive = true
			if has {
				t.Fatalf("live list row %v carries %s", row["id"], osClaudeIDKey)
			}
		}
	}
	if !sawLive || !sawEnded {
		t.Fatalf("list held live %v ended %v, want both", sawLive, sawEnded)
	}
}

// AC3: the same Claude session, ended and then resumed in a new process, shows
// once as a live row with no claudeSessionId on any door.
func TestOutsideResumedSessionShowsOnceWithoutClaudeSessionID(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	p := r.spawn(r.i.Home, root, "live")
	plant := func() {
		now := r.i.Clock().Now
		osPlantAt(t, otFile(p), osConversation(p.sid, p.dir, "acme-branch", now, "acme one", "acme done"), now)
	}
	plant()
	r.probe()
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	<-p.exit
	s := r.probe()
	ended := s.endedAt(p.dir)
	if ended == nil {
		t.Fatalf("after exit: no ended row for %s; snapshot %+v", p.dir, s)
	}
	if got := r.detailSession(ended.ID)[osClaudeIDKey]; got != p.sid {
		t.Fatalf("ended detail: %s = %v, want %q", osClaudeIDKey, got, p.sid)
	}

	r.spawn(r.i.Home, root, "live")
	plant()
	s, frame := r.probeFrame()
	live := s.liveAt(p.dir)
	rows := 0
	for _, row := range frame {
		if row["directory"] == p.dir {
			rows++
			if _, has := row[osClaudeIDKey]; has {
				t.Fatalf("resumed row %v carries %s", row["id"], osClaudeIDKey)
			}
		}
	}
	if live == nil || rows != 1 {
		t.Fatalf("after resume: live row %+v, %d WS rows for %s, want one live row", live, rows, p.dir)
	}
	for _, row := range r.endedList() {
		if row["directory"] == p.dir {
			if _, has := row[osClaudeIDKey]; has {
				t.Fatalf("resumed list row %v carries %s", row["id"], osClaudeIDKey)
			}
		}
	}
	if _, has := r.detailSession(live.ID)[osClaudeIDKey]; has {
		t.Fatalf("resumed detail carries %s", osClaudeIDKey)
	}
}
