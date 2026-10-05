# AA-07 · Decode Codex events into canonical events

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-00](AA-00-spike-codex.md), [AA-06](AA-06-kind-registry-hooks.md). Blocks: [AA-08](AA-08-codex-launch.md).

## Story

As a person who uses Codex, I want relay to understand Codex events, so that Codex sessions show the same states as Claude sessions.

## Scope

- A decoder. It turns one Codex JSON line into zero or more canonical events.
- Fixture replay tests. They use the transcripts from [AA-00](AA-00-spike-codex.md).
- A registry entry for the `codex` kind. The entry has no launch code yet.
- A known-good version range.

## Out of scope

- Starting a process. That is [AA-08](AA-08-codex-launch.md).
- Sending a message.

## Acceptance criteria

- Given each fixture, when the decoder runs, then it returns the expected canonical events.
- Given a thread-started event, then the decoder returns an `init` event with the thread id.
- Given a turn-completed event, then the decoder returns a turn-complete event.
- Given a failed turn, then the decoder returns an error event.
- Given an event of an unknown type, then the decoder returns nothing. It reports the type once per session.
- Given a Codex version outside the range, then relay marks the session "unmanaged". The session runs. It reports no state.
- Given the reducer from [AA-02](AA-02-state-reducer.md), when it replays the decoded fixtures, then it reaches the expected states.

## Logging

| Op | Level | Status | Extra keys | When |
|---|---|---|---|---|
| `harness.decode` | warn | error | `session_id`, `harness`, error `unknown_event_type` | A new unknown type. Once per type per session. |
| `harness.version` | warn | error | `harness`, `version`, error `outside_known_good_range` | The probe finds an untested version. |

The lines never hold an event body.

## Journey

None for the decoder. It has no door. [AA-J08](AA-08-codex-launch.md) covers it.

## Security impact

The decoder reads untrusted agent output. It never executes it. It never logs it. Tool arguments stay raw bytes. The decoder never decodes and re-encodes them.

## Docs

- [session-host.md](../../../session-host.md): the Codex kind.
- `docs/epics/agent-attention/codex-findings.md`: link from here.

## Rollback

Remove the registry entry. The fixtures stay.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [x] Valuable: someone gains from it.
- [ ] Estimable: the team can size it. **Flag:** The size depends on the findings of AA-00.
- [x] Small: fits one sprint.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [x] Atomic: asks for one feature.
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
