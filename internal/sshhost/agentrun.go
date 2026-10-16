package sshhost

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/internal/config"
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
runner_alive() {
  [ -f "$dir/run.json" ] || return 1
  pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$dir/run.json")
  started=$(sed -n 's/.*"started":"\([^"]*\)".*/\1/p' "$dir/run.json")
  [ -n "$pid" ] && [ -n "$started" ] || return 1
  [ "$(lstart "$pid")" = "$started" ]
}
`

// agentRunnerScript is the long-lived runner. It leads its own process group
// (the start script arranges that and the runner refuses to run otherwise),
// ignores HUP, feeds the agent's stdin from in.jsonl through a fifo so it
// owns the reader's pid, and writes exit when the agent ends.
const agentRunnerScript = `trap '' HUP
dir=$1
cd "$dir" || exit 1
lstart() { LC_ALL=C ps -o lstart= -p "$1" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ //;s/ $//'; }
pid=$$
pgid=$(ps -o pgid= -p "$pid" | tr -d ' ')
if [ "$pgid" != "$pid" ]; then : > norun; exit 1; fi
started=$(lstart "$pid")
printf '{"pid":%s,"pgid":%s,"started":"%s","generation":"%s"}\n' "$pid" "$pgid" "$started" "${dir##*/}" > run.json.tmp && mv run.json.tmp run.json
tail -n +1 -f in.jsonl > in.pipe &
tailpid=$!
umask "$2"
sh agent.sh < in.pipe >> out.jsonl 2>> stderr.log
code=$?
umask 077
kill "$tailpid" 2>/dev/null
printf '{"code":%s,"at":"%s"}\n' "$code" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > exit.tmp && mv exit.tmp exit
`

// agentStartScript takes $1 session, $2 generation (empty: allocate the next)
// and $3 the base64 of the agent's own launch script. It returns only after
// run.json exists, which is the completion handle a caller waits on.
const agentStartScript = agentPrelude + `
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
: > in.jsonl; : > out.jsonl; : > stderr.log
mkfifo in.pipe || { echo "relay: cannot create input pipe in $dir" >&2; exit 3; }
printf %s "$3" | base64 -d > agent.sh || { echo "relay: cannot write agent script in $dir" >&2; exit 3; }
cat > runner.sh <<'RELAY_RUNNER_EOF'
` + agentRunnerScript + `RELAY_RUNNER_EOF
if command -v setsid >/dev/null 2>&1; then
  setsid sh "$dir/runner.sh" "$dir" "$oldumask" </dev/null >/dev/null 2>&1 &
else
  set -m
  sh "$dir/runner.sh" "$dir" "$oldumask" </dev/null >/dev/null 2>&1 &
  set +m
fi
rpid=$!
i=0
while [ ! -f "$dir/run.json" ]; do
  i=$((i+1))
  if [ -f "$dir/norun" ] || [ "$i" -gt 100 ]; then
    kill "$rpid" 2>/dev/null
    echo "relay: agent runner did not start in $dir" >&2
    exit 4
  fi
  sleep 0.1 2>/dev/null || sleep 1
done
echo "@@RELAY_RUN@@ $gen"
`

const agentStatusScript = agentPrelude + `
resolve "$@" || { echo "@@RELAY_RUN@@ none"; exit 0; }
if [ -f "$dir/exit" ]; then
  code=$(sed -n 's/.*"code":\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$dir/exit")
  echo "@@RELAY_RUN@@ exited $gen ${code:-1}"
  exit 0
fi
if runner_alive; then echo "@@RELAY_RUN@@ alive $gen"; else echo "@@RELAY_RUN@@ dead $gen"; fi
`

// agentSendBody appends one line from stdin to in.jsonl. The refusal when the
// runner is gone is deliberate: an append to a file nobody reads must not
// report delivery.
const agentSendBody = `resolve "$@" || { echo "relay: no agent run for this session" >&2; exit 5; }
if [ -f "$dir/exit" ] || ! runner_alive; then echo "relay: the agent is not running" >&2; exit 5; fi
line=$(cat)
printf '%s\n' "$line" >> "$dir/in.jsonl" || exit 6
`

const agentSendScript = agentPrelude + agentSendBody

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
// is alive with the start time run.json recorded. A dead leader, a different
// start time or an exit file means the group is gone or no longer ours, and
// nothing is signalled.
const agentStopScript = agentPrelude + `
resolve "$@" || exit 0
[ ! -f "$dir/exit" ] || exit 0
[ -f "$dir/run.json" ] || exit 0
pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$dir/run.json")
pgid=$(sed -n 's/.*"pgid":\([0-9][0-9]*\).*/\1/p' "$dir/run.json")
started=$(sed -n 's/.*"started":"\([^"]*\)".*/\1/p' "$dir/run.json")
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
func RunnerStart(hostOS string, d RunDir, argv []string, env map[string]string) string {
	return RunnerStartIn(hostOS, "", d, argv, env)
}

// RunnerStartIn creates the run directory, starts argv under dir as a detached
// agent and returns after run.json exists. It prints "@@RELAY_RUN@@ <generation>".
// An empty d.Generation allocates the next one; a given one is refused if its
// directory exists. env goes to the agent only.
func RunnerStartIn(hostOS, dir string, d RunDir, argv []string, env map[string]string) string {
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(dir, argv, env)))
	return agentCommand(hostOS, agentStartScript, d.Session, d.Generation, script)
}

// RunnerSend appends the line on its stdin as one complete line to in.jsonl.
// The line travels on stdin so no other account sees it in a process listing.
// It fails, and so reports no delivery, when the append fails or the agent is
// gone. A caller must not retry on an ambiguous ssh exit: the append may have
// landed.
func RunnerSend(hostOS string, d RunDir) string {
	return agentCommand(hostOS, agentSendScript, d.Session, d.Generation)
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
// "dead <gen>" for the generation, the latest when d.Generation is empty.
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
	case RunAlive, RunDead:
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
