# Theoretical findings

Findings from reading code or review that real use has not shown. Each is
promoted to an issue the day it does.

- **Outside viewer: owner resizes, then detaches within one poll.** The
  viewer rereads the window size every 3 s. If the owner resizes and detaches
  inside that gap, and the viewer is then the only client, the window can
  stay at the old size. The viewer still sets nothing on the session; tmux
  sizes a window from its non-`ignore-size` clients.
- **Outside viewer: a join that lands as the 30 s join wait fires.** The
  new viewer is closed early. It never writes to the owner's session, and a
  frontend joins right after the create call returns.
- **Console confirm under an inherited `TMUX`.** The confirm step runs in
  relay's environment while the attach and the size poll run with `TMUX=`.
  If relay were started inside tmux on a non-default socket, the two could
  talk to different servers.
- **Login banners on a host's stdout.** The version and size parsers expect
  clean output, so a banner would turn a supported host into a 409.
- **Relay launched without Homebrew on PATH.** Console viewing finds tmux
  with `PATH` only, so every console view would answer 409.
- **No per-caller cap on viewers.** A `read` credential can start many
  viewers. Each one closes itself after 30 s if nobody joins it.
- **Outside sessions: a failed host path resolution drops symlinked rows.**
  Host project paths are resolved in a second SSH call per probe. If only that
  call fails, rows under a symlinked project path are removed until the next
  good probe, instead of being kept 60 s as for an unreachable machine.
- **Outside sessions: resolved paths are matched by line position.** Stdout
  from a host's shell startup files, or a resolved path containing a newline,
  would shift the lines; a row could lose or take a neighbour's `projectId`,
  never one outside the union of the host's projects.
- **Outside sessions: console path resolution has no deadline.** Resolving a
  console project on a hung network mount would block that console probe
  past its 10 s bound.
- **Outside sessions: an unreachable machine's rows once read 60.05 s.** A
  superseded test measured the removal after a debug-clock round trip. The
  code removes them 58 s after the last good probe, checked every 2 s.
- **Outside view: a new pane's folder and the owner's layout.** A superseded
  test could not pin a new pane's working folder, and its layout-corruption
  injection never produced a marker. Neither showed a defect.
- **Outside sessions: a probe can outlive its machine's removal.** A probe
  that passes its context check just before the host is removed writes the
  host's rows back, and nothing expires them.
- **Outside sessions: a host clock more than 1 s ahead.** Report times are
  the host's file mtimes, so every hook report is dropped and rows update at
  the 15 s probe instead.
- **Outside sessions: two probes can finish out of order.** A drain's probe
  and the baseline probe may overlap, and an exited row can return for one
  interval.
- **Outside sessions: npm Claude started with no arguments.** `node .../claude`
  with nothing after it does not match the probe's pattern; the native binary
  does.
- **Sandbox Claude trust seed: the launch folder swapped between checks.**
  The seed resolves the launch folder again after the launch path confined
  it. A folder swapped for a link to an outside path in that gap would be
  trusted. A sandboxed Claude can already write any trust entry into that
  folder's `.claude.json` itself.
- **Sandbox Claude trust seed: a home project in a different letter case.**
  The home check compares resolved strings, so `/users/<name>` is not seen as
  home. The `CLAUDE_CODE_SANDBOXED` gate then trusts it outright; the seed
  trusts it only if Claude matches trust keys ignoring case. The operator has
  to register home under that spelling, and the profile still confines it.
- **Sandbox Claude trust seed: a launch that fails after the seed.** The seed
  is written before the spawn, so a launch that then fails uses the folder's
  one seed.
- **Adopt: a recycled pid between the exit watch and the signal.** The ladder
  arms its exit watch, then signals. If the Claude process exits and the kernel
  hands its pid to another process in that gap, the signal could reach the new
  one. The pid's start time is re-read before each signal and the watch checks
  it again at registration; the window is microseconds.
- **Adopt: a session adopted before its first prompt.** Such a session has no
  transcript file. The door answers 201 and the resume finds nothing to
  continue, so the resumed session starts empty. Nothing is lost.
- **Adopt: a sandboxed agent that can write Claude's session registry.** An
  agent with write access to `~/.claude/sessions` could rewrite `sessionId`,
  `kind` or `entrypoint` in an entry. The checks still bound the effect to a
  real, live outside Claude process whose kernel start time matches the entry,
  in a registered project folder, so the worst case is adopting a session the
  owner could already adopt.
- **Adopt: a background shell that execs caffeinate.** The turn-helper
  exclusion matches the child's exec path only. A background Bash command that
  runs `exec /usr/bin/caffeinate -i <job>` leaves a direct child that reads as
  the helper while its job runs, so that child never vetoes an adopt. It needs
  an explicit `exec` in a background command.
- **Adopt: a bash-mode child before the last turn's first assistant record.**
  The child veto's window opens at the first `assistant` record after the last
  prompt of the run. A child started by `!` bash mode, which writes only `user`
  records, before that record would not veto; the registry status likely reads busy then.
