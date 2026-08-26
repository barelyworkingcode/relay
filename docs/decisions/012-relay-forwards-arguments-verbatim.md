# ADR-012: Relay Forwards a Call's Arguments, It Does Not Re-Serialise Them

**Status:** Accepted
**Date:** 2026-08-26

## Context

fsMCP refuses to write a lone (unpaired) UTF-16 surrogate. Its docs call the
refusal unconditional and say why: there is no valid UTF-8 encoding for a
surrogate code point, so writing one means silently substituting U+FFFD — the
lossy shape fsMCP's issue #11 exists to prevent. `JSON.parse` does not validate
surrogate pairing, so a wire string *can* carry one, which is precisely why the
check is at the JSON-RPC boundary.

Through relay that refusal never fired (issue #40). Measured, the same call two
ways:

    through relay:                 Wrote 5 bytes;  disk holds 61 ef bf bd 62
    bare stdio to the same server: refused, nothing written

`ExternalMcpManager.CallTool` decoded the caller's arguments into an
`interface{}` and re-encoded them before handing them to the MCP. Go's
`encoding/json` substitutes U+FFFD for an unpaired surrogate on the way into a
Go `string`, so fsMCP received a perfectly valid `a\ufffdb` and wrote it,
correctly and successfully. The corruption happened one layer up, where nothing
was watching.

The audit log carried the same defect independently. `redactArgs` decoded,
redacted, and re-encoded, so the recorded value was the *already substituted*
one. The log is the operator's ground truth and this project's docs are
emphatic that it is the only thing to trust; an operator reconstructing the
event from the log would have concluded the client sent the corrupted bytes.
A record that paraphrases and reads as a verbatim quote is worse than no record.

The surrogate is one instance. A decode/re-encode round trip through Go values
also sorts object keys, collapses duplicate keys to the last one, reformats
numbers through `float64` (`1.0` becomes `1`, `1e2` becomes `100`, a 20-digit
integer loses its low digits), and collapses redundant escapes (`\u0041`
becomes `A`). All of that was reaching MCPs and the log, and none of it was
intended.

## Decision

**Relay is a broker, not a re-serialiser.** Tool arguments are the client's
data. Relay's job is to decide whether the call is allowed — not to normalise
its payload.

1. **Arguments are forwarded as `json.RawMessage`, never decoded.**
   `ExternalMcpManager.CallTool` builds `params` as
   `map[string]json.RawMessage` and puts the caller's bytes in it unchanged.
   The arguments are still **validated** — unmarshalling into a
   `json.RawMessage` runs the same syntax check the old decode did, and
   malformed arguments are still refused with the same error — but nothing is
   materialised as a Go value. The pattern is already in the codebase
   (`Project.Context`, `jsonrpc.ServerRequest.Params`, the whole bridge and
   remote wire); this extends it to the last hop, which was the only one that
   had dropped it.

2. **`_meta` is assembled the same way**, as `map[string]json.RawMessage`. A
   scope value is the operator's data and relay has no more licence to rewrite
   that than it has to rewrite the client's.

3. **The outbound JSON-RPC frame is encoded with HTML escaping off**
   (`marshalJSONVerbatim`, `wire_json.go`, used by both the stdio and the HTTP
   transport). `json.Marshal` compacts an embedded `json.RawMessage` with
   escaping *on*, which rewrites `<`, `>` and `&` inside the caller's strings.
   Lossless, but still relay editing a payload it has no business editing, and
   it makes the property below untestable as a byte equality.

4. **The audit log records the same bytes the MCP received.** `redactRaw`
   (`audit.go`) walks the arguments *as bytes*: it decodes an object key only
   to ask whether the key looks like a credential, and copies the key's own
   source bytes into the output either way. Key order, duplicate keys, number
   spelling and every escape survive; a credential value is still replaced with
   `"[redacted]"`. Nothing else is rewritten.

5. **The record stays bounded.** Arguments are unbounded — a file write carries
   its whole content — and the log is append-only, so `max_arg_bytes` (4 KiB by
   default) still applies and an over-cap payload still degrades to a truncated
   JSON *string* with `args_truncated: true` and the full size in `args_bytes`.
   The record is bounded first and faithful within that bound, in that order.
   Storing the raw bytes without a bound was never on the table.

### The property, stated exactly

> The arguments an MCP receives are `json.Compact` of the arguments the client
> sent, and the arguments the audit log records are those same bytes.

Compaction is not optional: the stdio transport is newline-delimited, so a
caller's pretty-printed arguments *must* lose their insignificant whitespace or
the frame breaks. No MCP can observe the difference.

One measured exception, pinned by
`TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged`: Go's encoder
spells a raw U+2028/U+2029 inside a `json.RawMessage` as `\u2028`/`\u2029` even
with `SetEscapeHTML(false)`, and there is no seam to suppress it short of
hand-assembling the frame. `\u2028` decodes to U+2028 and to nothing else, so
the document's *meaning* is untouched. That is the line this ADR draws: relay
may not change what a document means, and does not claim to preserve how it was
spelled.

## What we deliberately did NOT do

- **Detect and refuse an unpaired surrogate at relay's ingress.** Safe, and it
  was option 2 in the issue, but it puts a content judgement in the broker,
  which is the wrong layer for it — and it fixes only the case someone thought
  of. Forwarding the bytes fixes every value relay was silently normalising,
  including the ones nobody has noticed yet.

- **Delete the guarantee from fsMCP's docs.** The guarantee is worth having and
  decision 1 restores it cheaply. fsMCP's side is a documentation note that the
  refusal holds over stdio and can be defeated by a broker that re-encodes.

- **Extend this to results.** Results already travel as `json.RawMessage` from
  the MCP to the caller with no decode anywhere, so there was nothing to fix.
  They are *not* covered by decision 3's escaping change on the inbound side of
  the bridge, which is a separate frame and a separate day's work if it ever
  matters.

- **Validate UTF-8 inside strings.** Relay does not, and JSON's syntax check
  does not either. If a client sends arguments containing raw invalid UTF-8
  bytes they are forwarded as sent — which is the whole point — but the
  *over-cap* audit path re-encodes the truncated prefix as a JSON string, and
  Go substitutes U+FFFD for invalid UTF-8 there. That path is already labelled
  `args_truncated`, so a reader knows the record is not the whole story.

## Consequences

- **Good:** fsMCP's documented refusal fires again through relay, and every
  other MCP's boundary checks now see what the client actually sent.
- **Good:** the audit log stops lying. `relay audit` prints the recorded
  `args` as JSON text, so a lone surrogate shows as the escape `\ud800` rather
  than as a replacement character an operator would blame on the client.
- **Good:** the log now shows the caller's key order, which is often the
  clearest signal of *which* client wrote a call.
- **Trade-off:** a duplicate key now reaches the MCP as a duplicate key.
  Relay declines to pick one, because which one a receiver honours is the
  receiver's business and relay quietly choosing the last was never a decision
  anyone made.
- **Trade-off:** tests that want Go values out of a captured `tools/call`
  params map have to decode them (`decodedToolParams` in `mock_mcp_test.go`).
  That is the point: nothing on the call path does it any more.
- **Trade-off:** `cmd/testmcp` now echoes with HTML escaping off, so its echo
  is evidence about relay rather than about itself.

## See also

- Issue #40 — the report, with the bare-stdio contrast that isolated the
  substitution to relay
- ADR-008 — the audit log at the router chokepoint; this ADR narrows what
  "redacted arguments" means
- ADR-011 — resource scope; the `_meta` this ADR stops re-encoding
- `wire_json.go` — `marshalJSONVerbatim`
- `arg_verbatim_test.go` — the regression suite, run against the real
  `cmd/testmcp` stdio child
- `docs/audit-log.md` — "Arguments and results"
