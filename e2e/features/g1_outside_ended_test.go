package features

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The ended-row tests plant transcript files under the console's HOME and the
// stub host's root, set their modification times against the instance clock,
// and read the scoped list after a probe round. Files are planted before the
// probe; the probe's snapshot (every machine's lastProbeAt moved) carries the
// ended scan, because the baseline publishes once.

const (
	osEndedHour = time.Hour
	osEndedDay  = 24 * time.Hour
)

// endedList reads the scoped list over HTTP and returns each session as a map,
// so a test can see which keys a row carries.
func (r *osRig) endedList() []map[string]any {
	r.t.Helper()
	resp := cosDo(r.t, r.i, "GET", osListPath, nil)
	if resp.Status != http.StatusOK {
		r.t.Fatalf("GET %s answered %d, want 200: %s", osListPath, resp.Status, resp.Body)
	}
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	resp.JSON(r.t, &body)
	return body.Sessions
}

func osParseTime(t *testing.T, field, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("%s = %q, want an RFC 3339 time: %v", field, value, err)
	}
	return at
}

func osNear(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Second && d < time.Second
}

func osDirs(rows []osRow) []string {
	out := make([]string, len(rows))
	for k, row := range rows {
		out[k] = row.Directory
	}
	return out
}

// AC1, AC4: a session whose file changed inside the window is listed as an
// ended row with the live row fields plus endedAt, on the console and a host.
func TestOutsideEndedRowListed(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	cdir := osDir(t, t.TempDir(), "console-done")
	hdir := osDir(t, filepath.Join(r.remote, "boxproj"), "host-done")
	_, cTime := r.ended(r.i.Home, cdir, "acme-console-done", 5*osEndedHour, "acme console answer")
	_, hTime := r.ended(r.remote, hdir, "acme-host-done", 6*osEndedHour, "acme host answer")
	live := r.spawn(r.i.Home, t.TempDir(), "still-live")
	live.transcript(t, osPrompt("acme one"), osAnswer("acme done"))

	s := r.probe()
	for _, w := range []struct {
		dir     string
		mtime   time.Time
		machine string
		host    bool
	}{{cdir, cTime, "Console", false}, {hdir, hTime, "testbox", true}} {
		row := s.endedAt(w.dir)
		if row == nil {
			t.Fatalf("no ended row for %s; snapshot %+v", w.dir, s)
		}
		if row.State != "ended" || row.Source != "probe" || !row.Outside || !row.ReadOnly || row.Machine != w.machine ||
			row.Branch != "acme-branch" || row.TmuxSession != nil || row.Viewable || row.ViewReason != "not_in_tmux" || (row.HostID != nil) != w.host {
			t.Fatalf("ended row for %s = %+v, want the read-only probe row fields of machine %s (hostId set %v)", w.dir, row, w.machine, w.host)
		}
		if !strings.HasPrefix(row.ID, "outside:") {
			t.Fatalf("ended row id %q, want an outside: id", row.ID)
		}
		if got := osParseTime(t, "endedAt", row.EndedAt); !osNear(got, w.mtime) {
			t.Fatalf("endedAt = %s, want the file's modification time %s", row.EndedAt, w.mtime.Format(time.RFC3339))
		}
	}
	if lr := s.liveAt(live.dir); lr == nil || lr.EndedAt != "" {
		t.Fatalf("live row = %+v, want a row with no endedAt", lr)
	}

	// since is the earliest transcript timestamp, an estimate; live rows carry no endedAt key.
	for _, raw := range r.endedList() {
		_, has := raw["endedAt"]
		switch raw["state"] {
		case "ended":
			if !has {
				t.Fatalf("ended row %v has no endedAt", raw["id"])
			}
			if raw["directory"] == cdir {
				since := osParseTime(t, "since", raw["since"].(string))
				if !osNear(since, cTime.Add(-2*time.Minute)) {
					t.Fatalf("since = %v, want the earliest record time %s", raw["since"], cTime.Add(-2*time.Minute).Format(time.RFC3339))
				}
				osParseTime(t, "updatedAt", raw["updatedAt"].(string))
			}
		default:
			if has {
				t.Fatalf("live row %v carries endedAt", raw["id"])
			}
		}
	}

	// The id is stable across probes.
	first := s.endedAt(cdir).ID
	if again := r.probe().endedAt(cdir); again == nil || again.ID != first {
		t.Fatalf("ended row after another probe = %+v, want id %s", again, first)
	}
}

// AC2: the window defaults to 3 days, and a row older than the window leaves
// the list when the clock passes it.
func TestOutsideEndedWindowDefault(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	in, out := osDir(t, root, "two-days"), osDir(t, root, "four-days")
	r.ended(r.i.Home, in, "acme-two-days", 2*osEndedDay, "acme in window")
	r.ended(r.i.Home, out, "acme-four-days", 4*osEndedDay, "acme out of window")

	s := r.probe()
	if s.endedAt(in) == nil {
		t.Fatalf("no ended row for the 2-day-old file; snapshot %+v", s)
	}
	if row := s.endedAt(out); row != nil {
		t.Fatalf("ended row for the 4-day-old file = %+v, want none: the default window is 3 days", row)
	}

	// Signal: a snapshot without the row, once the injected clock is past its window.
	sub := osSubscribe(t, r.i)
	r.i.ClockAdvance(36 * osEndedHour)
	sub.until(osBound, func(s osSnap) bool { return s.endedAt(in) == nil })
}

// AC2: outside_sessions.ended_window_days sets the window; 0 turns ended rows
// off; a value above 30 lists none.
func TestOutsideEndedWindowSetting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		days     int
		inAge    time.Duration // 0: no file expected to list
		outAge   time.Duration
		wantNone bool
	}{
		{name: "one day", days: 1, inAge: 12 * osEndedHour, outAge: 2 * osEndedDay},
		{name: "thirty days", days: 30, inAge: 20 * osEndedDay, outAge: 40 * osEndedDay},
		{name: "zero is off", days: 0, outAge: osEndedHour, wantNone: true},
		{name: "thirty one lists none", days: 31, outAge: osEndedHour, wantNone: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := osStart(t, false, osEndedWindow(c.days))
			root := t.TempDir()
			inDir, outDir := osDir(t, root, "inside"), osDir(t, root, "outside")
			if c.inAge > 0 {
				r.ended(r.i.Home, inDir, "acme-inside", c.inAge, "acme inside")
			}
			r.ended(r.i.Home, outDir, "acme-outside", c.outAge, "acme outside")
			s := r.probe()
			if c.inAge > 0 && s.endedAt(inDir) == nil {
				t.Fatalf("window %d days: no ended row for the file %s old; snapshot %+v", c.days, c.inAge, s)
			}
			if row := s.endedAt(outDir); row != nil {
				t.Fatalf("window %d days: ended row for the file %s old = %+v, want none", c.days, c.outAge, row)
			}
			if c.wantNone && len(s.endedRows()) != 0 {
				t.Fatalf("window %d days: ended rows = %+v, want none", c.days, s.endedRows())
			}
		})
	}
}

// AC3: at most 50 ended rows across all machines, newest first.
func TestOutsideEndedCapAndOrder(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	croot := t.TempDir()
	const total = 60
	want := make([]string, 0, total)
	for k := 0; k < total; k++ {
		name := fmt.Sprintf("cap-%02d", k)
		age := time.Duration(k+1) * 20 * time.Minute
		if k%2 == 0 {
			dir := osDir(t, croot, name)
			r.ended(r.i.Home, dir, "acme-"+name, age, "acme answer")
			want = append(want, dir)
		} else {
			dir := osDir(t, filepath.Join(r.remote, "boxproj"), name)
			r.ended(r.remote, dir, "acme-"+name, age, "acme answer")
			want = append(want, dir)
		}
	}
	rows := r.probe().endedRows()
	if !slices.Equal(osDirs(rows), want[:50]) {
		t.Fatalf("ended rows = %v\nwant the 50 newest, newest first: %v", osDirs(rows), want[:50])
	}
	for k := 1; k < len(rows); k++ {
		prev, cur := osParseTime(t, "endedAt", rows[k-1].EndedAt), osParseTime(t, "endedAt", rows[k].EndedAt)
		if cur.After(prev) {
			t.Fatalf("row %d endedAt %s is newer than row %d endedAt %s", k, rows[k].EndedAt, k-1, rows[k-1].EndedAt)
		}
	}
}

// AC5: the detail of an ended id shows the last turn from the file, at most 8192 characters.
func TestOutsideEndedDetailLastTurn(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	shortDir, longDir := osDir(t, root, "short"), osDir(t, root, "long")
	r.ended(r.i.Home, shortDir, "acme-short", osEndedHour, "acme short answer")
	long := strings.Repeat("x", 9000) + "tail"
	r.ended(r.i.Home, longDir, "acme-long", 2*osEndedHour, long)
	s := r.probe()

	type detail struct {
		Session  osRow `json:"session"`
		LastTurn struct {
			Text     string `json:"text"`
			Complete bool   `json:"complete"`
		} `json:"lastTurn"`
		LastExcerpt string `json:"lastExcerpt"`
	}
	read := func(dir string) detail {
		row := s.endedAt(dir)
		if row == nil {
			t.Fatalf("no ended row for %s", dir)
		}
		resp := r.detail(row.ID)
		if resp.Status != http.StatusOK {
			t.Fatalf("detail of the ended row for %s answered %d, want 200: %s", dir, resp.Status, resp.Body)
		}
		var d detail
		resp.JSON(t, &d)
		if d.Session.ID != row.ID || d.Session.State != "ended" {
			t.Fatalf("detail session = %+v, want the ended row %s", d.Session, row.ID)
		}
		return d
	}
	if d := read(shortDir); d.LastTurn.Text != "acme short answer" || d.LastExcerpt != d.LastTurn.Text || !d.LastTurn.Complete {
		t.Fatalf("short detail = %+v, want the complete answer as turn and excerpt", d)
	}
	d := read(longDir)
	runes := []rune(d.LastTurn.Text)
	if len(runes) != 8192 || !strings.HasSuffix(d.LastTurn.Text, "tail") {
		t.Fatalf("long detail turn has %d characters, want the last 8192 ending in tail", len(runes))
	}
	if ex := []rune(d.LastExcerpt); len(ex) != 500 || !strings.HasSuffix(d.LastExcerpt, "tail") {
		t.Fatalf("long detail excerpt has %d characters, want the last 500 ending in tail", len(ex))
	}
	resp := r.detail("outside:unknown")
	if resp.Status != http.StatusNotFound || !strings.Contains(string(resp.Body), "outside_session_not_found") {
		t.Fatalf("detail for an unknown id answered %d %s, want 404 outside_session_not_found", resp.Status, resp.Body)
	}
}

// AC6: a live session is never also ended; once its process is gone it is one
// ended row with a new id, and a resumed session shows once, as live.
func TestOutsideEndedNotListedWhileLive(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	p := r.spawn(r.i.Home, root, "live")
	plant := func() {
		now := r.i.Clock().Now
		osPlantAt(t, otFile(p), osConversation(p.sid, p.dir, "acme-branch", now, "acme one", "acme done"), now)
	}
	plant()
	s := r.probe()
	liveRow := s.liveAt(p.dir)
	if liveRow == nil || s.endedAt(p.dir) != nil || len(s.endedRows()) != 0 {
		t.Fatalf("live session: live row %+v, ended rows %+v, want one live row and no ended row", liveRow, s.endedRows())
	}

	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	<-p.exit
	s = r.probe()
	ended := s.endedAt(p.dir)
	if s.liveAt(p.dir) != nil || ended == nil || len(s.endedRows()) != 1 {
		t.Fatalf("after exit: snapshot %+v, want one ended row and no live row for %s", s, p.dir)
	}
	if ended.ID == liveRow.ID {
		t.Fatalf("ended row id %s equals the live id", ended.ID)
	}

	// The same Claude session resumed in a new process.
	p2 := r.spawn(r.i.Home, root, "live")
	plant()
	s = r.probe()
	rows := 0
	for _, row := range s.Sessions {
		if row.Directory == p.dir {
			rows++
		}
	}
	if s.liveAt(p.dir) == nil || rows != 1 || len(s.endedRows()) != 0 {
		t.Fatalf("after resume (pid %d): snapshot %+v, want exactly one live row for %s and no ended row", p2.pid, s, p.dir)
	}
}

// osBigRecord is a transcript record padded to about size bytes.
func osBigRecord(sid, cwd string, at time.Time, size int) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at.UTC().Format(time.RFC3339Nano), "sessionId": sid, "cwd": cwd, "gitBranch": "acme-branch",
		"pad": strings.Repeat("x", size), "message": map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "acme big"}}}})
	return string(b)
}

// AC7: the list reads a 16 KiB tail, the detail a 1 MiB tail, and neither
// follows a symlink.
func TestOutsideEndedTailBounds(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	home := r.i.Home
	now := r.i.Clock().Now
	mtime := now.Add(-osEndedHour).Truncate(time.Second)

	// A final record over 16 KiB leaves no complete record in the list tail; one under it does.
	bigDir, fitDir := osDir(t, root, "big-record"), osDir(t, root, "fit-record")
	for _, w := range []struct {
		dir, sid string
		size     int
	}{{bigDir, "acme-big-record", 17 * 1024}, {fitDir, "acme-fit-record", 15*1024 - 512}} {
		lines := append(osConversation(w.sid, w.dir, "acme-branch", mtime, "acme one", "acme two"), osBigRecord(w.sid, w.dir, mtime, w.size))
		osPlantAt(t, osTranscriptPath(home, w.dir, w.sid), lines, mtime)
	}

	// A symlinked file and a symlinked folder give no row.
	elsewhere := t.TempDir()
	fileDir, folderDir := osDir(t, root, "symlink-file"), osDir(t, root, "symlink-folder")
	realFile := filepath.Join(elsewhere, "acme-symlink-file.jsonl")
	osPlantAt(t, realFile, osConversation("acme-symlink-file", fileDir, "acme-branch", mtime, "acme one", "acme two"), mtime)
	linkPath := osTranscriptPath(home, fileDir, "acme-symlink-file")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFile, linkPath); err != nil {
		t.Fatal(err)
	}
	realFolder := filepath.Join(elsewhere, "folder")
	osPlantAt(t, filepath.Join(realFolder, "acme-symlink-folder.jsonl"), osConversation("acme-symlink-folder", folderDir, "acme-branch", mtime, "acme one", "acme two"), mtime)
	folderLink := filepath.Dir(osTranscriptPath(home, folderDir, "acme-symlink-folder"))
	if err := os.MkdirAll(filepath.Dir(folderLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFolder, folderLink); err != nil {
		t.Fatal(err)
	}

	// The detail reads a 1 MiB tail: a prompt within it gives the turn, a prompt beyond it gives none.
	nearDir, farDir := osDir(t, root, "prompt-near"), osDir(t, root, "prompt-far")
	for _, w := range []struct {
		dir, sid string
		filler   int
	}{{nearDir, "acme-prompt-near", 6}, {farDir, "acme-prompt-far", 13}} {
		lines := []string{osRec("user", map[string]any{"role": "user", "content": "acme genuine prompt"}, w.sid, w.dir, "acme-branch", mtime.Add(-time.Hour))}
		for k := 0; k < w.filler; k++ {
			lines = append(lines, osBigRecord(w.sid, w.dir, mtime.Add(-time.Hour+time.Duration(k)*time.Second), 100*1024))
		}
		lines = append(lines, osRec("assistant", map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "acme tail marker"}}}, w.sid, w.dir, "acme-branch", mtime))
		osPlantAt(t, osTranscriptPath(home, w.dir, w.sid), lines, mtime)
	}

	s := r.probe()
	for _, none := range []string{bigDir, fileDir, folderDir} {
		if row := s.endedAt(none); row != nil {
			t.Fatalf("ended row for %s = %+v, want none", none, row)
		}
	}
	for _, dir := range []string{fitDir, nearDir, farDir} {
		if s.endedAt(dir) == nil {
			t.Fatalf("no ended row for %s; snapshot %+v", dir, s)
		}
	}
	turn := func(dir string) string {
		resp := r.detail(s.endedAt(dir).ID)
		if resp.Status != http.StatusOK {
			t.Fatalf("detail for %s answered %d, want 200: %s", dir, resp.Status, resp.Body)
		}
		var d struct {
			LastTurn struct{ Text string } `json:"lastTurn"`
		}
		resp.JSON(t, &d)
		return d.LastTurn.Text
	}
	if got := turn(nearDir); !strings.HasSuffix(got, "acme tail marker") {
		t.Fatalf("turn with the prompt inside the 1 MiB tail = %q, want text ending in the marker", got)
	}
	if got := turn(farDir); got != "" {
		t.Fatalf("turn with the prompt beyond the 1 MiB tail = %q, want empty", got)
	}
}

// AC8: a host is scanned only under its registered projects; the console is
// scanned under all its own project folders.
func TestOutsideEndedScanScope(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	console := osDir(t, t.TempDir(), "no-project")
	inside := osDir(t, filepath.Join(r.remote, "boxproj"), "inside")
	outside := osDir(t, filepath.Join(r.remote, "elsewhere"), "outside")
	sibling := osDir(t, filepath.Join(r.remote, "boxproj-sibling"), "outside")
	r.ended(r.i.Home, console, "acme-console", osEndedHour, "acme console")
	r.ended(r.remote, inside, "acme-inside", osEndedHour, "acme inside")
	r.ended(r.remote, outside, "acme-outside", osEndedHour, "acme outside")
	r.ended(r.remote, sibling, "acme-sibling", osEndedHour, "acme sibling")

	s := r.probe()
	if row := s.endedAt(console); row == nil || row.HostID != nil || row.ProjectID != nil {
		t.Fatalf("console row for a folder under no project = %+v, want a row with no hostId and no projectId", row)
	}
	if row := s.endedAt(inside); row == nil || row.HostID == nil || row.ProjectID == nil {
		t.Fatalf("host row inside the project = %+v, want a row with hostId and projectId", row)
	}
	for _, dir := range []string{outside, sibling} {
		if row := s.endedAt(dir); row != nil {
			t.Fatalf("ended row for the host folder %s outside the host project = %+v, want none", dir, row)
		}
	}
}

// AC9: a missing or unreadable file drops its own row and never the list.
func TestOutsideEndedBadFileDropsRow(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	home := r.i.Home
	mtime := r.i.Clock().Now.Add(-osEndedHour).Truncate(time.Second)
	good := osDir(t, root, "good")
	r.ended(home, good, "acme-good", osEndedHour, "acme good")

	denied := osDir(t, root, "denied")
	deniedPath, _ := r.ended(home, denied, "acme-denied", osEndedHour, "acme denied")
	if err := os.Chmod(deniedPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deniedPath, 0o600) })

	garbled := osDir(t, root, "garbled")
	osPlantAt(t, osTranscriptPath(home, garbled, "acme-garbled"), []string{"not json {{{", "}{"}, mtime)
	empty := osDir(t, root, "empty")
	osPlantAt(t, osTranscriptPath(home, empty, "acme-empty"), nil, mtime)
	stranger := osDir(t, root, "stranger")
	osPlantAt(t, osTranscriptPath(home, stranger, "acme-stranger"), osConversation("acme-another-session", stranger, "acme-branch", mtime, "acme one", "acme two"), mtime)

	s := r.probe()
	if got := osDirs(s.endedRows()); !slices.Equal(got, []string{good}) {
		t.Fatalf("ended rows = %v, want only the good file's folder %s", got, good)
	}
	if got := r.endedList(); len(got) != 1 {
		t.Fatalf("list holds %d sessions, want 1: %v", len(got), got)
	}
}

// AC10: transcript text confers no authority. A forged cwd cannot point a row
// at another file, and an ended id is refused every action.
func TestOutsideEndedRowGrantsNoAuthority(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	root := t.TempDir()
	home := r.i.Home
	mtime := r.i.Clock().Now.Add(-osEndedHour).Truncate(time.Second)
	target := osDir(t, root, "target")
	targetPath, _ := r.ended(home, target, "acme-target", osEndedHour, "acme target")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	// This file sits in its own folder but its records name the target's folder.
	forgedHome := osDir(t, root, "forger")
	osPlantAt(t, osTranscriptPath(home, forgedHome, "acme-forged"), osConversation("acme-forged", target, "acme-branch", mtime, "acme one", "acme forged"), mtime)

	s := r.probe()
	rows := s.endedRows()
	if len(rows) != 1 || rows[0].Directory != target {
		t.Fatalf("ended rows = %+v, want only the target's own row: a cwd that does not map to its file gives no row", rows)
	}

	id := rows[0].ID
	cos := r.i.SocketHTTP(r.i.Credential("cos"))
	if resp := cos.Do("POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": id, "text": "acme refused"}, scopeHeader()); resp.Status < 400 || resp.Status >= 500 {
		t.Fatalf("POST /api/chief-of-staff/messages answered %d, want a 4xx refusal: %s", resp.Status, resp.Body)
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"DELETE", "/api/sessions/" + id, nil, http.StatusNoContent},
		{"POST", "/api/sessions/" + id + "/resume", nil, http.StatusNotFound},
		{"POST", "/api/sessions/" + id + "/message", map[string]any{"text": "acme refused"}, http.StatusNotFound},
	} {
		if resp := cos.Do(c.method, c.path, c.body); resp.Status != c.want {
			t.Fatalf("%s %s answered %d, want %d: %s", c.method, c.path, resp.Status, c.want, resp.Body)
		}
	}
	otRefusal(t, r.transcriptGet(id, ""), 404, "outside_session_not_found")
	after, err := os.ReadFile(targetPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the target transcript changed or vanished after the refused requests: %v", err)
	}
}
