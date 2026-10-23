package features

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"relaye2e/harness"
)

// The history tests plant interactive Claude transcripts under the console's
// HOME (and the stub host's root), then move the injected clock one probe
// interval. The signal that a scan saw the files is its session.history.scan
// event line for the machine; the files are planted before the wait. A scan
// runs only at boot and on the clock, so none is in flight at a plant.

const hxPath = "/api/session-history"

var hxBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type hxRow struct {
	ID             string  `json:"id"`
	Record         string  `json:"record"`
	Kind           string  `json:"kind"`
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
	ResumeReason   *string `json:"resumeReason"`
}

type hxBody struct {
	Ready    bool `json:"ready"`
	Revision int  `json:"revision"`
	Machines []struct {
		HostID  *string `json:"hostId"`
		Machine string  `json:"machine"`
		Status  string  `json:"status"`
	} `json:"machines"`
	Rows []hxRow `json:"rows"`
}

type hx struct {
	*osRig
	hostID string // "" without a host
}

// hxStart boots the rig and waits for the first scan of every machine.
func hxStart(t *testing.T, host bool) *hx {
	t.Helper()
	return hxReady(t, osStart(t, host), host)
}

func hxReady(t *testing.T, r *osRig, host bool) *hx {
	t.Helper()
	h := &hx{osRig: r}
	h.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Fields: map[string]any{"machine": "console"}}, osBound)
	if host {
		resp := h.i.SocketHTTP(h.i.Credential("cos")).Do("GET", "/api/hosts", nil)
		var hosts []struct {
			ID string `json:"id"`
		}
		resp.JSON(t, &hosts)
		if len(hosts) != 1 {
			t.Fatalf("GET /api/hosts = %s, want one host", resp.Body)
		}
		h.hostID = hosts[0].ID
		h.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Fields: map[string]any{"machine": h.hostID}}, osBound)
	}
	return h
}

// scan moves the clock one probe interval and waits for each machine's scan
// event that follows.
func (h *hx) scan(extra map[string]any) {
	h.t.Helper()
	since := time.Now()
	h.i.ClockAdvance(osProbeStep)
	for _, m := range append([]string{"console"}, h.hostID) {
		if m == "" {
			continue
		}
		f := map[string]any{"machine": m}
		for k, v := range extra {
			f[k] = v
		}
		h.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Since: since, Fields: f}, osBound)
	}
}

func (h *hx) get(query string) (hxBody, harness.Response) {
	h.t.Helper()
	resp := h.i.SocketHTTP(h.i.Credential("cos")).Do("GET", hxPath+query, nil)
	var b hxBody
	if resp.Status == http.StatusOK {
		resp.JSON(h.t, &b)
	}
	return b, resp
}

func (h *hx) rows() hxBody {
	h.t.Helper()
	b, resp := h.get("")
	if resp.Status != http.StatusOK {
		h.t.Fatalf("GET %s answered %d, want 200: %s", hxPath, resp.Status, resp.Body)
	}
	return b
}

func (b hxBody) byConv(conv string) *hxRow {
	for k := range b.Rows {
		if c := b.Rows[k].ConversationID; c != nil && *c == conv {
			return &b.Rows[k]
		}
	}
	return nil
}

func hxConv(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func hxRec(typ, conv, cwd string, at time.Time, extra map[string]any) string {
	m := map[string]any{"type": typ, "sessionId": conv, "entrypoint": "cli"}
	if cwd != "" {
		m["cwd"] = cwd
	}
	if !at.IsZero() {
		m["timestamp"] = at.UTC().Format(time.RFC3339Nano)
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func hxUser(conv, cwd string, at time.Time, text string, extra map[string]any) string {
	m := map[string]any{"message": map[string]any{"role": "user", "content": text}}
	for k, v := range extra {
		m[k] = v
	}
	return hxRec("user", conv, cwd, at, m)
}

func hxEnd(conv, cwd string, at time.Time) string {
	return hxRec("assistant", conv, cwd, at, map[string]any{"message": map[string]any{"role": "assistant", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": "acme answer"}}}})
}

// hxPlant writes a transcript whose file time is mtime and returns its path.
func hxPlant(t *testing.T, home, cwd, conv string, mtime time.Time, recs ...string) string {
	t.Helper()
	path := osTranscriptPath(home, cwd, conv)
	osPlantAt(t, path, recs, mtime)
	return path
}

// hxSimple plants a one-prompt transcript ending at end.
func hxSimple(t *testing.T, home, cwd, conv string, end time.Time, prompt string) {
	t.Helper()
	hxPlant(t, home, cwd, conv, end, hxUser(conv, cwd, end.Add(-time.Minute), prompt, nil), hxEnd(conv, cwd, end))
}

func hxTime(t *testing.T, s *string) time.Time {
	t.Helper()
	if s == nil {
		t.Fatalf("time is null, want a value")
	}
	return osParseTime(t, "time", *s)
}

func hxStr(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// hxSorted checks the documented order: newest lastActiveAt, else createdAt.
func hxSorted(t *testing.T, rows []hxRow) {
	t.Helper()
	prev := time.Time{}
	for k, r := range rows {
		at := osParseTime(t, "createdAt", r.CreatedAt)
		if r.LastActiveAt != nil {
			at = osParseTime(t, "lastActiveAt", *r.LastActiveAt)
		}
		if k > 0 && at.After(prev) {
			t.Fatalf("row %d (%s) is newer than the row before it: %s after %s", k, r.ID, at, prev)
		}
		prev = at
	}
}

// Rows, order, the cap of 500, and the launched/discovered split.
func TestHistoryListsLedgerAndDiscovered(t *testing.T) {
	t.Parallel()
	t.Run("rows", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		sid := startAgentSession(t, h.i, "claude-code", "sonnet")
		work := osDir(t, filepath.Join(h.i.Home, "work"), "acme")
		other := osDir(t, t.TempDir(), "elsewhere")
		hxSimple(t, h.i.Home, work, hxConv(1), hxBase.Add(3*time.Hour), "acme in project")
		hxSimple(t, h.i.Home, other, hxConv(2), hxBase.Add(time.Hour), "acme outside")
		hxSimple(t, h.i.Home, other, hxConv(3), hxBase.Add(2*time.Hour), "acme middle")
		h.scan(nil)

		b := h.rows()
		if !b.Ready || len(b.Machines) != 1 || b.Machines[0].Status != "answered" {
			t.Fatalf("body ready=%v machines=%+v, want ready with one answered machine", b.Ready, b.Machines)
		}
		var launched *hxRow
		var order []string
		for k, r := range b.Rows {
			if r.ID == sid {
				launched = &b.Rows[k]
			} else if r.Record == "discovered" {
				order = append(order, *r.ConversationID)
			}
		}
		if launched == nil || launched.Record != "launched" || launched.Kind != "claude" || launched.State != "live" || launched.ProjectID == nil ||
			launched.HostID != nil || launched.Resume != "none" || hxStr(launched.ResumeReason) != "live" || launched.Transcript != "unknown" ||
			launched.Title != nil || launched.LastActiveAt != nil || launched.ConversationID != nil {
			t.Fatalf("launched row = %+v, want the live claude record with no title, time or conversation", launched)
		}
		if want := []string{hxConv(1), hxConv(3), hxConv(2)}; !reflect.DeepEqual(order, want) {
			t.Fatalf("discovered rows in order %v, want %v (newest lastActiveAt first)", order, want)
		}
		hxSorted(t, b.Rows)
		in, out := b.byConv(hxConv(1)), b.byConv(hxConv(2))
		if in.ProjectID == nil || *in.ProjectID != *launched.ProjectID || in.Resume != "history" || in.Transcript != "present" {
			t.Fatalf("discovered row in the project = %+v, want its projectId and resume history", in)
		}
		if out.ProjectID != nil || out.Resume != "none" || hxStr(out.ResumeReason) != "no_project" {
			t.Fatalf("discovered row outside any project = %+v, want no project and resume none/no_project", out)
		}
	})
	t.Run("cap", func(t *testing.T) {
		t.Parallel()
		h := hxStart(t, false)
		dir := osDir(t, t.TempDir(), "bulk")
		const total = 501
		for n := 0; n < total; n++ {
			hxSimple(t, h.i.Home, dir, hxConv(n), hxBase.Add(time.Duration(n)*time.Minute), "acme bulk")
		}
		for k := 0; k < 10; k++ { // each scan reads at most 64 new files
			h.scan(nil)
		}
		b := h.rows()
		if len(b.Rows) != 500 || b.byConv(hxConv(0)) != nil || b.byConv(hxConv(total-1)) == nil {
			t.Fatalf("%d rows (oldest present %v), want 500 with the oldest of %d dropped", len(b.Rows), b.byConv(hxConv(0)) != nil, total)
		}
		hxSorted(t, b.Rows)
	})
}

// The title rule, in order: custom-title, ai-title, first genuine prompt, null.
func TestHistoryTitleRule(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	dir := osDir(t, t.TempDir(), "titles")
	end := hxBase.Add(time.Hour)
	title := func(kind, key, v string) string { return hxRec(kind, "", "", time.Time{}, map[string]any{key: v}) }
	plant := func(n int, recs ...string) {
		c := hxConv(n)
		hxPlant(t, h.i.Home, dir, c, end, append(recs, hxEnd(c, dir, end))...)
	}
	prompt := func(n int, text string, extra map[string]any) string {
		return hxUser(hxConv(n), dir, hxBase, text, extra)
	}
	long := "A\x07" + strings.Repeat("é", 200)
	plant(1, prompt(1, "acme prompt", nil), title("custom-title", "customTitle", "old custom"), title("custom-title", "customTitle", "new custom"), title("ai-title", "aiTitle", "acme ai"))
	plant(2, prompt(2, "acme prompt", nil), title("ai-title", "aiTitle", "old ai"), title("ai-title", "aiTitle", "new ai"))
	plant(3, prompt(3, "meta text", map[string]any{"isMeta": true}), prompt(3, "side text", map[string]any{"isSidechain": true}),
		prompt(3, "/help", nil), prompt(3, "<command-name>x</command-name>", nil), prompt(3, "\n\n  Fix the login page  \nsecond line", nil))
	plant(4)
	plant(5, title("custom-title", "customTitle", long))
	h.scan(nil)

	b := h.rows()
	for _, c := range []struct {
		n      int
		title  string
		source string
	}{{1, "new custom", "client"}, {2, "new ai", "client"}, {3, "Fix the login page", "prompt"}, {4, "", ""}} {
		r := b.byConv(hxConv(c.n))
		if r == nil {
			t.Fatalf("no row for transcript %d", c.n)
		}
		if c.title == "" {
			if r.Title != nil || r.TitleSource != nil {
				t.Fatalf("transcript %d title = %s source %s, want both null", c.n, hxStr(r.Title), hxStr(r.TitleSource))
			}
		} else if hxStr(r.Title) != c.title || hxStr(r.TitleSource) != c.source {
			t.Fatalf("transcript %d title = %q source %s, want %q source %s", c.n, hxStr(r.Title), hxStr(r.TitleSource), c.title, c.source)
		}
	}
	r := b.byConv(hxConv(5))
	if r == nil || r.Title == nil || utf8.RuneCountInString(*r.Title) != 120 || strings.IndexFunc(*r.Title, unicode.IsControl) >= 0 {
		t.Fatalf("long title row = %+v, want 120 runes and no control characters", r)
	}
}

// lastActiveAt is the newest record timestamp even when the file's time is
// later, and the file's time when no record has one.
func TestHistoryLastActiveFromTimestamp(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	dir := osDir(t, t.TempDir(), "times")
	newest := hxBase.Add(2 * time.Hour)
	later := hxBase.Add(30 * 24 * time.Hour)
	c1, c2 := hxConv(1), hxConv(2)
	hxPlant(t, h.i.Home, dir, c1, later, hxUser(c1, dir, hxBase, "acme one", nil), hxEnd(c1, dir, newest))
	hxPlant(t, h.i.Home, dir, c2, later, hxUser(c2, dir, time.Time{}, "acme two", nil), hxEnd(c2, dir, time.Time{}))
	h.scan(nil)

	b := h.rows()
	r1, r2 := b.byConv(c1), b.byConv(c2)
	if r1 == nil || r2 == nil {
		t.Fatalf("rows for the planted transcripts: %v %v", r1, r2)
	}
	if got := hxTime(t, r1.LastActiveAt); !got.Equal(newest) {
		t.Fatalf("lastActiveAt = %s, want the newest record timestamp %s, not the file time %s", got, newest, later)
	}
	if got := osParseTime(t, "createdAt", r1.CreatedAt); !got.Equal(hxBase) {
		t.Fatalf("createdAt = %s, want the first record timestamp %s", got, hxBase)
	}
	if got := hxTime(t, r2.LastActiveAt); !got.Equal(later) {
		t.Fatalf("lastActiveAt of a transcript with no timestamps = %s, want its file time %s", got, later)
	}
}

// hxDiscoveredID follows the issue contract: NUL separates the three parts.
func hxDiscoveredID(conv string) string {
	sum := sha256.Sum256([]byte("console\x00claude\x00" + conv))
	return "d-" + hex.EncodeToString(sum[:16])
}

// A discovered record is written for an unlinked interactive transcript; not
// for a headless one or one a managed session owns.
func TestHistoryDiscoveredRecord(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	sid := startAgentSession(t, h.i, "claude-code", "sonnet")
	// Signal: the reply, so the session has started its conversation.
	h.i.MustCLI("session", "message", "--id", sid, "--text", "hi", "--json")
	managed := ""
	for _, argv := range g1lAgentStarts(h.i) {
		for n, a := range argv {
			if a == "--session-id" && n+1 < len(argv) {
				managed = argv[n+1]
			}
		}
	}
	if managed == "" {
		t.Fatalf("no start of the fake agent for session %s carries --session-id", sid)
	}
	dir := osDir(t, t.TempDir(), "found")
	end := hxBase.Add(time.Hour)
	free, headless := hxConv(1), hxConv(2)
	hxSimple(t, h.i.Home, dir, free, end, "acme free")
	hxSimple(t, h.i.Home, dir, managed, end, "acme managed")
	hxPlant(t, h.i.Home, dir, headless, end, hxUser(headless, dir, end, "acme headless", map[string]any{"entrypoint": "sdk-cli"}), hxEnd(headless, dir, end))
	h.scan(nil)

	b := h.rows()
	r := b.byConv(free)
	if r == nil || r.Record != "discovered" || r.State != "discovered" || r.Kind != "terminal" || r.ProjectID != nil || r.HostID != nil ||
		r.Directory != dir || r.ID != hxDiscoveredID(free) || !hxTime(t, &r.CreatedAt).Equal(end.Add(-time.Minute)) {
		t.Fatalf("discovered row = %+v, want state discovered, kind terminal, no project, id %s", r, hxDiscoveredID(free))
	}
	if x := b.byConv(headless); x != nil {
		t.Fatalf("a headless sdk-cli transcript produced %+v, want no row", x)
	}
	if x := b.byConv(managed); x != nil {
		t.Fatalf("a managed session's transcript produced %+v, want no discovered row", x)
	}
	raw, err := os.ReadFile(filepath.Join(h.i.ConfigDir, g1rLedger))
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	var recs []map[string]any
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatalf("ledger does not decode: %v", err)
	}
	var found bool
	for _, rec := range recs {
		switch rec["conversation_id"] {
		case free:
			found = true
			if rec["state"] != "discovered" || rec["session_id"] != r.ID || rec["project_id"] != nil && rec["project_id"] != "" {
				t.Fatalf("ledger record = %v, want a discovered record %s with no project id", rec, r.ID)
			}
		case headless, managed:
			t.Fatalf("ledger holds a discovered record for %v", rec)
		}
	}
	if !found {
		t.Fatalf("ledger holds no record for conversation %s", free)
	}
}

// The same rows after a restart, with titles once the first scan ends.
func TestHistoryDiscoveredAfterRestart(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	dir := osDir(t, t.TempDir(), "restart")
	hxSimple(t, h.i.Home, dir, hxConv(1), hxBase.Add(time.Hour), "acme first")
	hxSimple(t, h.i.Home, dir, hxConv(2), hxBase.Add(2*time.Hour), "acme second")
	h.scan(nil)
	before := h.rows()
	if len(before.Rows) != 2 {
		t.Fatalf("%d rows before the restart, want 2", len(before.Rows))
	}

	// The transcripts leave the store while relay is down, so the rows after
	// the first scan can only come from the ledger.
	root := filepath.Join(h.i.Home, ".claude", "projects")
	stash := filepath.Join(t.TempDir(), "projects")
	if err := os.Rename(root, stash); err != nil {
		t.Fatalf("moving the store aside: %v", err)
	}
	since := time.Now()
	h.i.Restart()
	h.i.WaitSessionHost(osBound)
	h.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Since: since, Fields: map[string]any{"machine": "console"}}, osBound)
	early := h.rows()
	if !early.Ready || len(early.Rows) != 2 {
		t.Fatalf("after the first scan ready=%v rows=%+v, want the 2 discovered records", early.Ready, early.Rows)
	}
	for _, r := range early.Rows {
		if b := before.byConv(*r.ConversationID); b == nil || b.ID != r.ID || r.State != "discovered" || r.Transcript != "unknown" || r.Title != nil || r.LastActiveAt != nil {
			t.Fatalf("row %+v, want the discovered record %v with transcript unknown and no title or time (no answered listing)", r, b)
		}
	}

	if err := os.Rename(stash, root); err != nil {
		t.Fatalf("moving the store back: %v", err)
	}
	h.scan(nil)
	after := h.rows()
	if !reflect.DeepEqual(before.Rows, after.Rows) {
		t.Fatalf("after the store returned rows=%+v, want the rows %+v with their titles", after.Rows, before.Rows)
	}
}

// A host is read only in its projects' folders; a symbolic link on a file or
// folder drops it; a scan writes nothing.
func TestHistoryScanScope(t *testing.T) {
	t.Parallel()
	h := hxStart(t, true)
	proj := filepath.Join(h.remote, "boxproj")
	in := osDir(t, proj, "in")
	linked := osDir(t, proj, "linked")
	elsewhere := osDir(t, h.remote, "elsewhere")
	stash := osDir(t, h.remote, "stash")
	end := hxBase.Add(time.Hour)
	in1, out, linkFile, linkDir, con := hxConv(1), hxConv(2), hxConv(3), hxConv(4), hxConv(5)
	hxSimple(t, h.remote, in, in1, end, "acme in project")
	hxSimple(t, h.remote, elsewhere, out, end, "acme outside the project")
	hxSimple(t, h.i.Home, elsewhere, con, end, "acme console")
	real := hxPlant(t, stash, in, linkFile, end, hxUser(linkFile, in, end, "acme link", nil), hxEnd(linkFile, in, end))
	if err := os.Symlink(real, osTranscriptPath(h.remote, in, linkFile)); err != nil {
		t.Fatalf("linking the transcript: %v", err)
	}
	hxPlant(t, stash, linked, linkDir, end, hxUser(linkDir, linked, end, "acme folder link", nil), hxEnd(linkDir, linked, end))
	folder := filepath.Dir(osTranscriptPath(h.remote, linked, linkDir))
	if err := os.MkdirAll(filepath.Dir(folder), 0o755); err != nil {
		t.Fatalf("creating the projects folder: %v", err)
	}
	if err := os.Symlink(filepath.Dir(osTranscriptPath(stash, linked, linkDir)), folder); err != nil {
		t.Fatalf("linking the folder: %v", err)
	}
	snapshot := func() string {
		var out []string
		for _, root := range []string{h.remote, h.i.Home} {
			_ = filepath.Walk(filepath.Join(root, ".claude", "projects"), func(p string, fi os.FileInfo, err error) error {
				if err == nil {
					out = append(out, fmt.Sprint(p, fi.Size(), fi.Mode(), fi.ModTime().UnixNano()))
				}
				return nil
			})
		}
		return strings.Join(out, "\n")
	}
	before := snapshot()
	h.scan(nil)

	b := h.rows()
	r := b.byConv(in1)
	if r == nil || r.HostID == nil || *r.HostID != h.hostID || r.ProjectID == nil || hxStr(r.ResumeReason) != "host_unsupported" || r.Transcript != "present" {
		t.Fatalf("host project row = %+v, want hostId %s, a project and resume host_unsupported", r, h.hostID)
	}
	for _, c := range []string{out, linkFile, linkDir} {
		if x := b.byConv(c); x != nil {
			t.Fatalf("conversation %s (outside the project or behind a link) produced %+v, want no row", c, x)
		}
	}
	if c := b.byConv(con); c == nil || c.HostID != nil {
		t.Fatalf("console row = %+v, want a console row for a folder outside any project", c)
	}
	if after := snapshot(); after != before {
		t.Fatalf("the scan changed the transcript folders:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A missing store root, or a link on it, is a scan that did not answer.
func TestHistoryRootMissingNotAnswered(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	root := filepath.Join(h.i.Home, ".claude", "projects")
	if _, err := os.Lstat(root); err == nil {
		t.Fatalf("%s exists at boot; the test needs a missing store root", root)
	}
	b := h.rows()
	if !b.Ready || len(b.Machines) != 1 || b.Machines[0].Status != "unreachable" {
		t.Fatalf("body ready=%v machines=%+v, want ready with the console unreachable", b.Ready, b.Machines)
	}
	if got := h.i.Events(harness.EventQuery{Key: "session.history.scan", Fields: map[string]any{"machine": "console", "answered": false}}); len(got) == 0 {
		t.Fatalf("no session.history.scan line with answered false for a missing root")
	}

	real := filepath.Join(t.TempDir(), "projects")
	dir := osDir(t, t.TempDir(), "linked-root")
	hxSimple(t, filepath.Dir(real), dir, hxConv(1), hxBase, "acme behind a link")
	if err := os.Rename(filepath.Join(filepath.Dir(real), ".claude", "projects"), real); err != nil {
		t.Fatalf("moving the planted store: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(root), err)
	}
	if err := os.Symlink(real, root); err != nil {
		t.Fatalf("linking the root: %v", err)
	}
	h.scan(map[string]any{"answered": false})
	if b := h.rows(); b.Machines[0].Status != "unreachable" || len(b.Rows) != 0 {
		t.Fatalf("machines=%+v rows=%+v, want an unreachable console and no rows behind a linked root", b.Machines, b.Rows)
	}
}

// relay session history --json prints the route's body; --project filters.
func TestHistoryCLI(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	startAgentSession(t, h.i, "claude-code", "sonnet")
	work := osDir(t, filepath.Join(h.i.Home, "work"), "acme")
	other := osDir(t, t.TempDir(), "elsewhere")
	hxSimple(t, h.i.Home, work, hxConv(1), hxBase, "acme in project")
	hxSimple(t, h.i.Home, other, hxConv(2), hxBase, "acme outside")
	h.scan(nil)

	var cli, httpBody map[string]any
	res := h.i.CLI("session", "history", "--json")
	if res.Code != 0 {
		t.Fatalf("session history exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	res.JSON(t, &cli)
	_, resp := h.get("")
	resp.JSON(t, &httpBody)
	if !reflect.DeepEqual(cli, httpBody) {
		t.Fatalf("--json printed %v, want the route's body %v", cli, httpBody)
	}
	if res := h.i.CLI("session", "history"); res.Code != 0 {
		t.Fatalf("session history (text) exited %d\nstderr: %s", res.Code, res.Stderr)
	}

	pid := ""
	for _, r := range h.rows().Rows {
		if r.ProjectID != nil {
			pid = *r.ProjectID
		}
	}
	var scoped hxBody
	res = h.i.CLI("session", "history", "--project", pid, "--json")
	if res.Code != 0 {
		t.Fatalf("session history --project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	res.JSON(t, &scoped)
	if len(scoped.Rows) != 2 || scoped.byConv(hxConv(2)) != nil || scoped.byConv(hxConv(1)) == nil {
		t.Fatalf("--project %s rows = %+v, want the launched row and the discovered row inside it", pid, scoped.Rows)
	}
	for _, r := range scoped.Rows {
		if r.ProjectID == nil || *r.ProjectID != pid {
			t.Fatalf("--project row %+v has another project", r)
		}
	}
	if b, resp := h.get("?projectId=unknown-acme"); resp.Status != http.StatusOK || len(b.Rows) != 0 {
		t.Fatalf("unknown projectId answered %d %s, want 200 with no rows", resp.Status, resp.Body)
	}
}

// The Chief of Staff scope, a read credential and the TCP listener cannot read it.
func TestHistoryScopeRefused(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	dir := osDir(t, t.TempDir(), "private")
	hxSimple(t, h.i.Home, dir, hxConv(1), hxBase, "acme private prompt")
	h.scan(nil)
	if b := h.rows(); b.byConv(hxConv(1)) == nil {
		t.Fatalf("the control request lists no planted row: %+v", b.Rows)
	}

	scoped := cosDo(t, h.i, "GET", hxPath, nil)
	if scoped.Status != http.StatusForbidden || strings.Contains(string(scoped.Body), "acme private prompt") {
		t.Fatalf("chief-of-staff scope answered %d %s, want 403 with no history", scoped.Status, scoped.Body)
	}
	if resp := h.i.SocketHTTP(h.i.Credential("reader")).Do("GET", hxPath, nil); resp.Status != http.StatusForbidden {
		t.Fatalf("read-class credential answered %d, want 403: %s", resp.Status, resp.Body)
	}
	if resp := h.i.HTTP(h.i.Credential("cos")).Do("GET", hxPath, nil); resp.Status != http.StatusNotFound || strings.Contains(string(resp.Body), "acme private prompt") {
		t.Fatalf("TCP listener answered %d %s, want 404: the route is absent there", resp.Status, resp.Body)
	}
}

// Past 4096 transcripts the machine still answers, and a row whose file fell
// off the listing is unknown, not absent.
func TestHistoryListingCap(t *testing.T) {
	t.Parallel()
	h := hxStart(t, false)
	dir := osDir(t, t.TempDir(), "old")
	hxSimple(t, h.i.Home, dir, hxConv(0), hxBase, "acme oldest")
	h.scan(nil)
	if r := h.rows().byConv(hxConv(0)); r == nil || r.Transcript != "present" {
		t.Fatalf("oldest row before the flood = %+v, want transcript present", r)
	}
	bulk := filepath.Dir(osTranscriptPath(h.i.Home, osDir(t, t.TempDir(), "bulk"), "x"))
	if err := os.MkdirAll(bulk, 0o755); err != nil {
		t.Fatalf("creating %s: %v", bulk, err)
	}
	for n := 1; n <= 4096; n++ {
		if err := os.WriteFile(filepath.Join(bulk, hxConv(n)+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("writing transcript %d: %v", n, err)
		}
	}
	h.scan(map[string]any{"answered": true, "complete": false})
	b := h.rows()
	if r := b.byConv(hxConv(0)); r == nil || r.Transcript != "unknown" || b.Machines[0].Status != "answered" {
		t.Fatalf("oldest row past the cap = %+v machines %+v, want transcript unknown on an answered machine", r, b.Machines)
	}
}

// A discovered record whose conversation relay-sessions then holds is gone at
// the next scan.
func TestHistoryAdoptedDropsFound(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	h := hxReady(t, a.osRig, false)
	f := a.fake("app")
	now := time.Now().UTC()
	f.transcript(a.i.Home, hxUser(f.sid, f.dir, f.start, adPrompt, nil), hxEnd(f.sid, f.dir, now))
	f.register(a.i.Home, "idle", now)
	since := time.Now()
	s := a.probe() // its clock move starts a scan too
	a.i.WaitEvent(harness.EventQuery{Key: "session.history.scan", Since: since, Fields: map[string]any{"machine": "console"}}, osBound)
	if r := h.rows().byConv(f.sid); r == nil || r.Record != "discovered" {
		t.Fatalf("row before the adopt = %+v, want a discovered record", r)
	}
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	adCreated(t, a.adopt(map[string]any{"outsideId": live.ID, "interrupt": false, "model": "sonnet"}))
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()

	h.scan(map[string]any{"superseded": 1})
	if r := h.rows().byConv(f.sid); r != nil {
		t.Fatalf("row after the adopt = %+v, want the discovered record gone", r)
	}
}

// Removing a host project takes its folder out of the host's scope, so its
// discovered rows are unknown, not absent.
func TestHistoryHostScopeUnknown(t *testing.T) {
	t.Parallel()
	h := hxStart(t, true)
	other := filepath.Join(h.remote, "boxtwo")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("creating %s: %v", other, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme Two", "path": other, "host_id": h.hostID, "allowed_templates": []string{"claude-code"}})
	if res := h.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json"); res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	in := osDir(t, filepath.Join(h.remote, "boxproj"), "in")
	hxSimple(t, h.remote, in, hxConv(1), hxBase, "acme on the host")
	h.scan(nil)
	r := h.rows().byConv(hxConv(1))
	if r == nil || r.ProjectID == nil || r.Transcript != "present" {
		t.Fatalf("host row before the removal = %+v, want a project and transcript present", r)
	}
	if res := h.i.CLI("project", "remove", "--id", *r.ProjectID, "--json"); res.Code != 0 {
		t.Fatalf("project remove exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	h.scan(nil)
	if r := h.rows().byConv(hxConv(1)); r == nil || r.Transcript != "unknown" {
		t.Fatalf("host row after the removal = %+v, want transcript unknown", r)
	}
}
