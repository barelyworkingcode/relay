# AA-08 · Launch and drive Codex on this machine

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-07](AA-07-codex-decode.md). Blocks: [AA-09](AA-09-codex-host.md).

## Story

As a person who uses Codex, I want to start a Codex session in relay and talk to it, so that I use Codex and Claude from one place.

## Scope

- A provider that starts `codex app-server` through the shim.
- Send a message. Receive events through the decoder.
- Interrupt a running turn.
- Yolo mode for unattended runs (decision D1).
- Resume a thread.

## Out of scope

- SSH hosts. That is [AA-09](AA-09-codex-host.md).
- Approvals. They stay deferred.

## Acceptance criteria

- Given a project that allows the Codex template, when the user launches Codex, then a session starts in the project folder.
- Given a running session, when the user sends a message, then the reply appears as canonical events.
- Given a running turn, when the user interrupts, then the turn stops. The session stays alive.
- Given a stored thread, when relay resumes it, then the conversation continues.
- Given the sandbox, then the Codex process runs under relay's profile.
- Given a project that does not allow the template, then relay refuses with `template_not_allowed`.
- Given the child environment, then it holds no relay credential.
- Given a mid-turn crash, then the state becomes `ended` and the last error shows.

## Logging

| Op | Level | Status | Extra keys | When |
|---|---|---|---|---|
| `session.launch` | info | ok | `session_id`, `harness`, `harness_session_id` | The session starts. |
| `session.launch` | warn | error | `session_id`, `harness`, error `spawn_failed` | The process does not start. |
| `session.send` | info | ok | `session_id`, `trace_id` | A message is accepted. |
| `session.interrupt` | info | ok | `session_id` | An interrupt is accepted. |

The lines never hold message text.

## Journey

**AA-J08.** Preflight: a devbox with Codex. Steps:

1. Launch a Codex session.
2. Send one message.
3. Expect the state to go `running`, then `idle`.
4. Interrupt a long turn. Expect `idle`.
5. Stop and resume. Expect the same conversation.

Evidence: `session.launch`, `session.send` and `attention.transition` lines under one `session_id`.

## Security impact

Yolo mode removes the harness approvals. The relay sandbox stays. On a local machine the profile limits file access. The profile does not limit the network. This risk is accepted in D1.

The environment carries no relay credential. Secrets travel on fd 3, as for other kinds.

## Docs

- [session-host.md](../../../session-host.md): the Codex kind, the template, the folders it needs.
- [FEATURES.md](../../../FEATURES.md): the G1 rows.
- The `codex` template: a seeded entry in `terminal_templates`.

## Rollback

Remove the registry entry. Codex falls back to a plain terminal template.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [x] Valuable: someone gains from it.
- [x] Estimable: the team can size it.
- [ ] Small: fits one sprint. **Flag:** Launch, send, interrupt and resume. Resume may split out.
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
