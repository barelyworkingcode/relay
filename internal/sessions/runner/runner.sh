# The runner owns an agent's stdin and stdout and moves lines. It knows no
# protocol: it matches two fixed strings, given at start, in each stdout line.
# Arguments: run directory, the umask the agent runs under, relay session id,
# generation, turn-ended string, asking string. POSIX sh plus awk, tail, mkfifo,
# ps and base64; no tmux, perl, python or setsid(1).
trap '' HUP
dir=$1; agent_umask=$2; session=$3; gen=$4; turn_ended=$5; asking=$6
cd "$dir" || exit 1
LC_ALL=C; export LC_ALL

pid=$$
pgid=$(ps -o pgid= -p "$pid" | tr -d ' ')
if [ "$pgid" != "$pid" ]; then : > norun; exit 1; fi
started=$(ps -o lstart= -p "$pid" 2>/dev/null | tr -s ' ' ' ' | sed 's/^ //;s/ $//')

write_state() {
  printf '{"v":1,"pid":%s,"pgid":%s,"started":"%s","session":"%s","generation":"%s","delivered":"%s"}\n' \
    "$pid" "$pgid" "$started" "$session" "$gen" "$1" > runner.json.tmp && mv runner.json.tmp runner.json
}

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# event TYPE OUT [EXTRA]: EXTRA is a rendered `,"key":value` tail.
event() {
  printf '{"v":1,"type":"%s","session":"%s","generation":"%s","out":%s%s,"at":"%s"}\n' \
    "$1" "$session" "$gen" "$2" "$3" "$(now)" >> events.jsonl
}

out_size() { wc -c < out.jsonl | tr -d ' '; }

mkfifo in.pipe || { : > norun; exit 1; }
write_state ""

# The writer holds the agent's stdin open and delivers inbox files in name
# order. Deliberate: the move to sent/ follows the write and is the receipt, and
# turn_started precedes both, so a reader that saw the reply finds its event.
(
  exec 3> in.pipe
  delivered=""
  while [ ! -f agent.code ]; do
    for f in inbox/*; do
      [ -f "$f" ] || continue
      name=${f##*/}
      case "$name" in
        .*) continue;;
        *.json) kind=json;;
        *.ctl) kind=ctl;;
        *) continue;;
      esac
      id=${name%.*}
      [ "$kind" = json ] && event turn_started "$(out_size)" ",\"msg\":\"$id\""
      cat "$f" >&3 || exit 1
      [ -z "$(tail -c 1 "$f")" ] || printf '\n' >&3
      mv "$f" "sent/$name" || exit 1
      delivered=$id
      write_state "$delivered"
    done
    sleep 0.1 2>/dev/null || sleep 1
  done
) &
writer=$!

# Deliberate: a trap with a command is reset by exec, so the agent still gets
# the group's TERM; the runner survives it to record the agent's code.
trap 'termed=1' TERM

# awk ignores TERM (an ignored signal survives exec) so it drains the agent's
# last lines after a stop. Each event is appended and closed before its line is
# printed, which is what puts the event first.
(
  umask "$agent_umask"
  trap 'true' TERM
  sh agent.sh < in.pipe 2>> stderr.log
  c=$?
  umask 077
  echo "$c" > agent.code.tmp && mv agent.code.tmp agent.code
) | (
  trap '' TERM
  RELAY_SESSION=$session RELAY_GEN=$gen RELAY_TURN_ENDED=$turn_ended RELAY_ASKING=$asking exec awk '
    function event(type, o,    cmd, ts) {
      cmd = "date -u +%Y-%m-%dT%H:%M:%SZ"
      cmd | getline ts
      close(cmd)
      printf "{\"v\":1,\"type\":\"%s\",\"session\":\"%s\",\"generation\":\"%s\",\"out\":%.0f,\"at\":\"%s\"}\n", type, S, G, o, ts >> "events.jsonl"
      close("events.jsonl")
    }
    BEGIN { S = ENVIRON["RELAY_SESSION"]; G = ENVIRON["RELAY_GEN"]; T = ENVIRON["RELAY_TURN_ENDED"]; A = ENVIRON["RELAY_ASKING"]; off = 0 }
    {
      if (T != "" && index($0, T) > 0) event("turn_ended", off)
      if (A != "" && index($0, A) > 0) event("asking", off)
      print $0
      fflush()
      off += length($0) + 1
    }' >> out.jsonl
)

kill "$writer" 2>/dev/null
code=$(cat agent.code 2>/dev/null)
[ -n "$code" ] || code=1
event exited "$(out_size)" ",\"code\":$code"
printf '{"code":%s,"at":"%s"}\n' "$code" "$(now)" > exit.tmp && mv exit.tmp exit
