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
agent and the conversation id, joined by NUL, so one machine's
conversation always maps to one record.

A conversation relay-sessions manages gets no discovered record once the host
reports it as managed. Before the managed session's first turn no transcript
exists, so there is nothing to discover.

`AuthorizeLaunch` refuses any record that is not `dormant`, so a discovered
record never resumes through `POST /api/sessions/{id}/resume`. The sweeps that
mark records dormant or clean up a project skip it.

## Readers

One `Reader` per client. This release has the Claude reader; its `Discover`
returns a `Listing`: whether the machine answered, whether it is `Complete`
(every transcript printed, under the cap), `Scope` (the folder prefixes used; nil
is every folder), `Present` (every transcript the
listing script saw, read or not, interactive or not; ask `Listing.Has(agent, id)`)
and `Entries` (only the files read and parsed, interactive ones). A reader keeps a cache per machine, in memory only, keyed by path with
the file's size and modification time. A file absent from a listing leaves the
cache.

### Scan

Each machine has a worker: one scan at start, then one every
`outsidesessions.ProbeInterval` on the injected clock. The console reads every
folder under `~/.claude/projects`. An SSH host reads only the folders of that
host's registered projects, raw and resolved, as the ended scan does.

- **Listing script.** Regular files at depth 2
  (`~/.claude/projects/<folder>/<uuid>.jsonl`), so subagent folders are skipped,
  sorted by modification time, newest first, at most 4096 printed. It prints the
  home and `folder/stem size mtime` per file, and `X` when more files follow or
  when `find` or `stat` failed (a missing file is then possibly a listing gap).
  It prints an `R` line, the base64 folder name, for each symbolic-link folder
  and for the folder of each symbolic-link transcript; these make
  `Listing.Refused`. The listing is not answered when the root is missing or not a folder, or when a
  symbolic link sits on `~/.claude` or the root. Past the cap it still answers
  but is not `Complete`; a `find` or `stat` failure also makes it not `Complete`. A symbolic link on a file or its folder drops that file.
- **Read script.** Only for files new or changed since the last scan, at most 64
  per scan, newest first; the rest wait for the next scan. Per file it prints
  the last `custom-title` and `ai-title` records of the last 256 KiB (4 KiB
  each), up to the first four user records that are not `isMeta` or `isSidechain` in
  the first 64 KiB (8 KiB in all), the first record carrying `entrypoint`, the first record, and
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
3. the first non-empty line, not starting with `/` or `<` and trimmed, of the
   first of those user records that gives one, source `prompt`;
4. none: `null`.

Control and format characters (bidi overrides, zero-width) are dropped and the title is cut at 120 runes. Transcript
text is untrusted display data and grants nothing.

## The index

After each scan, every entry that no ledger record links (by machine, agent and
conversation id) and that relay-sessions does not manage becomes a discovered
record, written in one save per scan. At most 2000 discovered records per
machine: relay keeps the 2000 newest, held and new together, ranked by the
entry's `lastActiveAt`, else the record's `created`, and removes the rest in the
same save (eviction, not purge: no audit row, and the scan event's `evicted`
counts them). A removed record is added again only when its transcript becomes
newer than the 2000th. When the managed
read fails, a scan adds no records. A launched record that later holds the same
conversation hides the discovered row.

Titles and times live in memory. After a restart the discovered records are
still in the ledger, so the list is the same; titles and times return when each
machine's first scan ends, and `ready` is `false` until then.

A scan writes one `session.history.scan` event per machine
([events](events.md)). A machine whose scan fails, or whose listing is not
answered, keeps its last answered listing and reports `unreachable`.

After every scan whose managed read succeeded, relay removes, in one save, the
machine's discovered records whose conversation relay-sessions now holds (an
adopted session). Only records still `discovered` go; a failed read removes
nothing. The event's `superseded` counts them.

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
- `transcript` is `present` when the machine's last answered listing `Has` the
  conversation, `absent` when `Listing.Absent(agent, id, directory)` is true,
  else `unknown`. `Absent` needs an answered, `Complete` listing that lacks the
  conversation, whose scope covers the directory's folder, and which refused no
  folder named like it or starting with it; a listing past the
  cap, a host project removed from the scope, a missing `conversationId` or a
  client no reader covers all give `unknown`. A
  `present` row whose file no scan has read yet shows `title` and `lastActiveAt`
  `null`. A launched agent record has a `conversationId` only once a probe has linked it
  (see below); until then its row is `unknown`.
- Rows run newest first by `lastActiveAt`, else by `createdAt` (the wire value
  stays `null`), then by `id`; at most 500.

### Resume

The first row of this table that applies decides `resume`. `history` names the
history door, `POST /api/session-history/{id}/resume`.

| Row | `resume` | `resumeReason` |
|---|---|---|
| `state: "live"` | `none` | `live` |
| launched, kind `claude`, `pi`, `codex` or `chat`, dormant, `transcript: "absent"` | `none` | `transcript_absent` |
| launched, kind `claude`, `pi`, `codex` or `chat`, dormant | `session` | none |
| any other kind that is not a terminal | `none` | `kind` |
| launched terminal or discovered, on a host | `none` | `host_unsupported` |
| launched terminal or discovered, no `conversationId` | `none` | `no_conversation` |
| conversation in the latest outside probe's held set for its machine | `none` | `held` |
| `transcript: "absent"` | `none` | `transcript_absent` |
| no `projectId` | `none` | `no_project` |
| otherwise | `history` | none |

## Terminal records and resume

### The record

A terminal launched from a console project's `claude-code` template gets a
ledger record at launch: `kind` `pty`, `agent` `claude`, `template_id`,
`project_id`, `directory`, `origin`, state `live`, and the bound `root` like any
launched record. A drop-in terminal, an outside viewer, a host project's terminal
and every other template get none. The record is `dormant` after the terminal
exits, after its tab closes and after a relay restart.
`POST /api/sessions/{id}/resume` refuses a terminal record
(`session_not_resumable`, "resume this terminal session from the session
history"), and refuses a discovered one because it is not dormant.

### The link

Every outside probe calls `NativeProbe.Links()`: a map from a
`RELAY_SESSION_ID` marker to the Claude session id of the process carrying it.
A process counts only with a registry entry (`~/.claude/sessions/<pid>.json`)
for its own pid whose lifetime matches, whose `cwd` equals the process's and
whose `sessionId` is a conversation id. A marker two such processes carry is
dropped. No transcript is read.

The marker is the process's own claim, so a link sets the record's
`conversation_id` only when the record exists, is `live`, is a terminal or
`claude` record on this machine, the link's folder lies inside the record's
`directory` and, on the console, the pid descends from the record's bound root
whose start time still equals the one the kernel attested at launch. A failed
check links nothing and logs at Debug. Each change writes one
`session.history.link` event. `LinkConversation` sets the conversation and, in
the same save, removes every discovered record and every dormant terminal record
of the same machine, agent and conversation whose `directory` equals the link's
folder or holds it by path segments; it never removes a live record or
an agent record. A terminal that ends before any probe sees it keeps a record
with no conversation (`no_conversation`); its transcript lists as a discovered
row.

### The resume door

`POST /api/session-history/{id}/resume`, body `{"cols","rows"}`, class
`execute` (socket only, absent from the Chief of Staff scope). The checks run in
this order and the first failure answers (codes in [routes](routes.md)):

1. the body; 2. the record; 3. a terminal or discovered record, else
`use_session_resume`; 4. not `live`; 5. the console only; 6. a conversation;
7. a fresh console probe and the managed list; 8. the conversation not held by a
live process or relay-sessions; 9. `Index.Verify`: a transcript read now,
bypassing the scan cache, which derives the folder by the ended-row rule
(`transcript_absent` when none, `no_project` when no folder); 10. the project: a
discovered record's is the registered local project holding the verified folder,
a terminal record keeps its own project and folder and the verified folder must
equal it; 11. `AuthorizeLaunch` with the `claude-code` template and
`--resume <conversation id>`; 12. the host launch and the ledger commit; 13.
`LinkConversation` with the verified folder, which removes the old record.
`POST /api/sessions/{id}/resume` refuses a terminal or discovered record, live
or dormant, with `403 session_not_resumable` and a `denied` `session_resume`
audit row. `Inventory.Held` answers "no fresh probe" once the machine's last
probe is older than two probe intervals, so step 8 refuses with `unavailable`.

The 201 body is the terminal body plus `supersedes`. The launch writes its
ordinary `session_launch` row and no other audit row is added; the
`session.history.resume` event carries `session_id`, `new_session_id` and the
outcome. A second resume of one id answers `409 resume_in_progress`.
`LaunchRequest.ResumeConversation` is internal: only this door sets it, never a
request body, and it is valid on a console `claude-code` launch alone.

## Purge

Relay removes a history record whose transcript is gone, so the history offers
only what can still be resumed. The pass runs once after each machine scan that
answered and reads absence only through `Listing.Absent`: the listing answered
and was complete, its scope covers the record's folder, it refused no folder
named like it, and it does not hold the conversation. A capped, stale,
out-of-scope or refused listing is unknown, never absent, and sets nothing.

Only a launched `claude` terminal record and a discovered record are
candidates. A launched agent record whose transcript is gone stays and lists
`resume: "none"`, `resumeReason: "transcript_absent"`. A record is skipped,
whatever the listing says, when it is `live`, has `adopt_pending` set, or its
conversation is in the machine's held set or relay-sessions' managed list. A
held set that is stale (the machine's last probe is older than two probe
intervals) or unreadable, or an unreadable managed list, stops the pass for
that machine. So does an answered listing that holds no transcript at all
while candidates exist: that reads as a store that went away.

1. A candidate the listing holds, or whose transcript reads unknown (out of
   scope, refused or in a capped listing), has its `missing_since` cleared: an
   unknown stretch breaks "stayed absent", so the grace period starts again.
2. A candidate that is absent and has no `missing_since` gets the scan's time.
   A record with no `conversation_id` counts as absent, under the same rule.
3. A candidate absent for the grace period gets a `session_purge` audit row,
   then is removed, only if the guards above still hold under the ledger's lock.
   A failed audit write keeps the record. With auditing off nothing is marked
   or purged.

Each purge writes one `session.history.purge` event ([events](events.md)). The
cap on discovered records (2000 per machine) evicts without a purge and writes
no audit row.

### The setting

`session_history.purge_after_days` in `settings.json` is the grace period in
whole days, 0 to 90, default 7. A hand edit applies through the settings-file
watch. 0 turns purge off. Any other value turns it off and logs one Warn when
the value changes. `relay audit --event session_purge` lists what was purged.
