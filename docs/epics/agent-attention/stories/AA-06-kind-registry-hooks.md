# AA-06 · Kind registry, part 2: per-kind hooks

**Status: Needs review.** Epic: [E1](../README.md). Depends on: [AA-05](AA-05-kind-registry-names.md). Blocks: [AA-07](AA-07-codex-decode.md).

## Story

As a relay maintainer, I want each kind to declare its own behavior, so that a new harness cannot inherit a wrong default.

## Scope

A registry entry holds these hooks:

| Hook | Replaces |
|---|---|
| `TemplateID` | The `kindTemplateIDs` map. It has three readers. |
| `Factory` | The `buildProvider` switch. |
| `Capabilities` | The `CapabilitiesForProvider` switch. |
| `WantsModelKey` | The model-key rule. |
| `Sandboxed` | The sandbox rule. |
| `RequiresModel` | The blank-model rule. |
| `HostRefused` | The "pi is refused on a host" rule. |
| `ExtraSandboxGrants` | The pi transcript directory grant. It also feeds the writable roots. |
| `ModelRouter` | The `deriveSessionKind` prefix rule. |
| `BinaryLookup` | The binary discovery in `provider/env.go`. |

Code that is truly Claude-specific stays where it is. Examples: the Claude history readers and the legacy ledger import.

## Defaults fail closed

An unregistered kind gets these answers:

- It runs in a sandbox.
- It gets no extra grant.
- It gets no model key.
- A host project refuses it.

A kind that omits a hook gets the same answers.

## Acceptance criteria

- Given each existing kind, when relay launches it, then the sandbox profile equals the profile before this story.
- Given each existing kind, then the writable roots equal the roots before this story.
- Given a pi launch on a host project, then relay refuses it with `provider_not_available_on_host`.
- Given an unregistered kind, then it gets a sandbox and no grant.
- Given `kindTemplateIDs`, then no reader keeps the old map.
- Given `deriveSessionKind`, then the chat rule stays last, because chat is the catch-all.
- Given a test kind with no hooks, then the test sees the fail-closed defaults.
- Given the gate tests, then each guard still finds a match.

## Logging

No new line. Existing `session.launch` lines keep their codes. A test pins each code: `provider_not_available_on_host`, `template_not_allowed`, `sandbox_unavailable` and `unknown_kind`.

## Journey

**AA-J05 (part 2).** Steps:

1. Launch each existing kind. Compare the sandbox profile to the golden file.
2. Launch pi on a host project. Expect refusal.
3. Register a test kind with no hooks. Launch it. Expect a sandbox and no grant.

## Security impact

This story touches the sandbox, the model key and the host rule. Each is a security invariant in [CLAUDE.md](../../../../CLAUDE.md). The review needs a security reader. A mistake can widen a grant. Golden profile tests guard against that.

## Docs

- [session-host.md](../../../session-host.md): the lines on the kinds, the model key and the sandbox.
- [architecture.md](../../../architecture.md): the model-key lines.
- [package-layout.md](../../../package-layout.md): the registry package.

## Rollback

Revert the commit. AA-07 and later stop. The earlier stories stay.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [x] Valuable: someone gains from it.
- [ ] Estimable: the team can size it. **Flag:** The size depends on a security review of the sandbox hooks.
- [ ] Small: fits one sprint. **Flag:** Follows from the atomic flag.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [ ] Atomic: asks for one feature. **Flag:** Ten hooks. The sandbox hooks may split out.
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
