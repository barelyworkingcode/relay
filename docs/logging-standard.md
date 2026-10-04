# Logging standard

One contract for every relay service: levels, line keys and a trace ID. It is
not shared code. Each service is written in its own language and implements
the contract itself.

The reader this is for is a developer who has a failure and needs to find the
step that failed without reproducing it. Users never see these logs. They stay
in the background: small, bounded, and never shown on a screen, in a prompt or
in a message.

The machine-readable form of the line format is
[`logging-schema.json`](logging-schema.json). This document is the reasoning;
the schema is what a test checks.

## What a service does

1. Write one JSON object per line to **stderr**. Do not open or rotate a log file.
2. Give every line the keys below.
3. Give every user action one `trace_id`, and pass it on every call you make.
4. Log one `info` line at the end of each operation. Log nothing per poll,
   heartbeat or stream chunk.
5. Never log a prompt, a body, a token or any other credential.

## Levels

| Level | Use it when |
|---|---|
| `error` | An operation failed and a developer or operator needs to look at it. |
| `warn` | An operation was degraded, retried or refused, and the service carried on. |
| `info` | An operation finished. One line per operation boundary. |
| `debug` | Detail that only helps while diagnosing. Off by default. |

`debug` is off by default. It is turned on with the environment variable
`RELAY_LOG_LEVEL=debug`, read once at start. A service that starts at `debug`
returns to `info` after 30 minutes and logs one `warn` line saying so. A
forgotten flag cannot write all week. The 30 minutes run from each process start,
so a service that restarts gets a new window; the rotation cap still bounds disk use.

`RELAY_LOG_LEVEL` accepts `error`, `warn`, `info` and `debug`.

## Line format

One JSON object per line. No pretty-printing, no multi-line values.

| Key | Type | Meaning |
|---|---|---|
| `ts` | string | UTC, RFC 3339, milliseconds, ending in `Z`: `2026-01-02T03:04:05.678Z`. |
| `level` | string | `error`, `warn`, `info` or `debug`. |
| `msg` | string | Short human text, at most 500 characters. No values that belong in a key. |
| `service` | string | The service id, for example `relay-scheduler`. |
| `op` | string | The operation, dotted and lower case: `schedule.create`, `job.fire`. |
| `status` | string | `ok`, `error` or `denied`. |
| `duration_ms` | integer | How long the operation took. `0` for a line that marks a point in time. |
| `error` | string | Names the failure, at most 500 characters. Empty when `status` is `ok`. |
| `trace_id` | string | The trace ID, or an empty string outside any user action. |

Every line carries all nine keys; none is omitted. Only three may be empty or
zero: `error` is the empty string when `status` is `ok`, `trace_id` is the empty
string outside any user action, and `duration_ms` is `0` on a line that marks a
point in time. `ts`, `level`, `service`, `op` and `status` always hold a real value.

`service` is the service id Relay uses for the log file name, `logs/<service-id>.log`.

`status` and `level` agree. `ok` logs at `info` or `debug`. `denied` logs at
`warn`. `error` logs at `error`, or at `warn` when the service recovered. A `debug`
line in the middle of an operation uses `ok`.

Optional entity keys name what the operation touched. They are ids, never
content:

| Key | Meaning |
|---|---|
| `session_id` | A terminal, chat or agent session. |
| `job_id` | A scheduled job. Stable across every run of that job. |
| `run_id` | One run of a job. New for every fire. |

A service may add other keys. They hold names and ids, not values from a user.

Lines that do not start with `{` are allowed. They are panics, stack traces and
third-party output, because Relay merges stdout and stderr into one file. They
carry no `trace_id`, and nothing validates them.

## Trace ID

One trace ID identifies one user action as it crosses services. Searching the
ID in every service's log returns the whole path of that action.

- **Who creates it.** The first service to receive the action: a browser
  request, a CLI call, a job that fires. A service that receives an ID keeps it.
- **Format.** Generated as 32 lowercase hex characters from a random source.
  Accepted inbound if it matches `[A-Za-z0-9_-]{8,64}`.
- **Validation.** Inbound IDs are untrusted. A service that receives an ID with
  another length or any other character discards it and creates a new one. This
  keeps a caller from forging or injecting log lines.
- **A new action starts a new trace.** A job that fires hours after it was
  created starts its own trace. It logs a new `run_id` and the stored `job_id`.
  The service that creates a job logs its `job_id` on the creation line, and
  the scheduler logs the same `job_id` on every fire. To join creation to every
  run, search the `job_id`.

### Carriers

| Transport | Carrier |
|---|---|
| HTTP | Request header `X-Trace-Id`. |
| WebSocket | The upgrade request carries `X-Trace-Id` for the handshake only. A message that starts an action carries a `trace_id` field of its own. |
| MCP calls | `_meta.trace_id` in the call parameters. Only Relay, as the mediator, creates `_meta`; a client Relay does not mediate carries the ID some other way, because a bare `_meta` marks a call as mediated. |
| Bridge requests (newline JSON over a unix socket) | A `trace_id` field on the request. |
| Remote requests and the file-mount attach preamble (newline JSON over mutual TLS) | A `trace_id` field. The listener decodes these strictly and rejects unknown fields, so Relay accepts the field before any client sends it. |
| Length-prefixed JSON frames (speech services) | A `trace_id` field in the request JSON. |
| A spawned subprocess | Environment variable `RELAY_TRACE_ID`. It names the action that started the child. The child uses it for its startup lines only, never re-exports it, and a spawner removes an inherited one before setting its own. |

The header is `X-Trace-Id`, not `X-Relay-*`. The model endpoint and the model
router delete every inbound `x-relay-*` header, so a header in that family would
be dropped before it reached the next hop.

`X-Trace-Id` is dropped on any call that leaves the machine, such as a request to a hosted
model provider. The ID is not a secret, but it has no meaning there.

A call made while serving a request passes the request's ID. A call made with
no request in hand, such as a background tick, creates a new ID or sends none.

### Audit log

The audit log has its own schema and stays separate. Each audit record gains a
`trace_id` field, so a developer can go from a log line to the audit record of
the same action. The field comes from what the caller sent, like the audit log's
other caller-supplied fields, so it joins records to a trace but proves nothing
about who acted. The audit log's durability rules do not change.

## Location and volume

- A service writes to stderr and manages no log file.
- Relay captures a launched service's stdout and stderr into one file,
  `logs/<service-id>.log` under its application-support directory. The writer
  rotates by size at 8 MiB and keeps one older file, so a service holds at most
  about 16 MiB. There is no age limit; the size cap bounds disk use and writes.
- A service that Relay does not launch, for example one started by hand in a
  terminal, logs to that terminal.
- A stdio MCP server's stdout carries the protocol. It writes logs to stderr only.
- Output a service copies from a child process goes into one field of the service's own line, or to its own file. It is never written raw into the service's JSON stream, where it could pose as the service's own lines.
- Do not log per poll, heartbeat, reconnect attempt or stream chunk. Log the
  operation once, when it ends.
- A failure that repeats logs its first occurrence, then one line with a count
  at an interval, not one line per repeat.

## What a service never logs

Names, not values. Never write any of these to a log line, at any level:

- a prompt, a model response or a transcript;
- a request or response body, or tool arguments;
- a token, key, password, cookie or authorization header;
- text copied from a child process's output without redaction.

A line names the tool, the route or the id. It does not carry the content.

## Tracing a failure

A developer asks why a scheduled task did not run at 09:00.

1. Search the scheduler's log for the `job_id`. The creation line has the
   `trace_id` of the request that created it, and every fire has its own
   `run_id` and `trace_id`.
2. Search that fire's `trace_id` in every service's log. The path reads in time
   order: scheduler `job.fire`, then the frontend, then the session host, until
   the line with `status` `error` and its `error` text.
3. If the fire never appears, the scheduler's log shows no `job.fire` for the
   `job_id`, which points at the scheduler and not at what it calls.

## How each repo logs today

Facts as of this document. "Must change" is what the repo needs to meet the
standard; each repo files its own issue for it. Adopt the standard in new code
and in code a repo already changes. It does not require rewriting lines that
are not touched.

| Repo | Today | Must change |
|---|---|---|
| relay | `slog` text handler to stderr, level from `RELAY_LOG_LEVEL` (default info). Teed to `logs/relay.log` by the rotating writer. Local-time RFC 3339. About 360 calls. No trace ID. A separate audit log with its own schema, UTC `ts` and uuid `id`. | Switch to the JSON handler with the nine keys. Create a trace ID at the frontend server, bridge, model endpoint and remote listener, and carry it in context. Pass it through the bridge request, MCP `_meta`, the frontend reverse proxy, WebSocket upgrade and start-of-action messages, session host, model broker and spawn env. Accept `trace_id` on the remote request and the mount attach preamble before any client sends it. Wrap child output instead of writing it raw. Forward `X-Trace-Id` on the model path; the `x-relay-*` strip does not touch it. Capture a stdio MCP's stderr, which is discarded today. Add `trace_id` to audit records. Drop or aggregate repeating lines. |
| relay-sessions (in the relay repo) | Mix of stdlib `log` and `slog`. No level, local time. | Use the same handler as relay. Replace the stdlib `log` calls. |
| relayLLM | `slog` default handler: local-time text, no level control, so `debug` lines never appear. Re-emits managed child process output verbatim. No ID. Strips inbound `x-relay-*` headers. | JSON handler, `RELAY_LOG_LEVEL`, UTC `ts`. Middleware that creates and validates the trace ID, and passes it on the bridge request, proxy headers and child env. Bound and redact the child-output passthrough. One `info` line per request. |
| relayScheduler | `slog` default handler: local-time text, no level control. Run history is JSON files and is not a log. A job ID exists (`Task.ID`). The wire field `runId` holds a session or terminal id, so it is not a per-fire log key. Execution records use UTC. | JSON handler, `RELAY_LOG_LEVEL`, UTC `ts`. Accept a valid inbound trace ID or create one in the create and run-now handlers, and create one when a job fires. Add the header to every outbound call and WebSocket dial. Add `job_id` and a new `run_id` for each fire to each line; do not reuse the wire `runId`. |
| eve | Hand-written logger: text `[Prefix] message`, no timestamp, no level printed. Level from `LOG_LEVEL`. Debug and info to stdout, warn and error to stderr. No ID. | Write JSON lines to stderr only. Read `RELAY_LOG_LEVEL`. Add middleware that creates and validates the trace ID for HTTP and WebSocket. Pass it through the relay transport and the speech frames. Remove per-chunk and per-reconnect lines. Carry the trace ID in the WebSocket message that starts a chat turn. |
| relayRemote | `slog` text handler to stderr, level fixed at info, key `time`. One debug line, unreachable. Some CLI text uses `fmt` directly. | JSON handler, `ts` key, `RELAY_LOG_LEVEL`. Create a trace ID per tool call. Add `trace_id` to the remote request, after relay accepts it. Remove the frame excerpt from the debug line. |
| relayHarness | `fmt` messages to stderr. Own audit file of one JSON record per tool call, and an unrotated `fsmcp.log` of child stderr. UTC RFC 3339 `ts` in the audit record. A random session ID. No trace ID. | Log in the standard format to stderr. Map audit keys to `status`, `duration_ms` and `session_id`, and add `service`, `op`, `error` and `trace_id`. Create a trace ID per turn. Pass it on the bridge request, the model request header and MCP `_meta`. Fold `fsmcp.log` into the standard or drop it. |
| fsmcp | No logging. Stdout is the MCP protocol. | Add a stderr JSON writer. Read and validate `_meta.trace_id`. One `info` line per tool call, with the tool name and no arguments. |
| relayFS | No logging. Plain `fmt` messages to stderr for usage and errors. | Add a stderr JSON writer. Carry the trace ID in the attach preamble, after relay accepts it. One line each for attach, mount, unmount, probe and session loss. |
| relayComfy | A relay-launched service. Its MCP shim uses `slog` with the default handler (local-time text, stderr) and about 10 calls; one line prints a bare `fmt` message. The daemon is a wrapper script around a ComfyUI child process. No ID. | JSON handler on stderr, `RELAY_LOG_LEVEL`, UTC `ts`. Read `_meta.trace_id` per tool call and log one `info` line. Wrap ComfyUI child output instead of passing it raw. |
| relayTelegram | Stdlib `log` text lines, a handful of calls, local time, no level. A client of eve. No ID. | JSON handler on stderr, `RELAY_LOG_LEVEL`. Create a trace ID per incoming message and send it as `X-Trace-Id` to eve. |
| macMCP | No logging. Stdout is the MCP protocol. | Add a stderr JSON writer. Read `_meta.trace_id`. One `info` line per tool call. Relay must capture the stderr. |
| relaySTT | `print` to stdout, no timestamp or level. Requests are length-prefixed JSON. | Replace `print` with the stderr JSON writer. Add `trace_id` to the request frame and the outbound HTTP header. |
| relayTTS | `print` to stdout, no timestamp or level. Same framing as speech-to-text. | Same as relaySTT. |

## Not covered

- Client apps on a device, such as the iOS client, which log to the system log of that device.

- A tracing or metrics stack.
- Capturing request or response bodies.
- A shared logging library. A service meets the contract in its own language.
