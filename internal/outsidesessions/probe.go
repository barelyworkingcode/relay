package outsidesessions

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// UnixProbeScript needs only the host's existing shell, ps and base64. Linux
// uses procfs; macOS uses lsof. Raw argv/environment never leave the subprocess.
// The 32-session cap is applied to sessions with a registry entry first, newest
// pid first, so a busy machine drops its oldest unregistered processes.
const UnixProbeScript = `
b64() { base64 | tr -d '\r\n'; }
printf 'H\t'; printf '%s' "$HOME" | b64; printf '\n'
unset $(env | sed -n 's/^\(GIT_[A-Za-z0-9_]*\)=.*/\1/p')
unset TMUX
[ -d "$HOME/.claude/relay-outside-reports" ] && printf 'S\t1\n'
ps -axo pid=,ppid= | awk '{printf "A\t%s\t%s\n",$1,$2}'
candidates=$(TZ=UTC LC_ALL=C ps -axo pid=,ppid=,lstart=,comm= | awk '{n=$NF; sub(/.*\//,"",n)} n=="claude"||n=="claude.exe"||n=="node"||n=="node.exe"' | sort -rn)
{ printf '%s\n' "$candidates" | while read -r pid rest; do [ ! -f "$HOME/.claude/sessions/$pid.json" ] || printf '%s %s\n' "$pid" "$rest"; done
  printf '%s\n' "$candidates" | while read -r pid rest; do [ -f "$HOME/.claude/sessions/$pid.json" ] || printf '%s %s\n' "$pid" "$rest"; done; } | {
live=0
while read -r pid ppid day mon dom clock year command; do
 base=${command##*/}; case "$base" in claude|claude.exe) ;; node|node.exe) args=$(ps -p "$pid" -o args=); case "$args" in *'/claude '*|*'/claude.js '*|*'/cli.js '*) ;; *) continue;; esac ;; *) continue;; esac
 [ "$live" -lt 32 ] || break
 if [ -d "/proc/$pid" ]; then cwd=$(readlink "/proc/$pid/cwd"); lifetime=$(awk -v hz="$(getconf CLK_TCK)" -v boot="$(awk '/^btime /{print $2}' /proc/stat)" '{printf "%.6f",boot+$22/hz}' "/proc/$pid/stat"); marker=$(tr '\000' '\n' < "/proc/$pid/environ" 2>/dev/null | sed -n 's/^RELAY_SESSION_ID=//p'); else cwd=$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p'); lifetime="$day $mon $dom $clock $year"; marker=$(ps eww -p "$pid" -o command= | sed -n 's/.* RELAY_SESSION_ID=\([^ ]*\).*/\1/p'); fi
 [ -n "$cwd" ] || continue
 live=$((live+1))
 branch=$(cd "$cwd" 2>/dev/null && env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_CONFIG -u GIT_CONFIG_COUNT -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES git -c core.fsmonitor=false symbolic-ref --short -q HEAD 2>/dev/null)
 printf 'P\t%s\t%s\t' "$pid" "$ppid"; printf '%s' "$lifetime" | b64; printf '\t'; printf '%s' "$cwd" | b64; printf '\t'; printf '%s' "$branch" | b64; printf '\t'; printf '%s' "$marker" | b64; printf '\n'
 # Only this process's transcript is read: the registry session for its pid,
 # else the newest in its folder (then the first line identifies the session).
 sid=; rf="$HOME/.claude/sessions/$pid.json"
 if [ -f "$rf" ] && [ ! -L "$rf" ] && [ "$(wc -c < "$rf")" -le 65536 ]; then sid=$(sed -n 's/.*"sessionId" *: *"\([A-Za-z0-9_-]*\)".*/\1/p' "$rf" | head -n1); fi
 dir="$HOME/.claude/projects/$(printf '%s' "$cwd" | sed 's/[^A-Za-z0-9]/-/g')"
 [ -d "$dir" ] && [ ! -L "$dir" ] || continue
 if [ -n "$sid" ]; then f="$dir/$sid.jsonl"; else f=$(ls -t "$dir"/*.jsonl 2>/dev/null | head -n1); fi
 [ -n "$f" ] && [ -f "$f" ] && [ ! -L "$f" ] || continue
 printf 'F\t'; printf '%s' "$f" | b64; printf '\t'
 { if [ -z "$sid" ]; then head -n1 "$f" | head -c 32768; printf '\n'; fi; tail -c 16384 "$f"; } | b64; printf '\n'
done
}
n=0; for f in "$HOME"/.claude/sessions/*.json; do [ -f "$f" ] && [ ! -L "$f" ] || continue; [ "$n" -lt 512 ] || break; [ "$(wc -c < "$f")" -le 65536 ] || continue; printf 'R\t'; cat "$f" | b64; printf '\n'; n=$((n+1)); done
if command -v tmux >/dev/null 2>&1; then tmux list-panes -a -F 'T|#{pane_pid}|#{session_name}' 2>/dev/null | while IFS='|' read -r tag pid name; do printf 'T\t%s\t' "$pid"; printf '%s' "$name" | b64; printf '\n'; done; fi
`

type Process struct {
	PID, PPID                     int
	Lifetime, Cwd, Branch, Marker string
}
type Registry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"statusUpdatedAt"`
	Status    string `json:"status"`
	Name      string `json:"name"`
	Tmux      string `json:"tmux"`
	// ProcStart, Kind and Entrypoint decide whether the session may be adopted.
	// They are judged only; they never reach a response.
	ProcStart  string `json:"procStart"`
	Kind       string `json:"kind"`
	Entrypoint string `json:"entrypoint"`
}
type NativeProbe struct {
	Home       string
	Processes  map[int]Process
	Registries []Registry
	Panes      map[int]string
	Files      map[string][]byte
	SawSpool   bool
}

func decode(s string) string { b, _ := base64.StdEncoding.DecodeString(s); return string(b) }
func ParseNative(data []byte) (NativeProbe, error) {
	p := NativeProbe{Processes: map[int]Process{}, Panes: map[int]string{}, Files: map[string][]byte{}}
	if len(data) > 8<<20 {
		return p, fmt.Errorf("probe exceeded bound")
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch f[0] {
		case "S":
			p.SawSpool = true
		case "H":
			if len(f) == 2 {
				p.Home = decode(f[1])
			}
		case "P":
			if len(f) == 7 {
				pid, _ := strconv.Atoi(f[1])
				ppid, _ := strconv.Atoi(f[2])
				p.Processes[pid] = Process{pid, ppid, decode(f[3]), decode(f[4]), decode(f[5]), decode(f[6])}
			}
		case "R":
			if len(f) == 2 {
				var r Registry
				if json.Unmarshal([]byte(decode(f[1])), &r) == nil {
					p.Registries = append(p.Registries, r)
				}
			}
		case "T":
			if len(f) == 3 {
				pid, _ := strconv.Atoi(f[1])
				p.Panes[pid] = decode(f[2])
			}
		case "A":
			if len(f) == 3 {
				pid, _ := strconv.Atoi(f[1])
				ppid, _ := strconv.Atoi(f[2])
				if _, ok := p.Processes[pid]; !ok {
					p.Processes[pid] = Process{PID: pid, PPID: ppid}
				}
			}
		case "F":
			if len(f) == 3 {
				p.Files[decode(f[1])] = []byte(decode(f[2]))
			}
		}
	}
	if p.Home == "" {
		return p, fmt.Errorf("probe did not return home")
	}
	return p, nil
}

var sessionIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func TranscriptPath(home, cwd, id string) string {
	if !sessionIdentity.MatchString(id) {
		return ""
	}
	folder := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-")
	return strings.TrimRight(home, "/") + "/.claude/projects/" + folder + "/" + id + ".jsonl"
}
func Contains(root, cwd string) bool {
	if root == "" {
		return false
	}
	root = strings.TrimRight(root, "/")
	return cwd == root || strings.HasPrefix(cwd, root+"/")
}

// Observations refuses ambiguous old clients and checks the observed cwd,
// selected managed marker, ancestry and actual tmux pane ancestry independently.
func (p NativeProbe) Observations(managedPID int, managedIDs map[string]bool) []Observation {
	out := []Observation{}
	seen := map[int]bool{}
	for _, r := range p.Registries {
		proc, ok := p.Processes[r.PID]
		if !ok || proc.Lifetime == "" || proc.Cwd != r.Cwd || !registryLifetimeMatches(proc.Lifetime, r.StartedAt) || !sessionIdentity.MatchString(r.SessionID) || managedIDs[r.SessionID] || p.managed(proc, managedPID) {
			continue
		}
		seen[r.PID] = true
		out = append(out, p.observe(proc, r, true))
	}
	for pid, proc := range p.Processes {
		if seen[pid] || proc.Lifetime == "" || p.managed(proc, managedPID) {
			continue
		}
		count := 0
		for _, other := range p.Processes {
			if other.Lifetime != "" && other.Cwd == proc.Cwd {
				count++
			}
		}
		if count != 1 {
			continue
		}
		var candidate Registry
		matches := 0
		for path, data := range p.Files {
			for _, line := range strings.Split(string(data), "\n") {
				var meta struct {
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
					Timestamp string `json:"timestamp"`
				}
				if json.Unmarshal([]byte(line), &meta) == nil && meta.Cwd == proc.Cwd && sessionIdentity.MatchString(meta.SessionID) && !managedIDs[meta.SessionID] && path == TranscriptPath(p.Home, meta.Cwd, meta.SessionID) && transcriptLifetimeMatches(proc.Lifetime, meta.Timestamp) {
					candidate = Registry{PID: pid, SessionID: meta.SessionID, Cwd: meta.Cwd}
					matches++
					break
				}
			}
		}
		if matches == 1 {
			out = append(out, p.observe(proc, candidate, false))
		}
	}
	markAmbiguous(out)
	return out
}

// markAmbiguous withdraws adoption from every row whose conversation id more
// than one live process holds: nothing says which of them to end.
func markAmbiguous(obs []Observation) {
	held := map[string]int{}
	for _, o := range obs {
		held[o.ClaudeID]++
	}
	for i := range obs {
		if held[obs[i].ClaudeID] > 1 && obs[i].Session.Adoptable {
			obs[i].Session.Adoptable = false
			obs[i].Session.AdoptReason = AdoptReasonAmbiguous
		}
	}
}

// GateAdoption applies the two reasons that outrank the registry's: a session
// on an SSH host, then a Pi or Codex session outside every registered project.
// A Claude session outside every project is adoptable: relay resumes it in its
// own folder with no sandbox. Pi needs a project for its model key and its
// transcript grant, and a Codex attach needs one for launch authorization. A
// row they do not touch keeps the reason observe gave it.
func GateAdoption(s *Session, onHost, inProject bool, agent string) {
	switch {
	case onHost:
		s.AdoptReason = AdoptReasonHost
	case !inProject && (agent == "pi" || agent == "codex"):
		s.AdoptReason = AdoptReasonNoProject
	default:
		return
	}
	s.Adoptable = false
}

// AdoptReason judges the adoption tests that need only a registry entry and
// the kernel's start time of its process; "" means they all pass. The registry
// names a process, never a path, and its text grants nothing: each test is
// about the entry's shape, and the start time is the process's own.
func AdoptReason(r Registry, start time.Time) string {
	if !IsConversationID(r.SessionID) {
		return AdoptReasonInvalidID
	}
	registered, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(r.ProcStart), " "), time.UTC)
	if err != nil || !registered.Equal(start.UTC().Truncate(time.Second)) {
		return AdoptReasonStartMismatch
	}
	if r.Kind != "interactive" {
		return AdoptReasonNotInteractive
	}
	if r.Entrypoint != "cli" {
		return AdoptReasonNotCommandLine
	}
	return ""
}

// IsConversationID reports whether id is a canonical UUID, the only shape
// adoption accepts: the id becomes a --resume argument and a file name.
func IsConversationID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil && len(id) == 36
}
func (p NativeProbe) managed(proc Process, root int) bool {
	if proc.Marker != "" {
		return true
	}
	for n := 0; n < 64 && proc.PID > 0; n++ {
		if root > 0 && proc.PID == root {
			return true
		}
		next, ok := p.Processes[proc.PPID]
		if !ok || next.PID == proc.PID {
			break
		}
		proc = next
	}
	return false
}
func (p NativeProbe) observe(proc Process, r Registry, registered bool) Observation {
	s := Session{ClaudeSessionID: r.SessionID, Directory: r.Cwd, Branch: proc.Branch, State: "idle", Source: "probe", Since: stamp(sinceTime(proc.Lifetime, r.StartedAt)), ViewReason: "not_in_tmux"}
	s.Name = r.Name
	if s.Name == "" {
		s.Name = "Claude — " + filepath.Base(r.Cwd)
	}
	switch r.Status {
	case "running", "working", "busy":
		s.State = "running"
	case "asking", "waiting", "permission", "waiting_for_permission", "waiting-on-permission":
		s.State = "asking"
	}
	switch started, ok := lifetimeTime(proc.Lifetime); {
	case !registered:
		s.AdoptReason = AdoptReasonNotRegistered
	case !ok:
		s.AdoptReason = AdoptReasonStartMismatch
	default:
		s.AdoptReason = AdoptReason(r, started)
	}
	s.Adoptable = s.AdoptReason == ""
	transcript := TranscriptPath(p.Home, r.Cwd, r.SessionID)
	state, at := ProbeState(p.Files[transcript])
	statusAt := time.UnixMilli(r.UpdatedAt)
	if at.After(statusAt) {
		s.State = state
		statusAt = at
	}
	current := proc
	for n := 0; n < 64 && current.PID > 0; n++ {
		if name, ok := p.Panes[current.PID]; ok {
			s.TmuxSession = &name
			break
		}
		next, ok := p.Processes[current.PPID]
		if !ok || next.PID == current.PID {
			break
		}
		current = next
	}
	return Observation{Session: s, PID: proc.PID, Lifetime: proc.Lifetime, ClaudeID: r.SessionID, Transcript: transcript, StatusAt: statusAt}
}

// sinceTime prefers the registry start; a session with no registry file falls
// back to the process start so its row never reads 1970.
func sinceTime(lifetime string, started int64) time.Time {
	if started > 0 {
		return time.UnixMilli(started)
	}
	if t, ok := lifetimeTime(lifetime); ok {
		return t
	}
	return time.Time{}
}
func lifetimeTime(lifetime string) (time.Time, bool) {
	if strings.Contains(lifetime, ".") {
		sec, err := strconv.ParseFloat(lifetime, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(sec * 1000)), true
	}
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", lifetime, time.UTC)
	return t, err == nil
}
func registryLifetimeMatches(lifetime string, started int64) bool {
	if started <= 0 {
		return false
	}
	observed, ok := lifetimeTime(lifetime)
	if !ok {
		return false
	}
	delta := time.UnixMilli(started).Sub(observed)
	return delta >= -time.Second && delta < 30*time.Second
}

// Registry-less discovery must have a transcript boundary from this process
// lifetime, rather than resurrecting a previous session in the same folder.
func transcriptLifetimeMatches(lifetime, timestamp string) bool {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	return err == nil && registryLifetimeMatches(lifetime, t.UnixMilli())
}
