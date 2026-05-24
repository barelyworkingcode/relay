---
name: triage-issue
description: Walk a GitHub issue from open → labeled → owned → linked-PR-or-closed.
---

# Triage Issue

Use when the user pastes a GitHub issue URL or says "triage <issue>".

## Steps

1. Read the issue body + comments. Summarize in one sentence.
2. Reproduce locally if there's a code path involved.
3. Apply the right labels: one of `bug | feature | docs | infra`, plus a
   priority `p0 | p1 | p2`.
4. Assign an owner (default: the person nearest the change in `git log`).
5. Either link a draft PR (if the fix is obvious and < 30 lines), or
   close with a comment explaining why we won't do this.
6. Post a one-line summary in the triage channel.
