package sshhost

import (
	"encoding/base64"
	"sort"
	"strings"

	runnerfiles "github.com/barelyworkingcode/relay/internal/sessions/runner"
)

// A detached host agent runs as the pane of a tmux session named for its
// relay session, so the owner finds it on the host by that name whether it
// runs headless or dropped in (docs/ssh-hosts.md, "Detached sessions").

// AgentPanePrefix starts every agent pane's tmux session name. It never
// parses as a persist name (config.ParsePersistSessionName needs a "-" after
// 8 characters), so the persistent-session list and kill never touch one.
const AgentPanePrefix = "relay-agent-"

// AgentPaneName is the tmux session name of a relay session's agent.
func AgentPaneName(sessionID string) string { return AgentPanePrefix + sessionID }

// agentPaneLaunch is the launch step of the pane form. $6 is the tmux path and
// $7 the session name. Deliberate: tmux gets the runner as an argv, never a
// shell string, so the pane's process is the runner itself: runner.json's pid
// is #{pane_pid} and leads its own process group. Below tmux 3.0, or after any
// failed tmux call, the runner starts alone and one stderr line names the
// version.
const agentPaneLaunch = `
pane_fallback() {
  reason=$1; shift
  echo "relay: tmux ${tmux_version:-unknown} cannot host the agent pane ($reason); the agent runs without one" >&2
  launch_plain "$@"
}
pane_dead() { "$tmux" display-message -p -t "=$pane:" '#{pane_dead}' 2>/dev/null; }
pane_wait_dead() {
  w=0
  while [ "$(pane_dead)" != 1 ] && [ "$w" -lt 30 ]; do sleep 0.1 2>/dev/null || sleep 1; w=$((w+1)); done
  [ "$(pane_dead)" = 1 ]
}
pane_launch() {
  tmux=$6; pane=$7
  if [ -z "$tmux" ] || [ -z "$pane" ]; then launch_plain "$@"; return; fi
  tmux_version=$("$tmux" -V 2>/dev/null | sed 's/^[^0-9]*//')
  major=${tmux_version%%[!0-9]*}
  if [ -z "$major" ] || [ "$major" -lt 3 ]; then pane_fallback "tmux 3.0 or later is required" "$@"; return; fi
  if "$tmux" has-session -t "=$pane" 2>/dev/null; then
    if [ "$(pane_dead)" != 1 ]; then
      pgid=$("$tmux" display-message -p -t "=$pane:" '#{pane_pid}' 2>/dev/null)
      case "$pgid" in ''|*[!0-9]*) pane_fallback "the pane has no process id" "$@"; return;; esac
      kill -s TERM -- "-$pgid" 2>/dev/null
      pane_wait_dead || { kill -s KILL -- "-$pgid" 2>/dev/null; pane_wait_dead; } || { pane_fallback "the pane did not end" "$@"; return; }
    fi
    "$tmux" respawn-pane -t "=$pane:" sh "$dir/runner.sh" "$dir" "$oldumask" "$1" "$gen" "$4" "$5" 2>/dev/null \
      || { pane_fallback "respawn-pane failed" "$@"; return; }
  else
    "$tmux" new-session -d -s "$pane" -x 200 -y 50 sh "$dir/runner.sh" "$dir" "$oldumask" "$1" "$gen" "$4" "$5" \; \
      set-option -w -t "=$pane:" remain-on-exit on 2>/dev/null \
      || "$tmux" has-session -t "=$pane" 2>/dev/null \
      || { pane_fallback "new-session failed" "$@"; return; }
  fi
}
`

var agentPaneStartScript = agentStartHead + agentPaneLaunch + `pane_launch "$@"
` + agentStartWait

// RunnerStartInPane is RunnerStartIn with the runner as the pane of the tmux
// session pane. It prints "@@RELAY_RUN@@ <generation>" once runner.json exists.
func RunnerStartInPane(hostOS, dir string, d RunDir, argv []string, env map[string]string, p runnerfiles.Patterns, tmuxPath, pane string) string {
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(dir, argv, env)))
	return agentCommand(hostOS, agentPaneStartScript, d.Session, d.Generation, script, p.TurnEnded, p.Asking, tmuxPath, pane)
}

// PaneDropInArgv is the remote argv of a drop-in terminal on a host with
// tmux. With the session present it respawns the dead pane with the UI,
// detaches every client when the UI exits, and attaches; without it, it runs
// the UI as a drop-in with no tmux does. env reaches the pane through -e,
// because a pane takes the tmux server's environment, not the client's.
func PaneDropInArgv(tmuxPath, pane, dir string, ui []string, env map[string]string) []string {
	target := "=" + pane
	var uiArgs strings.Builder
	for _, a := range ui {
		uiArgs.WriteString(" " + shQuote(a))
	}
	// tmux expands a -c value as a format, so a directory holding "#P" would
	// start the UI elsewhere. The shell in the pane changes directory instead.
	inner := `"$@"; ` + shQuote(tmuxPath) + " detach-client -s " + shQuote(target)
	if dir != "" {
		inner = `if cd "$1"; then shift; "$@"; else echo "relay: cannot enter $1" >&2; fi; ` +
			shQuote(tmuxPath) + " detach-client -s " + shQuote(target)
	}

	var respawn strings.Builder
	respawn.WriteString(shQuote(tmuxPath) + " respawn-pane -t " + shQuote(target+":"))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		respawn.WriteString(" -e " + shQuote(k+"="+env[k]))
	}
	respawn.WriteString(" sh -c " + shQuote(inner) + " relay-ui")
	if dir != "" {
		respawn.WriteString(" " + shQuote(dir))
	}
	respawn.WriteString(uiArgs.String())

	script := ""
	if dir != "" {
		script = "cd " + shQuote(dir) + " || exit 1\n"
	}
	script += "if " + shQuote(tmuxPath) + " has-session -t " + shQuote(target) + " 2>/dev/null; then\n" +
		"  " + respawn.String() + " || exit 1\n" +
		"  exec " + shQuote(tmuxPath) + " attach-session -t " + shQuote(target) + "\n" +
		"fi\n" +
		"exec" + uiArgs.String() + "\n"
	return []string{"sh", "-c", script, "relay-pane"}
}

// agentKillPaneScript takes $1 the tmux path and $2 the session name. Only a
// session that exists is killed, so a missing one, or no server, exits 0.
const agentKillPaneScript = `"$1" has-session -t "=$2" 2>/dev/null || exit 0
exec "$1" kill-session -t "=$2"
`

// KillAgentPane ends the tmux session of a deleted agent; a missing session
// exits 0.
func KillAgentPane(hostOS, tmuxPath, pane string) string {
	return RemoteCommandForOS(hostOS, "", []string{"sh", "-c", agentKillPaneScript, "relay-agent", tmuxPath, pane}, nil)
}
