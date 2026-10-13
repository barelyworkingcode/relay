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
- **Outside sessions: a drain can drop hook reports.** A drain deletes spool
  files as it reads them, then runs a probe bounded at 3 s. If that probe runs
  past the bound, the drained reports are lost and the state waits for the
  next 15 s probe, still inside the 60 s bound.
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
