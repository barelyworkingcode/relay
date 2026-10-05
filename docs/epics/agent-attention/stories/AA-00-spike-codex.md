# AA-00 · Spike: record how Codex app-server behaves

**Status: Needs review.** Epic: [E1](../README.md). Blocks: [AA-07](AA-07-codex-decode.md).

## Story

As the relay maintainer, I want recorded Codex transcripts and written answers, so that the Codex adapter rests on facts and not on guesses.

## Time box

Two days. The output is a document and fixture files. The spike adds no product code.

## Questions

1. What is the exact flag for yolo mode?
2. Does an approval arrive as a server request? What is its shape?
3. Can the caller choose the thread id before launch?
4. What command resumes a thread headless? What command resumes it in the terminal UI?
5. How does the caller interrupt a running turn?
6. Does Codex start its own sandbox inside relay's sandbox? Does that fail?
7. Which Codex versions did the spike use?

## Acceptance criteria

- Given a devbox with Codex, when the spike ends, then each question has a written answer.
- Given the answers, when a reader opens the fixtures folder, then it holds one recorded transcript per answered question.
- Given a transcript, when a reader replays it, then it holds no token and no personal path.
- Given question 6, when the answer is "it fails", then the document says how relay's sandbox changes the choice.

## Output

- A document at `docs/epics/agent-attention/codex-findings.md`.
- Fixtures under `internal/sessions/provider/testdata/codex/`.

## Logging

None. The spike runs by hand.

## Journey

None. The fixtures feed the conformance test in [AA-07](AA-07-codex-decode.md).

## Security impact

Fixtures can leak secrets. The spike reviews each fixture before commit.

## Docs

None changes. The findings document is new.

## Rollback

Delete the findings document and the fixtures.

## Checklist

Status: **Needs review.** An unchecked box marks a known weakness. It has a flag.

### INVEST

- [x] Independent: needs few other stories.
- [x] Negotiable: the detail can change.
- [ ] Valuable: someone gains from it. **Flag:** A spike. It gives knowledge, not a feature.
- [x] Estimable: the team can size it.
- [x] Small: fits one sprint.
- [x] Testable: a test can prove it.

### Quality User Story criteria (13)

- [x] Well-formed: has a role and a means.
- [x] Atomic: asks for one feature.
- [x] Minimal: holds only role, means and end.
- [x] Conceptually sound: the means is a feature and the end is a reason.
- [ ] Problem-oriented: states the problem, not the solution. **Flag:** The role is a maintainer. No end user gains directly.
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
