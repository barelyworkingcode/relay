package features

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"relaye2e/harness"
)

// The transcript route is read-only and socket-only (class execute), so these
// tests call it with the unscoped "cos" credential, which holds execute.

type otEntry struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type otPage struct {
	Entries []otEntry `json:"entries"`
	Cursor  *string   `json:"cursor"`
	HasMore bool      `json:"hasMore"`
}

func otPath(id, query string) string {
	p := "/api/outside-sessions/" + id + "/transcript"
	if query != "" {
		p += "?" + query
	}
	return p
}

func (r *osRig) transcriptGet(id, query string) harness.Response {
	r.t.Helper()
	return r.i.SocketHTTP(r.i.Credential("cos")).Do("GET", otPath(id, query), nil)
}

func (r *osRig) transcriptPage(id, query string) otPage {
	r.t.Helper()
	resp := r.transcriptGet(id, query)
	if resp.Status != http.StatusOK {
		r.t.Fatalf("GET %s answered %d, want 200: %s", otPath(id, query), resp.Status, resp.Body)
	}
	var page otPage
	resp.JSON(r.t, &page)
	if page.Entries == nil {
		r.t.Fatalf("entries is null in %s, want an array", resp.Body)
	}
	return page
}

func otRefusal(t *testing.T, resp harness.Response, status int, code string) {
	t.Helper()
	var e struct{ Error, Message string }
	resp.JSON(t, &e)
	if resp.Status != status || e.Error != code {
		t.Fatalf("answered %d %q, want %d %q: %s", resp.Status, e.Error, status, code, resp.Body)
	}
}

// otFile is the transcript path Claude Code uses for p.
func otFile(p *osProc) string {
	folder := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(p.dir, "-")
	return filepath.Join(p.home, ".claude", "projects", folder, p.sid+".jsonl")
}

// otConversation is n records alternating prompt and answer, each text unique.
func otConversation(n int, pad string) []string {
	lines := make([]string, n)
	for k := range lines {
		text := fmt.Sprintf("acme %04d %s", k, pad)
		if k%2 == 0 {
			lines[k] = osPrompt(text)
		} else {
			lines[k] = osAnswer(text)
		}
	}
	return lines
}

func otTexts(es []otEntry) []string {
	out := make([]string, len(es))
	for k, e := range es {
		out[k] = e.Text
	}
	return out
}

// otRaw renders one record with the fields the mapping table names.
func otRaw(kind, ts string, content any, extra map[string]any) string {
	rec := map[string]any{"type": kind, "message": map[string]any{"content": content}}
	if ts != "" {
		rec["timestamp"] = ts
	}
	for k, v := range extra {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

// otSession starts a console claude whose transcript is lines and returns it with its row.
func otSession(t *testing.T, r *osRig, name string, lines ...string) (*osProc, osRow) {
	t.Helper()
	p := r.spawn(r.i.Home, t.TempDir(), name)
	p.transcript(t, lines...)
	row := r.probe().at(p.dir)
	if row == nil {
		t.Fatalf("no row for %s", p.dir)
	}
	return p, *row
}

// AC1 and AC4: the newest page, in file order, for a claude outside tmux.
func TestOutsideTranscriptNewestPage(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	lines := otConversation(120, "")
	_, row := otSession(t, r, "newest", lines...)
	if row.Viewable || row.ViewReason != "not_in_tmux" {
		t.Fatalf("row = %+v, want viewable false with viewReason not_in_tmux", row)
	}
	page := r.transcriptPage(row.ID, "")
	var want []string
	for k := 70; k < 120; k++ {
		want = append(want, fmt.Sprintf("acme %04d ", k))
	}
	if got := otTexts(page.Entries); !slices.Equal(got, want) {
		t.Fatalf("entries = %q, want the newest 50 records, oldest first: %q", got, want)
	}
	seen := map[string]bool{}
	for _, e := range page.Entries {
		if e.ID == "" || seen[e.ID] {
			t.Fatalf("entry id %q is empty or repeated in %+v", e.ID, page.Entries)
		}
		seen[e.ID] = true
	}
	if page.Cursor == nil || !page.HasMore {
		t.Fatalf("page = cursor %v hasMore %v, want a cursor and hasMore true", page.Cursor, page.HasMore)
	}
}

// AC1: every row of the record mapping, skips included.
func TestOutsideTranscriptEntryMapping(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	const ts = "2026-10-13T10:00:00.000Z"
	blocks := func(bs ...map[string]any) []any {
		out := make([]any, len(bs))
		for k, b := range bs {
			out[k] = b
		}
		return out
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	long := strings.Repeat("界", 9000)
	lines := []string{
		otRaw("user", ts, "acme prompt", nil),
		otRaw("user", ts, blocks(text("acme block prompt")), nil),
		otRaw("user", ts, blocks(map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "acme output"}), nil),
		otRaw("user", ts, blocks(map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": blocks(text("acme one"), map[string]any{"type": "image"}, text("acme two"))}), nil),
		otRaw("user", ts, blocks(map[string]any{"type": "tool_result", "tool_use_id": "t3", "content": "acme boom", "is_error": true}), nil),
		otRaw("assistant", ts, blocks(
			map[string]any{"type": "thinking", "thinking": "acme private"},
			map[string]any{"type": "redacted_thinking", "data": "acme private"},
			text("acme answer"),
			map[string]any{"type": "tool_use", "id": "t4", "name": "Bash", "input": json.RawMessage(`{ "command": "ls", "n": 1 }`)},
		), nil),
		otRaw("assistant", "", blocks(text("acme no stamp")), nil),
		// Skipped records.
		otRaw("user", ts, "acme side", map[string]any{"isSidechain": true}),
		otRaw("user", ts, "acme meta", map[string]any{"isMeta": true}),
		otRaw("system", ts, "acme system", nil),
		otRaw("summary", ts, "acme summary", nil),
		otRaw("file-history-snapshot", ts, "acme snapshot", nil),
		"{acme not json",
		otRaw("user", ts, "   ", nil),
		otRaw("user", ts, blocks(text(" \n")), nil),
		otRaw("assistant", ts, "acme plain assistant string", nil),
		otRaw("user", ts, long, nil),
	}
	_, row := otSession(t, r, "mapping", lines...)
	page := r.transcriptPage(row.ID, "")
	capped := string([]rune(long)[:8191]) + "…"
	want := []otEntry{
		{Role: "user", Text: "acme prompt", At: ts},
		{Role: "user", Text: "acme block prompt", At: ts},
		{Role: "tool", Text: "acme output", At: ts},
		{Role: "tool", Text: "acme one\n[image]\nacme two", At: ts},
		{Role: "tool", Text: "Error: acme boom", At: ts},
		{Role: "assistant", Text: "acme answer", At: ts},
		{Role: "tool", Text: `Bash {"command":"ls","n":1}`, At: ts},
		{Role: "assistant", Text: "acme no stamp", At: ""},
		{Role: "user", Text: capped, At: ts},
	}
	if len(page.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(page.Entries), len(want), page.Entries)
	}
	ids := map[string]bool{}
	for k, e := range page.Entries {
		if e.ID == "" || ids[e.ID] {
			t.Fatalf("entry %d id %q is empty or repeated", k, e.ID)
		}
		ids[e.ID] = true
		if e.Role != want[k].Role || e.Text != want[k].Text || e.At != want[k].At {
			t.Fatalf("entry %d = {%s %.80q %q}, want {%s %.80q %q}", k, e.Role, e.Text, e.At, want[k].Role, want[k].Text, want[k].At)
		}
	}
	last := page.Entries[len(page.Entries)-1].Text
	if n := utf8.RuneCountInString(last); n != 8192 || !strings.HasSuffix(last, "…") {
		t.Fatalf("capped entry has %d characters, suffix ok %v, want 8192 ending with …", n, strings.HasSuffix(last, "…"))
	}
	if page.Cursor != nil || page.HasMore {
		t.Fatalf("a whole file in one page answered cursor %v hasMore %v, want null and false", page.Cursor, page.HasMore)
	}
}

// AC2: the cursor walks to the start without a repeat or a gap; bad paging input is refused.
func TestOutsideTranscriptCursorPagesOlder(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	_, row := otSession(t, r, "paging", otConversation(120, "")...)
	var got []string
	query := "limit=17"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatalf("still paging after %d pages", pages)
		}
		page := r.transcriptPage(row.ID, query)
		got = append(otTexts(page.Entries), got...)
		if page.Cursor == nil {
			if page.HasMore {
				t.Fatalf("last page has hasMore true with a null cursor")
			}
			break
		}
		if !page.HasMore {
			t.Fatalf("page has cursor %s and hasMore false", *page.Cursor)
		}
		query = "limit=17&before=" + *page.Cursor
	}
	var want []string
	for k := 0; k < 120; k++ {
		want = append(want, fmt.Sprintf("acme %04d ", k))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pages joined to %d entries %q, want all 120 once, in order", len(got), got)
	}
	for _, q := range []string{"before=x", "before=0", "before=-5", "before=007", "before=99999999999999"} {
		otRefusal(t, r.transcriptGet(row.ID, q), 400, "invalid_cursor")
	}
	for _, q := range []string{"limit=0", "limit=201", "limit=x", "limit=-1", "limit="} {
		otRefusal(t, r.transcriptGet(row.ID, q), 400, "invalid_limit")
	}
	if page := r.transcriptPage(row.ID, "limit=200"); len(page.Entries) != 120 {
		t.Fatalf("limit=200 answered %d entries, want all 120", len(page.Entries))
	}
}

// AC3: a session on an SSH host reads over the host shell.
func TestOutsideTranscriptOnHost(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	h := r.spawn(r.remote, filepath.Join(r.remote, "boxproj"), "host")
	h.transcript(t, osPrompt("acme host hello"), osAnswer("acme host hi"))
	row := r.probe().at(h.dir)
	if row == nil || row.HostID == nil {
		t.Fatalf("host row = %+v, want a row with a hostId", row)
	}
	page := r.transcriptPage(row.ID, "")
	if got, want := otTexts(page.Entries), []string{"acme host hello", "acme host hi"}; !slices.Equal(got, want) {
		t.Fatalf("host entries = %q, want %q", got, want)
	}
}

// AC5: a request reads no more than the byte cap.
func TestOutsideTranscriptReadsAtMostTheByteCap(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	const cap = 262144
	lines := otConversation(400, strings.Repeat("x", 2000))
	size := 0
	for _, l := range lines {
		size += len(l) + 1
	}
	_, row := otSession(t, r, "cap", lines...)
	page := r.transcriptPage(row.ID, "limit=200")
	if len(page.Entries) == 0 || len(page.Entries) >= 200 {
		t.Fatalf("got %d entries, want some but fewer than 200: a %d-byte window holds about %d", len(page.Entries), cap, cap/2100)
	}
	if last := page.Entries[len(page.Entries)-1].Text; !strings.HasPrefix(last, "acme 0399 ") {
		t.Fatalf("newest entry = %.20q, want the last record", last)
	}
	c, err := strconv.Atoi(deref(page.Cursor))
	if page.Cursor == nil || err != nil || c < size-cap || !page.HasMore {
		t.Fatalf("cursor %v hasMore %v, want a cursor of at least %d and hasMore true", page.Cursor, page.HasMore, size-cap)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// AC5: a transcript past the 8 MiB run bound still serves its newest page.
func TestOutsideTranscriptLargeFileServesTail(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p, row := otSession(t, r, "large", osPrompt("acme small"), osAnswer("acme small"))
	lines := otConversation(4600, strings.Repeat("x", 2000))
	lines = append(lines, osAnswer("acme newest"))
	data := strings.Join(lines, "\n") + "\n"
	if len(data) < 9<<20 {
		t.Fatalf("test file is %d bytes, want at least 9 MiB", len(data))
	}
	// Replaced after the probe: the route reads the file at request time.
	osWrite(t, otFile(p), []byte(data))
	page := r.transcriptPage(row.ID, "")
	if len(page.Entries) == 0 || page.Entries[len(page.Entries)-1].Text != "acme newest" {
		t.Fatalf("got %d entries, want the newest record (acme newest) last", len(page.Entries))
	}
	c, err := strconv.Atoi(deref(page.Cursor))
	if err != nil || c < len(data)-262144 || !page.HasMore {
		t.Fatalf("cursor %v hasMore %v, want a cursor of at least %d", page.Cursor, page.HasMore, len(data)-262144)
	}
}

// AC6: an unknown id, an ended session and its ended row answer 404.
func TestOutsideTranscriptUnknownOrEndedIs404(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	otRefusal(t, r.transcriptGet("outside:unknown", ""), 404, "outside_session_not_found")
	// The transcript carries the identity fields an ended row is read from.
	p := r.spawn(r.i.Home, t.TempDir(), "ending")
	osPlantAt(t, otFile(p), osConversation(p.sid, p.dir, "acme-branch", r.i.Clock().Now, "acme one", "acme done"), r.i.Clock().Now)
	row := r.probe().liveAt(p.dir)
	if row == nil {
		t.Fatalf("no row for %s", p.dir)
	}
	r.transcriptPage(row.ID, "") // the same id answers while the session lives
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	<-p.exit
	s := r.probe()
	if s.liveAt(p.dir) != nil {
		t.Fatalf("live row for %s survived the probe after its process ended", p.dir)
	}
	otRefusal(t, r.transcriptGet(row.ID, ""), 404, "outside_session_not_found")
	ended := s.endedAt(p.dir)
	if ended == nil {
		t.Fatalf("no ended row for %s; snapshot %+v", p.dir, s)
	}
	otRefusal(t, r.transcriptGet(ended.ID, ""), 404, "outside_session_not_found")
}

// AC7: the Chief of Staff scope and a read-class credential cannot reach the route.
func TestOutsideTranscriptRefusedInChiefOfStaffScope(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	_, row := otSession(t, r, "scoped", osPrompt("acme one"), osAnswer("acme private answer"))
	r.transcriptPage(row.ID, "") // the same request passes without the scope
	if resp := cosDo(t, r.i, "GET", otPath(row.ID, ""), nil); resp.Status != http.StatusForbidden || strings.Contains(string(resp.Body), "acme private answer") {
		t.Fatalf("scoped request answered %d %s, want 403 with no transcript text", resp.Status, resp.Body)
	}
	found := false
	for _, a := range r.i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if reason, _ := a["error"].(string); strings.Contains(reason, "outside chief-of-staff scope") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no denied control_decision row with reason %q", "outside chief-of-staff scope")
	}
	if resp := r.i.SocketHTTP(r.i.Credential("reader")).Do("GET", otPath(row.ID, ""), nil); resp.Status != http.StatusForbidden {
		t.Fatalf("read-class credential answered %d, want 403: %s", resp.Status, resp.Body)
	}
	d := r.detail(row.ID)
	var detail struct {
		LastTurn struct{ Text string } `json:"lastTurn"`
		Entries  json.RawMessage       `json:"entries"`
	}
	d.JSON(t, &detail)
	if d.Status != http.StatusOK || detail.LastTurn.Text != "acme private answer" || detail.Entries != nil {
		t.Fatalf("scoped detail answered %d %s, want 200 with lastTurn only", d.Status, d.Body)
	}
}

// AC8: the route takes no body and a read changes nothing on disk.
func TestOutsideTranscriptTakesNoBodyWritesNothing(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p, row := otSession(t, r, "readonly", otConversation(30, "")...)
	cli := r.i.SocketHTTP(r.i.Credential("cos"))
	otRefusal(t, cli.Do("GET", otPath(row.ID, ""), map[string]any{"limit": 1}), 400, "invalid_request")
	for _, q := range []string{"after=1", "limit=5&limit=6", "before=5&before=6"} {
		otRefusal(t, r.transcriptGet(row.ID, q), 400, "invalid_request")
	}
	file := otFile(p)
	snapshot := func() string {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		names, err := os.ReadDir(filepath.Dir(file))
		if err != nil {
			t.Fatal(err)
		}
		var list []string
		for _, n := range names {
			list = append(list, n.Name())
		}
		return fmt.Sprintf("%x %d %d %q", sha256.Sum256(data), info.Size(), info.ModTime().UnixNano(), list)
	}
	before := snapshot()
	for k := 0; k < 10; k++ {
		r.transcriptPage(row.ID, "")
	}
	if after := snapshot(); after != before {
		t.Fatalf("ten reads changed the transcript or its folder:\nbefore %s\nafter  %s", before, after)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if resp := cli.Do(m, otPath(row.ID, ""), nil); resp.Status != http.StatusNotFound || !strings.Contains(string(resp.Body), "no service registered for this path") {
			t.Errorf("%s answered %d, want 404 no service registered for this path: %s", m, resp.Status, resp.Body)
		}
	}
}

// Regression: a skipped record on the first line makes a whole short file report older pages.
func TestOutsideTranscriptSkippedFirstLineHasNoOlderPage(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	for _, first := range []string{
		otRaw("summary", "", "acme summary", nil),
		`{"type":"queue-operation","operation":"enqueue","content":"acme queued"}`,
	} {
		_, row := otSession(t, r, fmt.Sprintf("skipfirst%d", len(first)), first, osPrompt("acme one"), osAnswer("acme two"))
		page := r.transcriptPage(row.ID, "")
		if got, want := otTexts(page.Entries), []string{"acme one", "acme two"}; !slices.Equal(got, want) {
			t.Fatalf("entries = %q, want %q", got, want)
		}
		if page.Cursor != nil || page.HasMore {
			t.Fatalf("answered cursor %v hasMore %v after a skipped first line, want null and false", page.Cursor, page.HasMore)
		}
	}
}

// Regression: an unreadable, symlinked or missing transcript answers a wrong status or leaks the target.
func TestOutsideTranscriptUnsafeOrMissingFile(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	unavailable := func(row osRow, query string) {
		t.Helper()
		otRefusal(t, r.transcriptGet(row.ID, query), 502, "outside_status_unavailable")
	}
	t.Run("unreadable", func(t *testing.T) {
		p, row := otSession(t, r, "unreadable", otConversation(10, "")...)
		if err := os.Chmod(otFile(p), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(otFile(p), 0o600) })
		if _, err := os.ReadFile(otFile(p)); err == nil {
			t.Fatal("transcript stays readable at mode 0: this test needs an account that honours file modes, not root")
		}
		unavailable(row, "")
		unavailable(row, "before=100")
	})
	t.Run("symlinked file", func(t *testing.T) {
		p, row := otSession(t, r, "linkfile", otConversation(10, "")...)
		target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
		osWrite(t, target, []byte(osPrompt("acme secret")+"\n"))
		if err := os.Remove(otFile(p)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, otFile(p)); err != nil {
			t.Fatal(err)
		}
		unavailable(row, "")
	})
	t.Run("symlinked folder", func(t *testing.T) {
		p, row := otSession(t, r, "linkdir", otConversation(10, "")...)
		moved := filepath.Join(t.TempDir(), "moved")
		if err := os.Rename(filepath.Dir(otFile(p)), moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, filepath.Dir(otFile(p))); err != nil {
			t.Fatal(err)
		}
		unavailable(row, "")
	})
	t.Run("not written yet", func(t *testing.T) {
		p, row := otSession(t, r, "missing", osPrompt("acme one"))
		if err := os.Remove(otFile(p)); err != nil {
			t.Fatal(err)
		}
		page := r.transcriptPage(row.ID, "")
		if len(page.Entries) != 0 || page.Cursor != nil || page.HasMore {
			t.Fatalf("missing file answered %+v, want no entries, null cursor, hasMore false", page)
		}
		otRefusal(t, r.transcriptGet(row.ID, "before=10"), 400, "invalid_cursor")
	})
}

// Regression: a window holding no complete record ends the walk or drops the older records.
func TestOutsideTranscriptOversizedNewestRecord(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	lines := append(otConversation(6, ""), osAnswer(strings.Repeat("y", 300000)))
	_, row := otSession(t, r, "oversized", lines...)
	page := r.transcriptPage(row.ID, "")
	if len(page.Entries) != 0 || page.Cursor == nil || !page.HasMore {
		t.Fatalf("page = %d entries cursor %v hasMore %v, want none, a cursor and hasMore true", len(page.Entries), page.Cursor, page.HasMore)
	}
	var got []string
	for pages := 0; page.Cursor != nil; pages++ {
		if pages > 10 {
			t.Fatalf("still paging after %d pages", pages)
		}
		page = r.transcriptPage(row.ID, "before="+*page.Cursor)
		got = append(otTexts(page.Entries), got...)
	}
	for k := 0; k < 6; k++ {
		if !slices.Contains(got, fmt.Sprintf("acme %04d ", k)) {
			t.Fatalf("following the cursor never reached record %d: %q", k, got)
		}
	}
}

// Regression: a malformed query string reaches the handler as a valid request.
func TestOutsideTranscriptMalformedQueryIsInvalidRequest(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	_, row := otSession(t, r, "badquery", otConversation(4, "")...)
	for _, q := range []string{"limit=%zz", "before=5;x=1"} {
		otRefusal(t, r.transcriptGet(row.ID, q), 400, "invalid_request")
	}
}
