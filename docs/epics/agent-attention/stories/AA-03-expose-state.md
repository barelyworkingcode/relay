# AA-03 · Expose session state

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-02](AA-02-state-reducer.md). Blocks: [AA-04](AA-04-needs-you-board.md), [AA-10](AA-10-handoff-local.md).

## Story

As a person who runs many agents, I want each session to report its state, so that eve and the Chief of Staff can read it.

## Scope

- The session manager feeds each canonical event to the reducer.
- The manager keeps the current state per session.
- The state appears in the session list.
- A WebSocket frame, `session_state`, announces each change.
- The frame carries the state, the open tool count and the time of the change.
- A `turn_done` frame carries a short excerpt of the last assistant message.

The route and frame names are open. Refinement decides them.

## Out of scope

- The eve screen. That is [AA-04](AA-04-needs-you-board.md).
- A new credential class.

## Acceptance criteria

- Given a running Claude session, when it finishes a turn, then the list shows `idle`.
- Given a state change, when it happens, then every joined viewer gets one `session_state` frame.
- Given a viewer that joins late, when it joins, then it receives the current state.
- Given a stalled session, when the clock passes N seconds, then the state becomes `stalled`. The change needs no new event.
- Given a relay-sessions restart, when a session no longer exists, then the list does not show a state for it.
- Given a `turn_done` frame, then its excerpt holds at most 500 characters.
- Given the HTTP door and the IPC door, then both read the same core.

## Logging

| Op | Level | Status | Extra keys | When |
|---|---|---|---|---|
| `attention.transition` | info | ok | `session_id`, `from_state`, `to_state` | The state changes. |
| `attention.event_ignored` | warn | error | `session_id`, error `unknown_event_type` | The reducer ignores an event type. Once per type per session. |

- `duration_ms` is `0` on a transition line.
- No line is written per event, per tick or per chunk.
- No line holds the excerpt or any message text.

## Journey

**AA-J03.** Preflight: a devbox with Claude. Steps:

1. Launch a session. Read the state. Expect `starting` or `running`.
2. Send a short message. Expect `running`, then `idle`.
3. Read the list. Expect `idle`.
4. Stop the session. Expect `ended`.

Evidence: the `attention.transition` lines show each step under one `session_id`.

## Security impact

The state and the excerpt leave relay-sessions. The excerpt is agent output. It can hold secrets. It travels only to viewers that may already read the session. The viewer check stays the same. The excerpt never enters a log line.

## Docs

- [session-host.md](../../session-host.md): "Session state", the frame, the list field.
- [FEATURES.md](../../FEATURES.md): a G1 row for session state, with journey AA-J03.
- [logging-standard.md](../../logging-standard.md): no change. The ops use the standard keys.

## Rollback

Remove the frame and the list field. Viewers ignore unknown fields.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [x] Valuable: someone gains from it.
- [x] Estimable: the team can size it.
- [ ] Small: fits one sprint. **Flag:** Follows from the atomic flag.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [ ] Atomic: asks for one feature. **Flag:** Three items: list field, WS frame and turn excerpt. The excerpt may become its own story.
- [x] Minimal: holds only role, means and end.
- [x] Conceptually sound: the means is a feature and the end is a reason.
- [x] Problem-oriented: states the problem, not the solution.
- [x] Unambiguous: has one reading.
- [x] Conflict-free: does not clash with another story.
- [x] Full sentence: reads as a full sentence.
- [x] Estimatable: the team can size it.
- [x] Unique: no other story repeats it.
- [x] Uniform: follows the same format as its siblings.
- [x] Independent: needs few other stories.
- [x] Complete: no step is missing from the set.

Unique, uniform, independent, conflict-free and complete are judged across the set. The epic README records that review.

### Definition of ready

This story has acceptance criteria, a journey (or the reason for none), log lines, a security note, a docs list, linked dependencies and a rollback.
