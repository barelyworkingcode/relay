# AA-05 · Kind registry, part 1: names and validation

**Status: Needs review.** Epic: [E1](../README.md). Blocks: [AA-06](AA-06-kind-registry-hooks.md).

## Story

As a relay maintainer, I want one list of session kinds, so that a new harness needs one entry and not a dozen edits.

## Background

The kinds `claude`, `pi` and `chat` appear in about 34 places. Twelve are plain name and validation switches. The constants exist twice: in `cmd/relay` and in `internal/sessions/session`.

## Scope

- One definition of the kind names.
- One function that answers "is this a known kind?".
- The existing switches read that function.
- No behavior change.

## Out of scope

- Per-kind behavior. That is [AA-06](AA-06-kind-registry-hooks.md).
- A new kind.

## Acceptance criteria

- Given the old code and the new code, when the same launch request arrives, then both give the same answer.
- Given an unknown kind, when a launch arrives, then relay refuses it.
- Given the kind constants, then one package defines each name once.
- Given the persisted `ProviderType` strings, then they do not change.
- Given the full test suite, then no test changes except an import path.

## Logging

| Op | Level | Status | Extra keys | When |
|---|---|---|---|---|
| `session.launch` | warn | denied | `session_id`, error `unknown_kind` | A request names an unknown kind. |

Existing lines do not change.

## Journey

**AA-J05 (part 1).** Steps:

1. Send a launch request with kind `nonesuch`.
2. Expect refusal.
3. Expect the `session.launch` denied line.

## Security impact

The risk is a default that widens. An unknown kind must get no grant and a sandbox. This story keeps the current answer. A test pins it.

The gate tests match source text. Run them before and after. Confirm that each guard still finds something.

## Docs

- [session-host.md](../../../session-host.md): the line that names the three kinds.
- [CLAUDE.md](../../../../CLAUDE.md): line 25 names the kinds. The reviewer decides if it changes.

## Rollback

Revert the commit. The change is mechanical.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [ ] Valuable: someone gains from it. **Flag:** Only maintainers gain. No end user sees it.
- [x] Estimable: the team can size it.
- [x] Small: fits one sprint.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [x] Atomic: asks for one feature.
- [x] Minimal: holds only role, means and end.
- [x] Conceptually sound: the means is a feature and the end is a reason.
- [ ] Problem-oriented: states the problem, not the solution. **Flag:** The role is a maintainer.
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
