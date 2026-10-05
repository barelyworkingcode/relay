# AA-04 · Show a "needs you" board in eve

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-03](AA-03-expose-state.md).

## Story

As a person who runs many agents, I want one board that lists the sessions that need me, so that I stop checking each session by hand.

## Scope

- An eve view that reads session state.
- Sessions in `asking`, `idle`, `errored` and `stalled` sort first.
- Each row shows the project, the session name, the state and the time in that state.
- A row opens the session.
- The board extends the existing agent board in eve.

Eve repository: [public/agent-board.js](https://github.com/barelyworkingcode/eve/blob/main/public/agent-board.js). Design note: [design-workbench.md](https://github.com/barelyworkingcode/eve/blob/main/docs/design-workbench.md).

## Out of scope

- An LLM summary. That is [E2](../../chief-of-staff/README.md).
- Push notifications.

## Acceptance criteria

- Given a session that finishes a turn, when the frame arrives, then its row moves to the top group within one second.
- Given a `running` session, then it sits in the lower group.
- Given an `ended` session, then it leaves the board after the user dismisses it.
- Given a reload, then the board shows the current states. It does not wait for a new event.
- Given a lost relay connection, then the board shows "state unknown". It does not show `idle`.
- Given a row, when the user selects it, then eve opens that session.

## Logging

Eve logs through its own logger. The line follows the logging standard.

| Op | Level | Status | When |
|---|---|---|---|
| `attention.board.load` | info | ok or error | The board loads the list. |

The line holds counts and ids. It never holds an excerpt.

## Journey

**AA-J04.** An eve journey on the devbox. Steps:

1. Launch a session. Send a short message.
2. Open the board.
3. Wait for the turn to end.
4. Expect the session in the top group with state `idle`.

Evidence: a screenshot assertion on the row, and the `attention.transition` line in relay-sessions.

## Patch rules (eve)

This story adds no new call to relay. It reads an existing frame and an existing list. If refinement adds a call, the PR also adds a fake route in `test/integration/fake-relay.js` and a pin in `relay-source-pins.test.js`.

## Security impact

The board shows agent output excerpts. They stay in the page. The page already shows the same text in the session view. No new origin or credential.

## Docs

- Eve [FEATURES.md](https://github.com/barelyworkingcode/eve/blob/main/docs/FEATURES.md): the board row.
- [FEATURES.md](../../../FEATURES.md) in relay: G1 row.

## Rollback

Remove the board view. Relay keeps sending the frame.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [x] Valuable: someone gains from it.
- [x] Estimable: the team can size it.
- [x] Small: fits one sprint.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [x] Atomic: asks for one feature.
- [x] Minimal: holds only role, means and end.
- [x] Conceptually sound: the means is a feature and the end is a reason.
- [x] Problem-oriented: states the problem, not the solution.
- [ ] Unambiguous: has one reading. **Flag:** 'Needs you' includes `idle`. `idle` can mean done or waiting. E2 decides (decision D4).
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
