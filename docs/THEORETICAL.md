# Theoretical findings

Findings from reading code or review that did not reproduce in real use.
Each is promoted to an issue the day real use shows it.

| Area | Finding | Why it stays theoretical |
|---|---|---|
| Outside sessions | A report drain deletes spool files as it reads them, then runs a probe bounded at 3 s. If that probe runs past the bound, the drained reports are lost and the hook state waits for the next probe. | Not reproduced; a slow host still converges at the next 15 s probe, inside the 60 s bound. |
