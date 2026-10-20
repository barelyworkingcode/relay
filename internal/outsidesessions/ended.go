package outsidesessions

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const EndedRowsMax = 50
const EndedListTailBytes = 16384

var folderPrefix = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
var folderMangle = regexp.MustCompile(`[^A-Za-z0-9]`)

// FolderName is the project folder name Claude derives from a working folder.
func FolderName(cwd string) string { return folderMangle.ReplaceAllString(cwd, "-") }

// EndedScanScript lists transcript files modified at or after cutoffUnix,
// newest first, at most EndedRowsMax of them, one "E" line each: the path, the
// mtime in seconds and the last EndedListTailBytes of the file, all but the
// mtime in base64. nil folderPrefixes scans every project folder; a non-nil
// slice scans only folders whose name starts with one of them. A symlink on
// the file, its folder, ~/.claude/projects or ~/.claude is refused. It writes
// nothing. A file that cannot be read is skipped.
//
// The cutoff is an integer and every prefix and held id passes a charset check
// here, so none of them can carry shell syntax.
func EndedScanScript(cutoffUnix int64, folderPrefixes []string, held []string) string {
	if cutoffUnix < 0 {
		cutoffUnix = 0
	}
	filter := ""
	if folderPrefixes != nil {
		var pats []string
		for _, p := range folderPrefixes {
			if folderPrefix.MatchString(p) {
				pats = append(pats, p+"*")
			}
		}
		if len(pats) == 0 {
			return "exit 0\n"
		}
		filter = ` case "$base" in ` + strings.Join(pats, "|") + ") ;; *) continue;; esac\n"
	}
	heldCase := ""
	var ids []string
	for _, id := range held {
		if sessionIdentity.MatchString(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		heldCase = ` case "$stem" in ` + strings.Join(ids, "|") + ") continue;; esac\n"
	}
	list := `find "$root" -mindepth 2 -maxdepth 2 -type f -name '*.jsonl' -exec stat `
	return `b64() { base64 | tr -d '\r\n'; }
printf 'H\t'; printf '%s' "$HOME" | b64; printf '\n'
root="$HOME/.claude/projects"
[ ! -L "$HOME/.claude" ] && [ ! -L "$root" ] && [ -d "$root" ] || exit 0
{ if [ "$(uname)" = Darwin ]; then ` + list + `-f '%m %N' {} +; else ` + list + `-c '%Y %n' {} +; fi; } 2>/dev/null | sort -rn | awk -v c=` +
		strconv.FormatInt(cutoffUnix, 10) + ` '$1+0>=c' | {
n=0
while read -r mt f; do
 [ "$n" -lt ` + strconv.Itoa(EndedRowsMax) + ` ] || break
 case "$mt" in ''|*[!0-9]*) continue;; esac
 d=${f%/*}; base=${d##*/}; file=${f##*/}; stem=${file%.jsonl}
 case "$stem" in ''|*[!A-Za-z0-9_-]*) continue;; esac
` + filter + heldCase + ` [ -f "$f" ] && [ ! -L "$f" ] && [ ! -L "$d" ] || continue
 tail -c ` + strconv.Itoa(EndedListTailBytes) + ` "$f" >/dev/null 2>&1 || continue
 printf 'E\t'; printf '%s' "$f" | b64; printf '\t%s\t' "$mt"; tail -c ` + strconv.Itoa(EndedListTailBytes) + ` "$f" 2>/dev/null | b64; printf '\n'
 n=$((n+1))
done
}
`
}

// ParseEnded turns EndedScanScript output into ended observations, newest
// first, at most EndedRowsMax. A row needs a record in the tail whose session
// id is the file's stem and whose cwd maps back to this very file under home;
// the file name alone, and the folder name, never decide the working folder.
// Empty home takes the script's own H line. Transcript text is display data.
func ParseEnded(data []byte, home string) []Observation {
	type file struct {
		path string
		at   time.Time
		tail []byte
	}
	var files []file
	scriptHome := ""
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case f[0] == "H" && len(f) == 2:
			scriptHome = decode(f[1])
		case f[0] == "E" && len(f) == 4:
			sec, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil || sec <= 0 {
				continue
			}
			files = append(files, file{decode(f[1]), time.Unix(sec, 0), []byte(decode(f[3]))})
		}
	}
	if home == "" {
		home = scriptHome
	}
	if home == "" {
		return nil
	}
	sort.SliceStable(files, func(a, b int) bool { return files[a].at.After(files[b].at) })
	out := []Observation{}
	seen := map[string]bool{}
	for _, f := range files {
		if len(out) >= EndedRowsMax {
			break
		}
		stem := strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
		if !sessionIdentity.MatchString(stem) || seen[stem] {
			continue
		}
		if o, ok := endedObservation(f.path, stem, f.at, f.tail, home); ok {
			seen[stem] = true
			out = append(out, o)
		}
	}
	return out
}

func endedObservation(path, stem string, at time.Time, tail []byte, home string) (Observation, bool) {
	lines := strings.Split(string(tail), "\n")
	if len(tail) >= EndedListTailBytes {
		// A full tail starts mid-record.
		lines = lines[1:]
	}
	var cwd, branch string
	var since time.Time
	for j := len(lines) - 1; j >= 0; j-- {
		var rec struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			GitBranch string `json:"gitBranch"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal([]byte(lines[j]), &rec) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && (since.IsZero() || t.Before(since)) {
			since = t
		}
		if cwd == "" && rec.SessionID == stem && rec.Cwd != "" && TranscriptPath(home, rec.Cwd, stem) == path {
			cwd, branch = rec.Cwd, rec.GitBranch
		}
	}
	if cwd == "" {
		return Observation{}, false
	}
	if since.IsZero() {
		since = at
	}
	s := Session{
		Name: "Claude — " + filepath.Base(cwd), Directory: cwd, Branch: branch, State: "ended",
		Since: stamp(since), Source: "probe", Outside: true, ReadOnly: true,
		ViewReason: "not_in_tmux", EndedAt: stamp(at),
	}
	return Observation{Session: s, ClaudeID: stem, Transcript: path, StatusAt: at}, true
}
