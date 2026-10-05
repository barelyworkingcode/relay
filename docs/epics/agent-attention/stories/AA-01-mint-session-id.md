# AA-01 · Mint the Claude session id before launch

**Status: Needs review.** Epic: [E1](../README.md). Blocks: [AA-10](AA-10-handoff-local.md).

## Story

As a person who runs many agents, I want relay to choose the session id before Claude starts, so that every event and every drop-in uses one known id.

## Background

Today relay reads the id from the first `init` event. Then relay can use `--resume`. Before that event, relay has no id for the harness.

Claude accepts `--session-id`. The owner confirmed this on a local machine.

## Scope

- Relay generates a UUID at launch.
- Relay passes it with `--session-id`.
- Relay stores it with the session.
- Resume uses the stored id.

## Out of scope

- Codex and pi ids.
- Any UI change.

## Acceptance criteria

- Given a new Claude session, when relay starts `claude`, then the argv holds `--session-id <uuid>`.
- Given the first `init` event, when it arrives, then its id equals the minted id.
- Given a stored session, when relay resumes it, then the argv holds `--resume <same uuid>`.
- Given an id that is already in use, when relay launches, then relay refuses with `session_exists`.
- Given a session saved before this story, when relay resumes it, then relay uses the id it saved.
- Given the `init` event holds a different id, when it arrives, then relay logs a warning and keeps the minted id.

## Logging

| Op | Level | Status | Extra keys | When |
|---|---|---|---|---|
| `session.launch` | info | ok | `session_id`, `harness_session_id` | The session starts. |
| `session.launch` | warn | error | `session_id`, error `session_exists` | The id is in use. |
| `session.launch` | warn | error | `session_id`, error `harness_id_mismatch` | The `init` id differs. |

The lines never hold a prompt.

## Journey

**AA-J01.** Preflight: a devbox with Claude. Steps:

1. Launch a Claude session through relay.
2. Read the session record. Read the harness id.
3. Send one message. Read the `init` event.
4. Stop the session. Resume it.

Evidence: the id in the record, the `init` event and the resume argv are equal. The `session.launch` line holds both ids.

## Security impact

The id is not a secret. A random UUID does not leak anything. The id appears in argv. Relay does not treat it as a credential.

## Docs

- [session-host.md](../../../session-host.md): the launch section.
- [FEATURES.md](../../../FEATURES.md): the G1 session rows, if they name resume.

## Rollback

Remove the flag. Relay returns to reading the id from `init`. Stored ids stay valid.

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
- [ ] Problem-oriented: states the problem, not the solution. **Flag:** The means names a mechanism: relay chooses the id. A reviewer may prefer 'every event has one known id'.
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
