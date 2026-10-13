# Theoretical findings

Findings from reading code or review that real use has not shown. Each is
promoted to an issue the day it does.

- **Outside viewer: owner resizes, then detaches within one poll.** The
  viewer rereads the window size every 3 s. If the owner resizes and detaches
  inside that gap, and the viewer is then the only client, the window can
  stay at the old size. The viewer still sets nothing on the session; tmux
  sizes a window from its non-`ignore-size` clients.
