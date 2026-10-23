# Session history

One list of every agent session relay knows of, with the client's own title and
the time it was last used. It answers `GET /api/session-history` and
`relay session history`. This file holds the records, the readers, the title
rule, the scan bounds, the wire and the resume table. The package is
`internal/sessionhistory`.

## Records

The session ledger (`sessions-ledger.json`) stays the authority index: project,
policy, state, origin and the link to a conversation. It holds no title and no
time. Two things join it here.

| Record | State | Written by | Authority |
|---|---|---|---|
| Launched | `live` or `dormant` | a launch | the project that launched it |
| Discovered | `discovered` | a scan, from a transcript relay did not launch | none: display only |

A discovered record carries `kind` and `agent` `claude`, `host_id` (empty on the
console), `conversation_id`, `directory` (the folder the transcript belongs to)
and `created` (the transcript's first timestamp). It has no `project_id`. Its
id is `d-` plus the first 16 bytes, in hex, of sha256 over the machine key, the
agent and the conversation id, each followed by NUL, so one machine's
conversation always maps to one record.

`AuthorizeLaunch` refuses any record that is not `dormant`, so a discovered
record never resumes through `POST /api/sessions/{id}/resume`. The sweeps that
mark records dormant or clean up a project skip it.

## Readers

One `Reader` per client. This release has the Claude reader; its `Discover`
returns every interactive transcript on a machine and whether the machine
answered. A reader keeps a cache per machine, in memory only, keyed by path with
the file's size and modification time. A file absent from a listing leaves the
cache.

### Scan

Each machine has a worker: one scan at start, then one every
`outsidesessions.ProbeInterval` on the injected clock. The console reads every
folder under `~/.claude/projects`. An SSH host reads only the folders of that
host's registered projects, raw and resolved, as the ended scan does.

- **Listing script.** At most 4096 regular files at depth 2
  (`~/.claude/projects/<folder>/<uuid>.jsonl`), so subagent folders are skipped.
  It prints the home and `path size mtime` per file. The listing is not
  answered when the root is missing or not a folder, when a symbolic link sits on
  `~/.claude` or the root, or when the cap is reached. A symbolic link on a
  file or its folder drops that file.
- **Read script.** Only for files new or changed since the last scan, at most 64
  per scan, newest first; the rest wait for the next scan. Per file it prints
  the last `custom-title` and `ai-title` records of the last 256 KiB (4 KiB
  each), the first user record that is not `isMeta` or `isSidechain` in the first
  64 KiB (8 KiB), the first record carrying `entrypoint`, the first record, and
  the last 16 KiB.
- Both scripts write nothing, and the output stays under the 8 MiB bound of
  `outsideRun`. A path the script printed is accepted only when it is exactly
  `<home>/.claude/projects/<folder>/<uuid>.jsonl` under an allowed prefix.
- **Interactive only.** The first `entrypoint` must be `cli`; a headless
  (`sdk-cli`) transcript yields no entry.
- **Directory.** The ended-row rule (`outsidesessions.LaunchFolder`): the newest
  record in the tail whose `sessionId` is the file's stem and whose `cwd`, or a
  parent of it, maps back to this file. No such record: no entry.
- **Times.** `lastActiveAt` is the newest `timestamp` in the tail, else the
  file's modification time (untimed records are written later than the last
  timed one, so the modification time alone runs late). `createdAt` is the first
  record's `timestamp`, else the modification time.

### Title rule

The first that gives a non-empty title wins:

1. the latest `custom-title` record (`customTitle`), source `client`;
2. the latest `ai-title` record (`aiTitle`), source `client`;
3. the first user prompt's first non-empty line that does not start with `/` or
   `<`, trimmed, source `prompt`;
4. none: `null`.

Control characters are dropped and the title is cut at 120 runes. Transcript
text is untrusted display data and grants nothing.

## The index

After each scan, every entry that no ledger record links (by machine, agent and
conversation id) and that relay-sessions does not manage becomes a discovered
record, written in one save per scan. At most 2000 discovered records per
machine; past that the oldest by `lastActiveAt` are not added. When the managed
read fails, a scan adds no records. A launched record that later holds the same
conversation hides the discovered row.

Titles and times live in memory. After a restart the discovered records are
still in the ledger, so the list is the same; titles and times return when each
machine's first scan ends, and `ready` is `false` until then.

A scan writes one `session.history.scan` event per machine
([events](events.md)). A machine whose scan fails, or whose listing is not
answered, keeps its last answered listing and reports `unreachable`.

## Wire

`GET /api/session-history[?projectId=<id>]`. Class `execute`: socket only,
absent from the TCP mux and from the Chief of Staff scope. An unknown
`projectId` answers an empty `rows`. A history that is not wired answers `503`
`history_unavailable`.

```json
{"ready": true, "revision": 12,
 "machines": [{"hostId": null, "machine": "Console", "status": "answered", "lastScanAt": "2026-10-22T10:00:00Z"}],
 "rows": [{"id": "<ledger record id>", "record": "launched", "kind": "terminal", "agent": "claude",
   "projectId": "p1", "hostId": null, "directory": "/work/acme", "conversationId": "<uuid>",
   "title": "Fix the login page", "titleSource": "client", "lastActiveAt": "2026-10-22T09:58:12Z",
   "createdAt": "2026-10-22T09:30:00Z", "state": "dormant", "transcript": "present",
   "resume": "history"}]}
```

- `ready` is `false` until every machine has finished its first scan, answered
  or not. `revision` grows with every scan, because `lastScanAt` is in the body.
- `machines[].status` is `answered`, `unreachable` or `not_scanned`. A machine
  whose store root is missing is `unreachable`.
- `record` is `launched` or `discovered`. `kind` is `claude`, `pi`, `codex`,
  `chat` or `terminal` (a discovered row is `terminal`). `agent` is `claude` or
  `null`. `state` is `live`, `dormant` or `discovered`.
- `projectId`: the record's project on a launched row. On a discovered row, the
  registered project whose folder holds `directory` on that machine; display only.
- `title` is at most 120 characters or `null`; `titleSource` is `client`,
  `prompt` or `null`. `lastActiveAt` is `null` when no scan has read the
  transcript.
- `transcript` is `present` when the machine's last answered listing holds the
  conversation, `absent` when an answered listing lacks it, `unknown` with no
  answered listing, no `conversationId`, or a client no reader covers.
- Rows run newest `lastActiveAt` first (`null` last), then by `id`; at most 500.

### Resume

The first row of this table that applies decides `resume`. `history` names a
door that a later release adds; until then it answers `404`.

| Row | `resume` | `resumeReason` |
|---|---|---|
| `state: "live"` | `none` | `live` |
| launched, kind `claude`, `pi`, `codex` or `chat`, dormant | `session` | none |
| any other kind that is not a terminal | `none` | `kind` |
| launched terminal or discovered, on a host | `none` | `host_unsupported` |
| launched terminal or discovered, no `conversationId` | `none` | `no_conversation` |
| conversation in the latest outside probe's held set for its machine | `none` | `held` |
| `transcript: "absent"` | `none` | `transcript_absent` |
| no `projectId` | `none` | `no_project` |
| otherwise | `history` | none |
