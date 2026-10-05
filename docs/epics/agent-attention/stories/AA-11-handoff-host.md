# AA-11 · Drop in to a Claude session on an SSH host

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-10](AA-10-handoff-local.md).

## Story

As a person who runs agents on a remote machine, I want to take over one session in a terminal there, so that I work with it live and the work survives a dropped link.

## Background

A host session already streams over `ssh -T`. A host terminal can persist through tmux. See [ssh-hosts.md](../../../ssh-hosts.md).

## Scope

- The drop-in action from [AA-10](AA-10-handoff-local.md) works on a host project.
- The terminal starts in a persistent tmux session on the host.
- The resume command runs inside tmux.

## Out of scope

- Return to headless.
- Reconnect of a headless stream.

## Acceptance criteria

- Given a host session in `idle`, when the user drops in, then a persistent terminal opens with the same conversation.
- Given a dropped link, when the user reconnects, then the terminal is still alive.
- Given a host without tmux, then relay refuses with `tmux_unavailable`.
- Given the state is stale because of a slow link, then relay refuses with `state_unknown`.
- Given a host template, then it never gets a sandbox claim.
- Given a handoff, then the remote command uses relay's launcher and quotes every argument.

## Logging

Same op as AA-10, with one more key.

| Op | Level | Status | Extra keys |
|---|---|---|---|
| `session.handoff` | info, warn, error | ok, denied, error | `session_id`, `host_id`, error `tmux_unavailable` when relevant |

## Journey

**AA-J11.** Preflight: a devbox host with Claude and tmux. Steps:

1. Launch a headless host session. Set a fact.
2. Wait for `idle`. Drop in.
3. Drop the SSH link. Reconnect.
4. Expect the terminal alive. Ask for the fact.

## Security impact

A host has no relay sandbox. The terminal carries the same limits as the headless host session, which are none. The handoff does not widen them. The launcher quoting is the main risk. A fixture test pins it.

## Docs

- [ssh-hosts.md](../../../ssh-hosts.md): "Drop-in".
- [FEATURES.md](../../../FEATURES.md): the G11 row.

## Rollback

Remove host support from the action. Local drop-in stays.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [ ] Independent: needs few other stories. **Flag:** The tmux path for a Claude headless stream needs confirmation.
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
- [ ] Independent: needs few other stories. **Flag:** The tmux path for a Claude headless stream needs confirmation.
- [x] Complete: no step is missing from the set.

Unique, uniform, independent, conflict-free and complete are judged across the set. The epic README records that review.

### Definition of ready

This story has acceptance criteria, a journey (or the reason for none), log lines, a security note, a docs list, linked dependencies and a rollback.
