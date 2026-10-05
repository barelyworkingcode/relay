# AA-09 · Run Codex on an SSH host

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-08](AA-08-codex-launch.md).

## Story

As a person who works on a remote machine, I want to run Codex there, so that the work stays on that machine.

## Scope

- The host probe finds the `codex` binary and its version.
- The provider starts Codex over `ssh -T` with the host launcher.
- Host sessions use the same decoder.
- A host template for Codex.

The design follows [ssh-hosts.md](../../../ssh-hosts.md).

## Out of scope

- Reconnect after a dropped link.
- Drop-in on a host. That is [AA-11](AA-11-handoff-host.md), for Claude.

## Acceptance criteria

- Given a host with Codex, when the probe runs, then the host record holds the path and the version.
- Given a host without Codex, when the user launches, then relay refuses with a message that says to run a probe.
- Given a host session, then the child gets no relay credential and no hook socket.
- Given a host session, then the state reducer reports the same states as a local session.
- Given a dropped link, then the state becomes `ended`.
- Given a host project, then the Codex template is the host template, not a console template.

## Logging

Existing ops, with one more key.

| Op | Level | Status | Extra keys |
|---|---|---|---|
| `session.launch` | info | ok | `session_id`, `harness`, `host_id` |
| `host.probe` | info | ok | `host_id`, `harness` |

## Journey

**AA-J09.** Preflight: a devbox host with Codex. Steps:

1. Probe the host.
2. Launch a Codex session in a host project.
3. Send one message. Expect `idle` after the reply.

## Security impact

A host template never sandboxes. Yolo mode on a host means an unconfined agent on that machine. The machine boundary is the only limit. The owner accepts this in D1. The host project needs an explicit opt-in for unattended runs.

## Docs

- [ssh-hosts.md](../../../ssh-hosts.md): the Codex section.
- [session-host.md](../../../session-host.md): the host template lines.

## Rollback

Remove the host template. Local Codex keeps working.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [ ] Independent: needs few other stories. **Flag:** The host design must confirm that Codex fits the host launcher.
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
- [x] Unambiguous: has one reading.
- [x] Conflict-free: does not clash with another story.
- [x] Full sentence: reads as a full sentence.
- [x] Estimatable: the team can size it.
- [x] Unique: no other story repeats it.
- [x] Uniform: follows the same format as its siblings.
- [ ] Independent: needs few other stories. **Flag:** The host design must confirm that Codex fits the host launcher.
- [x] Complete: no step is missing from the set.

Unique, uniform, independent, conflict-free and complete are judged across the set. The epic README records that review.

### Definition of ready

This story has acceptance criteria, a journey (or the reason for none), log lines, a security note, a docs list, linked dependencies and a rollback.
