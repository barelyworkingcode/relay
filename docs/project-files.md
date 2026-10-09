# Project files

Relay serves, contains and records the file operations eve performs on a
project: list, stat, read, stream, write, mkdir, rename, move, delete, search,
git (read-only), watch, and pastetmp for an SSH host. One place enforces the
rules and writes the audit, for a console project and a host project alike.

## Doors

Every operation is an `execute`-class route on the frontend socket, so the
loopback TCP listener serves none of them:

| Op | Route |
|---|---|
| list, stat, read, write, mkdir, rename, move, delete, search, git | `POST /api/projects/{id}/files/<op>` |
| stream | `GET /api/projects/{id}/files/stream?path=` |
| pastetmp | `POST /api/hosts/{id}/pastetmp` |
| watch, host status | `GET /ws/files` (WebSocket) |

`FileOps` (`cmd/relay/file_ops.go`) is the one core behind them. Routes only
decode and encode. The wire shapes and error codes are pinned by eve's client
and its fake relay; the strings live in `internal/projectfs/projectfs.go`.

## Check order

project, kind, lexical path, read-only, audit intent, backend. The project is
read from fresh settings on every request, so a `files_read_only` change or a
moved path applies to the next call. An access-profile project, or one with no
path, answers `NOT_AVAILABLE`.

## Containment

A path is cleaned lexically first (`CleanRel`): a `..` segment is `TRAVERSAL`.
The console backend then opens every path with `openat` from the root
directory and `O_NOFOLLOW_ANY`, so the kernel refuses a symbolic link in any
component: `SYMLINK`. There is no check-then-open window for a session that
plants links in its own project folder. List reports a link as type `symlink`
and never descends into it; search skips links. The root path itself may
contain links. On a host the agent walks each component with `lstat` and opens
the last with `O_NOFOLLOW`; a race there is not a relay promise, because host
sessions run unconfined (`ssh-hosts.md`).

Rename and move refuse a taken name on the console (`EEXIST`, using
`RENAME_EXCL` so the check and the rename are one step) and replace it on a
host. Delete moves a console entry to the Trash through `NSFileManager` and
deletes on a host permanently. Rename, move and delete refuse a link source.

## Read-only

`files_read_only: true` on a project refuses write, rename, move, delete and
mkdir with `READ_ONLY`. Set it in `settings.json`, with
`PUT /api/projects/{id}`, or with `relay project update --id ID
--files-read-only=true|false`. It only narrows, so it raises no presence
prompt. It does not stop agent sessions, whose reach is their sandbox.

## Audit

Mutations write `file_op` rows (see `audit-log.md`). With auditing on, an
intent row that cannot be written refuses the mutation with `AUDIT_UNAVAILABLE`
and nothing runs. With auditing off, operations run and nothing is recorded.
Before the intent, a mutation is probed for a link with `stat`, so a refused
link produces one `denied` row; the backend enforces the rule at the open
regardless. Reads are never recorded.

## Search and git

Inside a git work tree search reads `git ls-files -co --exclude-standard -z`.
Elsewhere it walks, skipping hidden entries and `node_modules`. Links and
binary files are skipped. Smart case, whole word, globs where `*` crosses `/`,
and `col` and `len` in UTF-16 code units, one-based `line` and `col`.

Git runs with a fixed `-c` prefix, an argument allowlist (`ValidateGitArgs`),
no inherited `GIT_*` variables, and a 10 second limit. A non-zero exit is a
result, not an error.

## Watch

`/ws/files` carries `watch`/`unwatch` from eve and `watch_ok`, `watch_error`,
`fs_event` and `host_status` back. One FSEvents stream serves a project for
every connection and stops with the last unwatch or disconnect. `watch_ok`
is sent only after the stream is started and flushed. Relay compares watched
projects with settings every two seconds and sends `PROJECT_CHANGED` when one
is deleted or its path or host changes. A connection that cannot keep up is
closed; eve reconnects and watches again.

## Host seam

`FileOps.Hosts` takes the host pool (`Backend`, `PasteTmp`, `Subscribe`,
`Statuses`). Without it a host project answers `HOST_UNREACHABLE`.
