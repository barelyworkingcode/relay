package sshhost

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	runnerfiles "github.com/barelyworkingcode/relay/internal/sessions/runner"
)

// An adopt on a host is one detached script (docs/session-adoption.md,
// "Sessions on SSH hosts"). The SSH command writes it as adopt.sh beside the
// session's run folders, starts it detached and only reads adopt.state, so a
// dropped link cannot leave a half-done transition.

// AdoptPins is what the script checks before it signals the outside process.
type AdoptPins struct {
	PID                                      int
	Lifetime, ConversationID, ChildTest      string
	Interrupt, NoSignal                      bool
	TranscriptSize, WindowFromMs, WindowToMs int64
}

// AdoptOutcome is the terminal line of adopt.state. Result is "ok" (with
// Generation), "refused" (Code busy or changed) or "failed" (Code stop_failed
// or launch_failed).
type AdoptOutcome struct{ Result, Code, Generation string }

// ErrAdoptLinkLost marks an adopt stream that ended with no terminal line: the
// script may still be running, or may have finished unseen.
var ErrAdoptLinkLost = errors.New("sshhost: the adopt result did not arrive")

const adoptSentinel = "@@RELAY_ADOPT@@"

// adoptSteps runs between the prelude and the start. Each refusal writes the
// one terminal line and exits; the EXIT trap writes failed launch_failed for
// any exit that wrote none, so the file always ends with exactly one.
//
// Deliberate: adopt_lifetime computes the pid's lifetime exactly as the
// outside probe does (outsidesessions.UnixProbeScript), because the pin is
// compared byte for byte. ps is called by name.
const adoptSteps = `
trap '' HUP
done_line=
printf '%s %s\n' "$$" "$(lstart "$$")" > "$base/adopt.lock/owner"
state() { printf '%s\n' "$1" >> "$base/adopt.state"; }
terminal() { [ -z "$done_line" ] || return 0; done_line=1; state "$1"; rm -rf "$base/adopt.lock"; }
trap '[ -n "$done_line" ] || terminal "failed launch_failed"; rm -rf "$base/adopt.lock"' EXIT
refuse() { terminal "refused $1"; exit 0; }
now_ms() {
  ms=$(date +%s%3N 2>/dev/null)
  case "$ms" in ''|*[!0-9]*) ms="$(date +%s)000";; esac
  printf %s "$ms"
}
adopt_lifetime() {
  if [ -d "/proc/$1" ]; then
    awk -v hz="$(getconf CLK_TCK)" -v boot="$(awk '/^btime /{print $2}' /proc/stat)" '{printf "%.6f",boot+$22/hz}' "/proc/$1/stat" 2>/dev/null
  else
    TZ=UTC LC_ALL=C ps -o lstart= -p "$1" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ //;s/ $//'
  fi
}
epoch_of() {
  if [ -d "/proc/$1" ]; then
    awk -v hz="$(getconf CLK_TCK)" -v boot="$(awk '/^btime /{print $2}' /proc/stat)" '{printf "%d",boot+$22/hz}' "/proc/$1/stat" 2>/dev/null
  else
    ls=$(TZ=UTC LC_ALL=C ps -o lstart= -p "$1" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ //;s/ $//')
    [ -n "$ls" ] || return 0
    TZ=UTC LC_ALL=C date -j -u -f '%a %b %e %H:%M:%S %Y' "$ls" +%s 2>/dev/null || TZ=UTC date -u -d "$ls" +%s 2>/dev/null
  fi
}
alive() {
  kill -0 "$a_pid" 2>/dev/null || return 1
  case "$(ps -o stat= -p "$a_pid" 2>/dev/null)" in *Z*) return 1;; esac
  return 0
}
registry_check() {
  reg="$HOME/.claude/sessions/$a_pid.json"
  [ -f "$reg" ] && [ ! -L "$reg" ] && [ "$(wc -c < "$reg" | tr -d ' ')" -le 65536 ] || refuse changed
  sid=$(sed -n 's/.*"sessionId" *: *"\([A-Za-z0-9_-]*\)".*/\1/p' "$reg" | head -n1)
  [ -n "$sid" ] && [ "$sid" = "$a_conv" ] || refuse changed
  status=$(sed -n 's/.*"status" *: *"\([^"]*\)".*/\1/p' "$reg" | head -n1)
}
transcript_size() {
  d="$HOME/.claude/projects/$(printf '%s' "$cwd" | sed 's/[^A-Za-z0-9]/-/g')"
  f="$d/$a_conv.jsonl"
  [ ! -L "$HOME/.claude" ] && [ ! -L "$HOME/.claude/projects" ] || return 1
  if [ -e "$f" ]; then
    [ -d "$d" ] && [ ! -L "$d" ] && [ -f "$f" ] && [ ! -L "$f" ] || return 1
    cursize=$(wc -c < "$f" | tr -d ' ')
  else
    cursize=0
  fi
  case "$cursize" in ''|*[!0-9]*) return 1;; esac
}
children_idle() {
  [ "$a_child" != none ] || return 0
  all=$(ps -axo pid=,ppid= 2>/dev/null) || return 1
  kids=$(printf '%s\n' "$all" | awk -v p="$a_pid" '$2==p{print $1}')
  for k in $kids; do
    comm=$(ps -o comm= -p "$k" 2>/dev/null | sed 's/^ *//;s/ *$//')
    [ -n "$comm" ] || continue
    [ "$comm" != /usr/bin/caffeinate ] || continue
    [ "$a_child" != all ] || return 1
    born=$(epoch_of "$k")
    [ -n "$born" ] || return 1
    if [ "$born" -ge $((a_from/1000)) ] && [ "$born" -le $((a_to/1000)) ]; then return 1; fi
  done
  return 0
}
idle_check() {
  case "$status" in running|working|busy|asking|waiting|permission|waiting_for_permission|waiting-on-permission) return 1;; esac
  if [ -d "/proc/$a_pid" ]; then cwd=$(readlink "/proc/$a_pid/cwd"); else cwd=$(lsof -a -p "$a_pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p'); fi
  [ -n "$cwd" ] || return 1
  transcript_size && [ "$cursize" = "$a_tsize" ] || return 1
  children_idle || return 1
  transcript_size && [ "$cursize" = "$a_tsize" ] || return 1
}
wait_gone() {
  w=0; max=$(($1*10))
  while alive && [ "$w" -lt "$max" ]; do
    if sleep 0.1 2>/dev/null; then w=$((w+1)); else sleep 1; w=$((w+10)); fi
  done
  if alive && [ "$(adopt_lifetime "$a_pid")" = "$a_life" ]; then return 1; fi
  return 0
}
step() {
  cur=$(adopt_lifetime "$a_pid")
  [ -n "$cur" ] || return 0
  if [ "$cur" != "$a_life" ]; then
    [ "$signalled" = 0 ] || return 0
    return 2
  fi
  kill -s "$1" "$a_pid" 2>/dev/null || { alive || return 0; return 1; }
  signalled=1
  state "signal $1 $(now_ms)"
  wait_gone "$2"
}
ladder() {
  signalled=0
  if [ "$a_int" = 1 ]; then
    step INT 5; rc=$?
    [ "$rc" -ne 2 ] || refuse changed
    [ "$rc" -ne 0 ] || return 0
  fi
  step TERM 5; rc=$?
  [ "$rc" -ne 2 ] || refuse changed
  [ "$rc" -ne 0 ] || return 0
  step KILL 5; rc=$?
  [ "$rc" -ne 2 ] || refuse changed
  [ "$rc" -eq 0 ] || { terminal "failed stop_failed"; exit 0; }
}
case "$a_pid" in ''|*[!0-9]*|0) refuse changed;; esac
cur=$(adopt_lifetime "$a_pid")
if [ -n "$cur" ]; then
  if [ "$a_nosig" = 1 ]; then
    if [ "$cur" = "$a_life" ] && alive; then refuse changed; fi
  else
    [ "$cur" = "$a_life" ] || refuse changed
    registry_check
    if [ "$a_int" != 1 ] && ! idle_check; then refuse busy; fi
    ladder
  fi
fi
`

// adoptOuter is the SSH command's script: $1 to $7 are the start's arguments.
// It writes adopt.sh from its heredoc, starts it detached, and prints each new
// adopt.state line behind the sentinel until the terminal one. Exit 6 is a
// result that did not come in time. Deliberate: adopt.lock (a directory holding
// the owner's pid and start time) is taken before adopt.state is touched and
// kept by adopt.sh until it ends, so two scripts never run for one session; a
// lock whose owner is gone or has another start time is taken over. A setup
// failure prints "failed setup" and a held lock "failed in_progress", on stdout
// only: nothing was touched. HUP is ignored so a dropped
// link leaves the detached script running; the dots on stderr end this reader
// when the link is gone.
const adoptOuter = agentPrelude + `
lock="$base/adopt.lock"
fail() { rm -rf "$lock" 2>/dev/null; echo "` + adoptSentinel + ` failed setup"; exit 3; }
mkdir -p "$base" 2>/dev/null || { echo "` + adoptSentinel + ` failed setup"; exit 3; }
lock_held() {
  o=$(cat "$lock/owner" 2>/dev/null)
  opid=${o%% *}; olife=${o#* }
  case "$opid" in ''|*[!0-9]*) return 1;; esac
  [ -n "$olife" ] && [ "$(lstart "$opid")" = "$olife" ]
}
if ! mkdir "$lock" 2>/dev/null; then
  [ -f "$lock/owner" ] || sleep 1
  if lock_held; then echo "` + adoptSentinel + ` failed in_progress"; exit 0; fi
  mv "$lock" "$lock.old.$$" 2>/dev/null; rm -rf "$lock.old.$$"
  mkdir "$lock" 2>/dev/null || { echo "` + adoptSentinel + ` failed in_progress"; exit 0; }
fi
printf '%s %s\n' "$$" "$(lstart "$$")" > "$lock/owner" || fail
st="$base/adopt.state"
: > "$st" || fail
: > "$base/adopt.log" || fail
cat > "$base/adopt.sh" <<'RELAY_ADOPT_EOF'
@@ADOPT_SCRIPT@@
RELAY_ADOPT_EOF
[ -s "$base/adopt.sh" ] || fail
trap '' HUP
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c "$(cat "$base/adopt.sh")" relay-adopt-run "$@" </dev/null >>"$base/adopt.log" 2>&1 &
else
  set -m
  sh -c "$(cat "$base/adopt.sh")" relay-adopt-run "$@" </dev/null >>"$base/adopt.log" 2>&1 &
  set +m
fi
n=0; i=0
if sleep 0.1 2>/dev/null; then tick=0.1; max=900; else tick=1; max=90; fi
while [ "$i" -lt "$max" ]; do
  total=$(wc -l < "$st" | tr -d ' ')
  if [ "$total" -gt "$n" ]; then
    chunk=$(sed -n "$((n+1)),${total}p" "$st")
    printf '%s\n' "$chunk" | sed 's/^/` + adoptSentinel + ` /' || exit 1
    n=$total
    if printf '%s\n' "$chunk" | grep -Eq '^(ok|refused|failed) '; then exit 0; fi
  fi
  sleep "$tick"
  i=$((i+1))
  if [ $((i % 50)) -eq 0 ]; then printf . >&2 || exit 1; fi
done
exit 6
`

// adoptPinsHeader renders the pins as shell assignments. Each value is cut
// down to the characters its field can hold, so none can end its quoting or
// the heredoc; a value cut short fails its comparison and refuses.
func adoptPinsHeader(p AdoptPins) string {
	b2i := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	keep := func(s, allowed string) string {
		return strings.Map(func(r rune) rune {
			if strings.ContainsRune(allowed, r) {
				return r
			}
			return -1
		}, s)
	}
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	child := p.ChildTest
	if child != "none" && child != "window" {
		child = "all"
	}
	pid := p.PID
	if pid < 0 {
		pid = 0
	}
	return "a_pid=" + strconv.Itoa(pid) + "\n" +
		"a_life=" + shQuote(keep(p.Lifetime, alnum+" .:")) + "\n" +
		"a_conv=" + shQuote(keep(p.ConversationID, alnum+"-")) + "\n" +
		"a_child=" + child + "\n" +
		"a_int=" + b2i(p.Interrupt) + "\n" +
		"a_nosig=" + b2i(p.NoSignal) + "\n" +
		"a_tsize=" + strconv.FormatInt(max(p.TranscriptSize, 0), 10) + "\n" +
		"a_from=" + strconv.FormatInt(max(p.WindowFromMs, 0), 10) + "\n" +
		"a_to=" + strconv.FormatInt(max(p.WindowToMs, 0), 10) + "\n"
}

// adoptCommand joins the pieces: the prelude, the pins, the adopt steps, the
// start exactly as RunnerStartIn runs it (launch is launch_plain or the pane
// form), and the terminal line.
func adoptCommand(hostOS string, pins AdoptPins, startHead, launch string, args ...string) string {
	script := agentPrelude + adoptPinsHeader(pins) + adoptSteps +
		strings.TrimPrefix(startHead, agentPrelude) + launch + agentStartWait + `terminal "ok $gen"` + "\n"
	argv := append([]string{"sh", "-c", strings.Replace(adoptOuter, "@@ADOPT_SCRIPT@@", script, 1), "relay-adopt"}, args...)
	return RemoteCommandForOS(hostOS, "", argv, nil)
}

// AdoptStartIn is the adopt command for a host whose agent runs under the
// plain runner. dir, argv, env and p are RunnerStartIn's.
func AdoptStartIn(hostOS, dir string, d RunDir, argv []string, env map[string]string, p runnerfiles.Patterns, pins AdoptPins) string {
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(dir, argv, env)))
	return adoptCommand(hostOS, pins, agentStartHead, `launch_plain "$@"`+"\n", d.Session, d.Generation, script, p.TurnEnded, p.Asking)
}

// AdoptStartInPane is AdoptStartIn with the runner as the pane of the tmux
// session pane, as RunnerStartInPane starts it.
func AdoptStartInPane(hostOS, dir string, d RunDir, argv []string, env map[string]string, p runnerfiles.Patterns, tmuxPath, pane string, pins AdoptPins) string {
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(dir, argv, env)))
	return adoptCommand(hostOS, pins, agentStartHead, agentPaneLaunch+`pane_launch "$@"`+"\n", d.Session, d.Generation, script, p.TurnEnded, p.Asking, tmuxPath, pane)
}

// ParseAdoptResult returns the first terminal line of the adopt stream. A
// stream with none is ErrAdoptLinkLost, as is a terminal line it does not know:
// the script's state is then unknown too.
func ParseAdoptResult(out string) (AdoptOutcome, error) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != adoptSentinel || f[1] == "signal" {
			continue
		}
		switch {
		case f[1] == "ok" && len(f) == 3 && isGeneration(f[2]):
			return AdoptOutcome{Result: "ok", Generation: f[2]}, nil
		case f[1] == "refused" && len(f) == 3 && (f[2] == "busy" || f[2] == "changed"):
			return AdoptOutcome{Result: "refused", Code: f[2]}, nil
		case f[1] == "failed" && len(f) == 3 && (f[2] == "stop_failed" || f[2] == "launch_failed" || f[2] == "setup" || f[2] == "in_progress"):
			return AdoptOutcome{Result: "failed", Code: f[2]}, nil
		}
		return AdoptOutcome{}, fmt.Errorf("%w: unexpected line %q", ErrAdoptLinkLost, l)
	}
	return AdoptOutcome{}, ErrAdoptLinkLost
}

func isGeneration(s string) bool {
	if len(s) < 2 || s[0] != 'g' {
		return false
	}
	_, err := strconv.ParseUint(s[1:], 10, 32)
	return err == nil
}
