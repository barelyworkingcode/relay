package sshhost

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
	runnerfiles "github.com/barelyworkingcode/relay/internal/sessions/runner"
)

// A detached agent runs under ~/.relay/agents/<session>/<generation>/ on a
// Unix host (docs/ssh-hosts.md, "Detached sessions"). Every function here
// returns a complete remote command already wrapped by RemoteCommandForOS, so
// no caller can skip the wrapper. The scripts are POSIX sh and need no perl,
// python or setsid(1): macOS has none of them on a stock install.

// RunDir names one spawn's run directory. An empty Generation means the
// latest one for every operation but RunnerStart, where it asks the host to
// allocate the next.
type RunDir struct{ Session, Generation string }

// RunPhase is what RunnerStatus reports about a generation.
type RunPhase string

const (
	RunNone   RunPhase = "none"   // no generation exists
	RunAlive  RunPhase = "alive"  // the runner is running
	RunExited RunPhase = "exited" // the agent exited; Code is its exit code
	RunDead   RunPhase = "dead"   // the runner is gone and wrote no exit file
	// RunLegacy is a live runner from before runner.json: it reads its input
	// from in.jsonl and has no inbox. Relay stops it and resumes the session.
	RunLegacy RunPhase = "legacy"
)

// RunState is RunnerStatus's parsed answer.
type RunState struct {
	Phase      RunPhase
	Generation string
	Code       int
}

// agentPrelude resolves the run directory from $1 (session) and $2
// (generation, empty for the latest). It is deliberate that every value is
// checked against [A-Za-z0-9_-] before it reaches a path: the ids are relay's
// own, but a path built from a caller string must not escape the agents dir.
const agentPrelude = `oldumask=$(umask)
umask 077
case "$1" in ''|*[!A-Za-z0-9_-]*) echo "relay: invalid session id" >&2; exit 2;; esac
case "$2" in *[!A-Za-z0-9_-]*) echo "relay: invalid generation" >&2; exit 2;; esac
[ -n "$HOME" ] || { echo "relay: HOME is not set on the host" >&2; exit 2; }
base="$HOME/.relay/agents/$1"
lstart() { LC_ALL=C ps -o lstart= -p "$1" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ //;s/ $//'; }
latest() {
  n=0
  for e in "$base"/g*; do
    [ -d "$e" ] || continue
    k=${e##*/}; k=${k#g}
    case "$k" in ''|*[!0-9]*) continue;; esac
    [ "$k" -gt "$n" ] && n=$k
  done
  [ "$n" -gt 0 ] && echo "g$n"
}
resolve() {
  gen="$2"
  [ -n "$gen" ] || gen=$(latest)
  [ -n "$gen" ] || return 1
  dir="$base/$gen"
  [ -d "$dir" ]
}
state_file() {
  if [ -f "$dir/runner.json" ]; then state="$dir/runner.json"; elif [ -f "$dir/run.json" ]; then state="$dir/run.json"; else return 1; fi
}
runner_alive() {
  state_file || return 1
  pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$state")
  started=$(sed -n 's/.*"started":"\([^"]*\)".*/\1/p' "$state")
  [ -n "$pid" ] && [ -n "$started" ] || return 1
  [ "$(lstart "$pid")" = "$started" ]
}
`

// agentStartHead and agentStartWait bracket the launch step, which differs
// between the plain and the pane form. launch_plain is deliberate: the shell
// function sees the script's arguments only when called with "$@".
//
// agentStartScript takes $1 session, $2 generation (empty: allocate the next),
// $3 the base64 of the agent's own launch script, and $4 and $5 the runner's
// turn-ended and asking strings. The pane form adds $6 the tmux path and $7
// the pane's session name (pane.go). It returns only after runner.json exists,
// which is the completion handle a caller waits on.
var agentStartHead = agentPrelude + `
mkdir -p "$base" 2>/dev/null || { echo "relay: cannot create run directory $base" >&2; exit 3; }
if [ -n "$2" ]; then
  gen="$2"; dir="$base/$gen"
  mkdir "$dir" 2>/dev/null || { echo "relay: cannot create run directory $dir" >&2; exit 3; }
else
  tries=0
  while :; do
    cur=$(latest); n=${cur#g}; [ -n "$n" ] || n=0
    gen="g$((n+1))"; dir="$base/$gen"
    mkdir "$dir" 2>/dev/null && break
    tries=$((tries+1))
    if [ ! -d "$dir" ] || [ "$tries" -gt 20 ]; then echo "relay: cannot create run directory $dir" >&2; exit 3; fi
  done
fi
cd "$dir" || exit 3
: > out.jsonl; : > events.jsonl; : > stderr.log
mkdir inbox sent || { echo "relay: cannot create message directories in $dir" >&2; exit 3; }
printf %s "$3" | base64 -d > agent.sh || { echo "relay: cannot write agent script in $dir" >&2; exit 3; }
cat > runner.sh <<'RELAY_RUNNER_EOF'
` + runnerfiles.Script + `RELAY_RUNNER_EOF
launch_plain() {
  if command -v setsid >/dev/null 2>&1; then
    setsid sh "$dir/runner.sh" "$dir" "$oldumask" "$1" "$gen" "$4" "$5" </dev/null >/dev/null 2>&1 &
  else
    set -m
    sh "$dir/runner.sh" "$dir" "$oldumask" "$1" "$gen" "$4" "$5" </dev/null >/dev/null 2>&1 &
    set +m
  fi
  rpid=$!
}
`

const agentStartWait = `i=0
while [ ! -f "$dir/runner.json" ]; do
  i=$((i+1))
  if [ -f "$dir/norun" ] || [ "$i" -gt 100 ]; then
    [ -z "$rpid" ] || kill "$rpid" 2>/dev/null
    echo "relay: agent runner did not start in $dir" >&2
    exit 4
  fi
  sleep 0.1 2>/dev/null || sleep 1
done
echo "@@RELAY_RUN@@ $gen"
`

var agentStartScript = agentStartHead + `launch_plain "$@"
` + agentStartWait

const agentStatusScript = agentPrelude + `
resolve "$@" || { echo "@@RELAY_RUN@@ none"; exit 0; }
if [ -f "$dir/exit" ]; then
  code=$(sed -n 's/.*"code":\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$dir/exit")
  echo "@@RELAY_RUN@@ exited $gen ${code:-1}"
  exit 0
fi
if runner_alive; then
  if [ -f "$dir/runner.json" ]; then echo "@@RELAY_RUN@@ alive $gen"; else echo "@@RELAY_RUN@@ legacy $gen"; fi
else
  echo "@@RELAY_RUN@@ dead $gen"
fi
`

// agentSendScript takes $1 session, $2 generation, $3 message id, $4 kind. It
// writes stdin's line as inbox/<id>.<kind> by temp file and rename, then waits
// for the runner to move it to sent/: that move is the receipt. Exit 5: no run,
// or the runner is gone, and the file is removed. Exit 7: no receipt within 25
// seconds while the runner lives; the file stays queued and may still be
// delivered, so a caller must not send it again.
const agentSendScript = agentPrelude + `
case "$3" in ''|*[!0-9a-f-]*) echo "relay: invalid message id" >&2; exit 2;; esac
case "$4" in json|ctl) ;; *) echo "relay: invalid message kind" >&2; exit 2;; esac
resolve "$@" || { echo "relay: no agent run for this session" >&2; exit 5; }
if [ -f "$dir/exit" ] || ! runner_alive; then echo "relay: the agent is not running" >&2; exit 5; fi
[ -d "$dir/inbox" ] || { echo "relay: the agent is not running under a message runner" >&2; exit 5; }
name="$3.$4"
line=$(cat)
printf '%s\n' "$line" > "$dir/inbox/.$3.tmp" || exit 6
mv "$dir/inbox/.$3.tmp" "$dir/inbox/$name" || { rm -f "$dir/inbox/.$3.tmp"; exit 6; }
if sleep 0.1 2>/dev/null; then tick=0.1; max=250; else tick=1; max=25; fi
i=0
while [ "$i" -lt "$max" ]; do
  [ -f "$dir/sent/$name" ] && exit 0
  if [ -f "$dir/exit" ] || ! runner_alive; then
    [ -f "$dir/sent/$name" ] && exit 0
    rm -f "$dir/inbox/$name"
    echo "relay: the agent is not running" >&2
    exit 5
  fi
  sleep "$tick"
  i=$((i+1))
done
[ -f "$dir/sent/$name" ] && exit 0
echo "relay: the runner did not take the message in time" >&2
exit 7
`

// agentReadScript takes $3, the byte offset of a line start. When the runner
// is already gone it prints the rest and ends. Otherwise it follows out.jsonl
// and prints a dot to stderr every few seconds: stdout carries event bytes
// only, so a heartbeat can never land inside a half-written line, while a
// reader orphaned by a dropped link still dies on the failed write.
const agentReadScript = agentPrelude + `
off="$3"
case "$off" in ''|*[!0-9]*) echo "relay: invalid offset" >&2; exit 2;; esac
resolve "$@" || { echo "relay: no agent run for this session" >&2; exit 5; }
[ -f "$dir/out.jsonl" ] || { echo "relay: no agent output in $dir" >&2; exit 5; }
start=$((off+1))
if [ -f "$dir/exit" ] || ! runner_alive; then exec tail -c +"$start" "$dir/out.jsonl"; fi
tail -c +"$start" -f "$dir/out.jsonl" &
tp=$!
trap 'kill "$tp" 2>/dev/null' EXIT
trap 'exit 1' HUP INT TERM PIPE
i=0
while kill -0 "$tp" 2>/dev/null; do
  sleep 1
  i=$((i+1))
  if [ -f "$dir/exit" ]; then sleep 1; exit 0; fi
  if [ $((i % 5)) -eq 0 ]; then
    runner_alive || { sleep 1; exit 0; }
    printf . >&2 || exit 1
  fi
done
`

const agentReadOnceScript = agentPrelude + `
off="$3"
case "$off" in ''|*[!0-9]*) echo "relay: invalid offset" >&2; exit 2;; esac
resolve "$@" || { echo "relay: no agent run for this session" >&2; exit 5; }
[ -f "$dir/out.jsonl" ] || exit 0
exec tail -c +"$((off+1))" "$dir/out.jsonl"
`

// agentStopScript ends the runner's process group, TERM then KILL, and
// returns once the group is gone. Deliberate: it kills only while the leader
// is alive with the start time the state file (runner.json, or run.json of a
// legacy run) recorded. A dead leader, a different
// start time or an exit file means the group is gone or no longer ours, and
// nothing is signalled.
const agentStopScript = agentPrelude + `
resolve "$@" || exit 0
[ ! -f "$dir/exit" ] || exit 0
state_file || exit 0
pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$state")
pgid=$(sed -n 's/.*"pgid":\([0-9][0-9]*\).*/\1/p' "$state")
started=$(sed -n 's/.*"started":"\([^"]*\)".*/\1/p' "$state")
[ -n "$pid" ] && [ "$pid" = "$pgid" ] || exit 0
cur=$(lstart "$pid")
[ -n "$cur" ] && [ "$cur" = "$started" ] || exit 0
kill -0 -- "-$pgid" 2>/dev/null || exit 0
kill -s TERM -- "-$pgid" 2>/dev/null
i=0
while kill -0 -- "-$pgid" 2>/dev/null && [ "$i" -lt 30 ]; do sleep 0.1 2>/dev/null || sleep 1; i=$((i+1)); done
if kill -0 -- "-$pgid" 2>/dev/null; then
  kill -s KILL -- "-$pgid" 2>/dev/null
  i=0
  while kill -0 -- "-$pgid" 2>/dev/null && [ "$i" -lt 30 ]; do sleep 0.1 2>/dev/null || sleep 1; i=$((i+1)); done
fi
if kill -0 -- "-$pgid" 2>/dev/null; then echo "relay: the agent process group did not end" >&2; exit 1; fi
`

const agentStderrScript = agentPrelude + `
resolve "$@" || exit 0
[ -f "$dir/stderr.log" ] || exit 0
exec tail -c 4096 "$dir/stderr.log"
`

const agentRemoveScript = agentPrelude + `
rm -rf -- "$base"
`

func agentCommand(hostOS, script string, args ...string) string {
	argv := append([]string{"sh", "-c", script, "relay-agent"}, args...)
	return RemoteCommandForOS(hostOS, "", argv, nil)
}

// RunnerStart is RunnerStartIn with the agent's working directory left to the
// host's login directory.
func RunnerStart(hostOS string, d RunDir, argv []string, env map[string]string, p runnerfiles.Patterns) string {
	return RunnerStartIn(hostOS, "", d, argv, env, p)
}

// RunnerStartIn creates the run directory, starts argv under dir as a detached
// agent and returns after runner.json exists. It prints "@@RELAY_RUN@@ <generation>".
// An empty d.Generation allocates the next one; a given one is refused if its
// directory exists. env goes to the agent only. p holds the strings the runner
// matches to write event lines.
func RunnerStartIn(hostOS, dir string, d RunDir, argv []string, env map[string]string, p runnerfiles.Patterns) string {
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(dir, argv, env)))
	return agentCommand(hostOS, agentStartScript, d.Session, d.Generation, script, p.TurnEnded, p.Asking)
}

// RunnerSend writes the line on its stdin as inbox/<id>.<kind> and exits 0 only
// once the runner has moved it to sent/. The line travels on stdin so no other
// account sees it in a process listing. Exit 5: no run, or the runner is gone
// (the file is removed). Exit 7: no receipt within 25 s while the runner lives;
// the file stays queued. A caller must not retry on exit 7 or an ambiguous ssh
// exit: the message may still be delivered.
func RunnerSend(hostOS string, d RunDir, id string, kind runnerfiles.MessageKind) string {
	return agentCommand(hostOS, agentSendScript, d.Session, d.Generation, id, string(kind))
}

// RunnerRead follows out.jsonl from a byte offset and ends when the runner is
// gone, after the data has drained. Its stdout is event bytes only.
func RunnerRead(hostOS string, d RunDir, offset int64) string {
	return agentCommand(hostOS, agentReadScript, d.Session, d.Generation, strconv.FormatInt(offset, 10))
}

// RunnerReadOnce prints out.jsonl from a byte offset to its current end and
// returns.
func RunnerReadOnce(hostOS string, d RunDir, offset int64) string {
	return agentCommand(hostOS, agentReadOnceScript, d.Session, d.Generation, strconv.FormatInt(offset, 10))
}

// RunnerStop ends the runner's process group and returns when it is gone.
func RunnerStop(hostOS string, d RunDir) string {
	return agentCommand(hostOS, agentStopScript, d.Session, d.Generation)
}

// RunnerStatus prints "none", "alive <gen>", "exited <gen> <code>" or
// "dead <gen>" or "legacy <gen>" for the generation, the latest when d.Generation is empty.
func RunnerStatus(hostOS string, d RunDir) string {
	return agentCommand(hostOS, agentStatusScript, d.Session, d.Generation)
}

// RunnerStderrTail prints the end of the agent's stderr.
func RunnerStderrTail(hostOS string, d RunDir) string {
	return agentCommand(hostOS, agentStderrScript, d.Session, d.Generation)
}

// RunnerRemove deletes every generation of the session.
func RunnerRemove(hostOS string, session string) string {
	return agentCommand(hostOS, agentRemoveScript, session, "")
}

// ParseRunStart returns the generation RunnerStart printed.
func ParseRunStart(out string) (string, error) {
	f, ok := runSentinelFields(out)
	if !ok || len(f) != 1 {
		return "", fmt.Errorf("unexpected runner start answer %q", firstLine(out))
	}
	return f[0], nil
}

const runSentinel = "@@RELAY_RUN@@"

// runSentinelFields returns the words after the first sentinel line. A host
// whose rc files print to stdout adds lines before it, so the answer is found,
// not assumed to be the whole output.
func runSentinelFields(out string) ([]string, bool) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && f[0] == runSentinel {
			return f[1:], true
		}
	}
	return nil, false
}

// ParseRunState parses RunnerStatus's answer.
func ParseRunState(out string) (RunState, error) {
	f, ok := runSentinelFields(out)
	if !ok || len(f) == 0 {
		return RunState{}, fmt.Errorf("unexpected runner status answer %q", firstLine(out))
	}
	switch RunPhase(f[0]) {
	case RunNone:
		return RunState{Phase: RunNone}, nil
	case RunAlive, RunDead, RunLegacy:
		if len(f) == 2 {
			return RunState{Phase: RunPhase(f[0]), Generation: f[1]}, nil
		}
	case RunExited:
		if len(f) == 3 {
			code, err := strconv.Atoi(f[2])
			if err == nil {
				return RunState{Phase: RunExited, Generation: f[1], Code: code}, nil
			}
		}
	}
	return RunState{}, fmt.Errorf("unexpected runner status answer %q", firstLine(out))
}

// AgentRunStatus asks h, from the console, about the latest generation of
// session's detached run. A transport failure is an error: the caller must
// not read it as "not running".
func AgentRunStatus(ctx context.Context, h config.Host, hostOS, session string) (RunState, error) {
	controlDir, err := ControlDir()
	if err != nil {
		return RunState{}, err
	}
	argv := SSHArgv(h, controlDir)
	argv = append(argv, "-T", "--", RunnerStatus(hostOS, RunDir{Session: session}))
	stdout, stderr, err := runner(ctx, argv[0], argv[1:])
	if err != nil {
		return RunState{}, fmt.Errorf("runner status on host %q: %s", h.Name, sshFailureMessage(err, stderr))
	}
	return ParseRunState(string(stdout))
}
