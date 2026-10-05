# Epic E2 · Chief of Staff

**Status: Needs review.** This epic is an outline. The stories are titles only. They have not passed the INVEST or QUS checks. Refine them after [AA-03](../agent-attention/stories/AA-03-expose-state.md) ships.

## Goal

A person runs many agents. The person wants one conversation that knows what matters.

The Chief of Staff is a threaded chat in eve. An LLM backs it. It reads session state from [E1](../agent-attention/README.md). It reports what needs the person. It stays quiet about the rest. It carries the person's instructions to agents.

## Decisions

**D1 · The Chief of Staff reads reducer output.** It does not read the raw event stream. The reducer sends state changes and a short excerpt at each turn end.

**D2 · The Chief of Staff can send instructions.** The owner says "tell this agent to merge" and relay sends it. There is no confirmation step. The owner accepts the risk.

**D3 · Each instruction is traceable.** Relay marks every message that the Chief of Staff sends. The audit log records it. The owner can tell it from a typed message.

**D4 · The idle-versus-waiting decision uses the LLM.** A finished turn that ends with a question is "waiting on you". The reducer cannot tell. The Chief of Staff reads the excerpt and decides.

## Candidate stories

| Id | Title | Depends on |
|---|---|---|
| CS-01 | Chief of Staff thread in eve, backed by a hidden session | E1 AA-03 |
| CS-02 | Event queue and digest | CS-01 |
| CS-03 | Editable rules: what to report and what to hide | CS-01 |
| CS-04 | Send an instruction to a session, with an audit mark | CS-01 |
| CS-05 | Morning brief uses the digest | CS-02 |

## Open questions

- Which model backs the thread? A system model cannot host a chat today.
- Does the Chief of Staff see every project? Relay credentials are project-scoped. A cross-project reader needs a deliberate grant.
- Where does the always-on reader run? Eve's server is the likely place.
- What is the hard limit on cost per day?

## Risks

- Agent output is untrusted. An injected agent can try to steer the Chief of Staff. The Chief of Staff can send instructions. The risk is accepted for now (D2). Rules and the audit mark limit the damage.
- A hidden session needs a filtered name prefix in eve, and it must register before it joins. See the eve notes on hidden sessions.

## Review

All files: **Needs review.**
