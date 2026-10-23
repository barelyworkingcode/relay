package sessionhistory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/barelyworkingcode/relay/internal/outsidesessions"
)

const (
	claudeListCap     = 4096
	claudeReadsMax    = 64
	claudeTitleWindow = 256 << 10
	claudeHeadWindow  = 64 << 10
	claudeTailWindow  = 16 << 10
	claudeTitleLine   = 4 << 10
	claudePromptLine  = 8 << 10
	titleRunesMax     = 120
)

var (
	folderCharset = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	entrypointRe  = regexp.MustCompile(`"entrypoint"\s*:\s*"([^"]*)"`)
	timestampRe   = regexp.MustCompile(`"timestamp"\s*:\s*"([^"]+)"`)
)

type claudeFile struct {
	path        string
	size, mtime int64
}

// claudeCached is what one scan learned about one file; entry is nil for a
// file that is not an interactive session, so it is not read again until it changes.
type claudeCached struct {
	size, mtime int64
	entry       *Entry
}

type claude struct {
	mu    sync.Mutex
	cache map[string]map[string]claudeCached // machine key, then path
}

// NewClaude returns the reader of ~/.claude/projects transcripts.
func NewClaude() Reader { return &claude{cache: map[string]map[string]claudeCached{}} }

func (c *claude) Agent() string { return "claude" }

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const claudeScriptHead = `b64() { base64 | tr -d '\r\n'; }
root="$HOME/.claude/projects"
`

const claudeRootCheck = `[ ! -L "$HOME/.claude" ] && [ ! -L "$root" ] && [ -d "$root" ] || exit 0
`

// claudeListScript prints the home, then "F folder/stem size mtime" per regular
// transcript at depth 2, and "OK" once it ran to the end. "X" before "OK" means
// the cap was reached. A symlink on the file, its folder, the root or ~/.claude
// is refused, and nothing is written. Every prefix passes a charset check, so
// none can carry shell syntax.
func claudeListScript(prefixes []string) string {
	filter := ""
	if prefixes != nil {
		var pats []string
		for _, p := range prefixes {
			if folderCharset.MatchString(p) {
				pats = append(pats, p+"*")
			}
		}
		if len(pats) == 0 {
			filter = ` continue` + "\n"
		} else {
			filter = ` case "$base" in ` + strings.Join(pats, "|") + ") ;; *) continue;; esac\n"
		}
	}
	list := `find "$root" -mindepth 2 -maxdepth 2 -type f -name '*.jsonl' -exec stat `
	return claudeScriptHead + `printf 'H\t'; printf '%s' "$HOME" | b64; printf '\n'
` + claudeRootCheck + `{ if [ "$(uname)" = Darwin ]; then ` + list + `-f '%m %z %N' {} +; else ` + list + `-c '%Y %s %n' {} +; fi; } 2>/dev/null | sort -rn | {
n=0; over=0
while read -r mt sz f; do
 case "$sz" in ''|*[!0-9]*) continue;; esac
 case "$mt" in ''|*[!0-9]*) continue;; esac
 d=${f%/*}; base=${d##*/}; file=${f##*/}; stem=${file%.jsonl}
 case "$base" in ''|*[!A-Za-z0-9-]*) continue;; esac
 case "$stem" in ????????-????-????-????-????????????) ;; *) continue;; esac
 case "$stem" in *[!0-9A-Fa-f-]*) continue;; esac
` + filter + ` [ ! -L "$f" ] && [ ! -L "$d" ] || continue
 [ "$n" -lt ` + strconv.Itoa(claudeListCap) + ` ] || { over=1; break; }
 printf 'F\t%s/%s\t%s\t%s\n' "$base" "$stem" "$sz" "$mt"
 n=$((n+1))
done
[ "$over" = 1 ] && echo X
echo OK
}
`
}

// claudeReadScript prints, per path, one "F" line of base64 fields: the path,
// the last custom-title and ai-title record of the last 256 KiB, the first four
// user records that are not meta or sidechain, 8 KiB in all, the first record carrying entrypoint,
// the first record, and the last 16 KiB. It reads and writes nothing else.
func claudeReadScript(paths []string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = shQuote(p)
	}
	title := func(kind string) string {
		return `tail -c ` + strconv.Itoa(claudeTitleWindow) + ` "$f" 2>/dev/null | grep -a '"type":"` + kind + `"' | tail -n 1 | head -c ` + strconv.Itoa(claudeTitleLine) + ` | b64`
	}
	head := `head -c ` + strconv.Itoa(claudeHeadWindow) + ` "$f" 2>/dev/null`
	return claudeScriptHead + claudeRootCheck + `for f in ` + strings.Join(quoted, " ") + `; do
 d=${f%/*}
 [ -f "$f" ] && [ ! -L "$f" ] && [ ! -L "$d" ] || continue
 printf 'F\t'; printf '%s' "$f" | b64
 printf '\t'; ` + title("custom-title") + `
 printf '\t'; ` + title("ai-title") + `
 printf '\t'; ` + head + ` | grep -a '"type":"user"' | grep -av '"isMeta":true' | grep -av '"isSidechain":true' | head -n 4 | head -c ` + strconv.Itoa(claudePromptLine) + ` | b64
 printf '\t'; ` + head + ` | grep -a -m 1 '"entrypoint"' | head -c ` + strconv.Itoa(claudePromptLine) + ` | b64
 printf '\t'; ` + head + ` | head -n 1 | head -c ` + strconv.Itoa(claudeTitleLine) + ` | b64
 printf '\t'; tail -c ` + strconv.Itoa(claudeTailWindow) + ` "$f" 2>/dev/null | b64
 printf '\n'
done
`
}

func unb64(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseClaudeList returns the home, the files and whether the listing is a
// complete answer. A path that is not exactly home/.claude/projects/<folder>/<uuid>.jsonl
// under an allowed prefix is dropped, whatever the script printed.
func parseClaudeList(data []byte, prefixes []string) (home string, files []claudeFile, answered, complete bool) {
	over, done := false, false
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case f[0] == "H" && len(f) == 2:
			home = unb64(f[1])
		case f[0] == "X":
			over = true
		case f[0] == "OK":
			done = true
		case f[0] == "F" && len(f) == 4:
			size, e1 := strconv.ParseInt(f[2], 10, 64)
			mtime, e2 := strconv.ParseInt(f[3], 10, 64)
			if e1 != nil || e2 != nil || home == "" {
				continue
			}
			files = append(files, claudeFile{f[1], size, mtime})
		}
	}
	if home == "" || !strings.HasPrefix(home, "/") {
		return "", nil, false, false
	}
	root := strings.TrimRight(home, "/") + "/.claude/projects/"
	kept := files[:0]
	for _, f := range files {
		folder, stem, ok := strings.Cut(f.path, "/")
		if !ok || !folderCharset.MatchString(folder) || !outsidesessions.IsConversationID(stem) || !hasPrefix(folder, prefixes) {
			continue
		}
		f.path = root + folder + "/" + stem + ".jsonl"
		kept = append(kept, f)
	}
	return home, kept, done, !over
}

func hasPrefix(folder string, prefixes []string) bool {
	if prefixes == nil {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(folder, p) {
			return true
		}
	}
	return false
}

// Discover lists the machine's interactive transcripts. It reads only files new
// or changed since the last scan, 64 at most, newest first; the rest wait for
// the next scan.
func (c *claude) Discover(ctx context.Context, m Machine) (Listing, error) {
	data, err := m.Run(ctx, claudeListScript(m.Prefixes))
	if err != nil {
		return Listing{}, err
	}
	home, files, answered, complete := parseClaudeList(data, m.Prefixes)
	if !answered {
		return Listing{}, nil
	}
	c.mu.Lock()
	cache := c.cache[m.Key]
	if cache == nil {
		cache = map[string]claudeCached{}
		c.cache[m.Key] = cache
	}
	listed := make(map[string]claudeFile, len(files))
	var changed []claudeFile
	for _, f := range files {
		listed[f.path] = f
		if got, ok := cache[f.path]; !ok || got.size != f.size || got.mtime != f.mtime {
			changed = append(changed, f)
		}
	}
	for path := range cache {
		if _, ok := listed[path]; !ok {
			delete(cache, path)
		}
	}
	c.mu.Unlock()
	sort.Slice(changed, func(a, b int) bool { return changed[a].mtime > changed[b].mtime })
	if len(changed) > claudeReadsMax {
		changed = changed[:claudeReadsMax]
	}
	if len(changed) > 0 {
		paths := make([]string, len(changed))
		for i, f := range changed {
			paths[i] = f.path
		}
		out, err := m.Run(ctx, claudeReadScript(paths))
		if err != nil {
			return Listing{}, err
		}
		reads := parseClaudeReads(out)
		c.mu.Lock()
		for _, f := range changed {
			if fields, ok := reads[f.path]; ok {
				cache[f.path] = claudeCached{f.size, f.mtime, buildClaudeEntry(home, f, fields)}
			}
		}
		c.mu.Unlock()
	}
	listing := Listing{Answered: true, Complete: complete, Scope: m.Prefixes, Present: make(map[string]bool, len(files))}
	c.mu.Lock()
	for _, f := range files {
		listing.Present[PresentKey("claude", strings.TrimSuffix(f.path[strings.LastIndex(f.path, "/")+1:], ".jsonl"))] = true
		if got, ok := cache[f.path]; ok && got.entry != nil {
			listing.Entries = append(listing.Entries, *got.entry)
		}
	}
	c.mu.Unlock()
	return listing, nil
}

// claudeFields are the decoded pieces of one file's read.
type claudeFields struct {
	custom, ai, prompt, entrypoint, first, tail string
}

func parseClaudeReads(data []byte) map[string]claudeFields {
	out := map[string]claudeFields{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if f[0] != "F" || len(f) != 8 {
			continue
		}
		out[unb64(f[1])] = claudeFields{unb64(f[2]), unb64(f[3]), unb64(f[4]), unb64(f[5]), unb64(f[6]), unb64(f[7])}
	}
	return out
}

// buildClaudeEntry turns one file's fields into an entry, or nil when the file
// is not an interactive session relay can place. Transcript text is untrusted.
func buildClaudeEntry(home string, f claudeFile, in claudeFields) *Entry {
	if m := entrypointRe.FindStringSubmatch(in.entrypoint); m == nil || m[1] != "cli" {
		return nil
	}
	stem := strings.TrimSuffix(f.path[strings.LastIndex(f.path, "/")+1:], ".jsonl")
	mtime := time.Unix(f.mtime, 0)
	e := &Entry{Agent: "claude", ConversationID: stem, LastActiveAt: mtime, CreatedAt: mtime}

	lines := strings.Split(in.tail, "\n")
	if len(in.tail) >= claudeTailWindow {
		// A full tail starts mid-record.
		lines = lines[1:]
	}
	var newest time.Time
	for j := len(lines) - 1; j >= 0; j-- {
		var rec struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(lines[j]), &rec) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && t.After(newest) {
			newest = t
		}
		if e.Directory == "" && rec.SessionID == stem && rec.Cwd != "" {
			if dir, ok := outsidesessions.LaunchFolder(home, rec.Cwd, stem, f.path); ok {
				e.Directory = dir
			}
		}
	}
	if e.Directory == "" {
		return nil
	}
	if !newest.IsZero() {
		e.LastActiveAt = newest
	}
	if t, ok := recordTime(in.first); ok {
		e.CreatedAt = t
	} else if t, ok := recordTime(in.entrypoint); ok {
		e.CreatedAt = t
	}

	if t := cleanTitle(jsonField(in.custom, "customTitle")); t != "" {
		e.Title, e.TitleSource = t, "client"
	} else if t := cleanTitle(jsonField(in.ai, "aiTitle")); t != "" {
		e.Title, e.TitleSource = t, "client"
	}
	for _, line := range strings.Split(in.prompt, "\n") {
		if e.Title != "" {
			break
		}
		if t := cleanTitle(promptTitle(line)); t != "" {
			e.Title, e.TitleSource = t, "prompt"
		}
	}
	return e
}

func jsonField(line, key string) string {
	var rec map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &rec) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(rec[key], &s) != nil {
		return ""
	}
	return s
}

// recordTime reads a record's timestamp; a cut record still yields it when the
// key arrived whole.
func recordTime(line string) (time.Time, bool) {
	m := timestampRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	return t, err == nil
}

// promptTitle is the first non-empty line of the user record's text that does
// not start with "/" or "<": a slash command or a wrapped notice is not a prompt.
func promptTitle(line string) string {
	var rec struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil {
		return ""
	}
	var text string
	if json.Unmarshal(rec.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "text" {
				text += b.Text + "\n"
			}
		}
	}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "/") && !strings.HasPrefix(l, "<") {
			return l
		}
	}
	return ""
}

// cleanTitle drops control and format characters (bidi overrides, zero-width), trims, and caps at 120 runes.
func cleanTitle(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > titleRunesMax {
		s = strings.TrimSpace(string(r[:titleRunesMax]))
	}
	return s
}
