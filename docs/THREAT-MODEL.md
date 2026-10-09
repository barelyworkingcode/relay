# Threat model

Who relay defends against, what it protects, and where its job ends.
Planners and reviewers judge findings against this document. A defect
outside it is not a relay bug, however real.

## Assets

- **A. Grants**: what each project, session, service and remote client may
  reach (tools, files, sockets, models).
- **B. Credentials**: tokens, launch identities, client certificates,
  passkeys, model keys.
- **C. The audit trail**: every gated action recorded, unforgeable by the
  actor it records.
- **D. Your data outside a grant**: files, mail, accounts and secrets no
  grant names.

## Attackers, and what relay promises against each

1. **A sandboxed session** (claude, pi, chat, terminal) doing anything its
   code can do. It cannot read or write outside its grants, reach a
   relay-managed service it wasn't granted or any listener that trusts
   loopback in place of a credential, obtain or keep a credential it wasn't
   issued, or act without the audit trail recording it.
   Examples: #53 (a planted symlink widens a later session's read grants),
   #104 (renaming a socket-deny directory to connect).
   The file plane (`docs/project-files.md`) holds the same line for eve's
   file operations: it never follows a symbolic link a session planted in its
   project folder, and every mutation it permits or refuses is in the audit.
2. **An enrolled remote client** (a machine holding a relay certificate). It
   reaches exactly its profile and nothing else, within its budget.
   Revocation ends it, live connections included.
3. **A prompt-injected agent acting within its grants** (any of the above,
   steered by content it read). Relay does not stop it using what it was
   granted. Relay's job is to **limit and record**: grants default narrow,
   anything reaching outside this Mac is refused unless granted, budgets cap
   volume, and every call is in the audit log with its outcome. A grant that
   lets it do more than the operator could reasonably tell from the screen
   is a relay bug (see out-of-scope 1).
3a. **Agent output steering the Chief of Staff.** Agent replies reach eve's
   Chief of Staff as turn excerpts, so an agent steered by content it read can
   write text aimed at the Chief of Staff and, through it, at every other
   agent. Agent output is untrusted data, and relay does not judge it. Relay
   limits and records. A request in the chief-of-staff scope reaches the
   session list, a `/ws` it cannot write to, one send route and one start
   route: no tool call, file read, terminal of its own choosing, permission
   answer, session stop or resume, and no config change. The start route
   launches an agent in a registered project, local or on an SSH host, in
   the project root or a folder inside it (symlinks resolved for a local
   project; a hosted project's folder is checked as text, as for any hosted
   launch). A remote (mTLS) project is refused, and so is a terminal start on
   a host. It goes through `AuthorizeLaunch`, so
   the project's policy, allowed models and templates and the sandbox apply.
   A headless agent runs `bypassPermissions` as every headless agent does, so
   its reach is what its sandbox and project grant allow. On an SSH host it
   runs as the host account with no sandbox, like every host session. The session carries
   the `chief-of-staff` origin and a `session_launch` row with `origin` and
   `prompt_bytes` (and `host_id` for a hosted project) is written. A start is refused when auditing is off or not wired; a headless
   start ends when its durable `session_message` intent row cannot be written.
   Each send goes to one listed session, at
   most one per turn (mid-turn the answer is `already_processing`), carries
   the `chief-of-staff` origin in the transcript, and has a `session_message`
   record written before delivery; a send that cannot be recorded is refused.
   What the receiving agent then does is its own grant (out-of-scope 4).
   Relay does not promise that a frontend routes its Chief of Staff messages
   through the scope, or that it never marks a message the person typed: the
   mark records which door a frontend chose.
   The scope also can't change where the Chief of Staff runs: the
   `/api/chief-of-staff/config` routes are refused inside it.
3b. **A read-only-projects session** (`settings.readOnlyProjects`, claude
   only, started by a frontend that holds execute). It reads every registered
   local project, and that read reach is new: a session otherwise reads only
   its own project. Relay promises it writes none of them and reads nothing
   outside the project folders, the template's grants and the paths the
   Claude CLI needs. The sandbox profile is the enforcement; the tool list is
   not. A caller that can start sessions can start one with this option, so
   the option widens read reach for that caller only as far as the projects
   already registered. The set of readable projects is fixed when the
   profile is written, and a project change ends the live sessions that hold
   one.
4. **A peer on the network or in the browser** (a machine that can reach a
   relay listener, or a web page open on this Mac reaching relay's localhost
   ports). It gets nothing without a credential: no tool call, no config
   change, no enrolment without the operator's approval on this Mac, no
   prompt it can trigger on the operator's screen.
5. **Other code running as the user, outside relay.** It can do anything the
   user can do without relay. It cannot pass relay's gates: it can't borrow a
   service's launch identity, act through the bridge as another project, or
   complete a presence-gated action without the operator.
   Example: #146 (a stale presence prompt completing for a caller that had
   gone).

The audit trail (asset C) is part of every promise above. Examples: #147 (a
cancelled presence prompt left no audit row), #150 (caller-controlled
terminal escapes in `relay audit`'s table).

## The test build

`./build.sh --test-build` makes a tray with three seams that take the outside
world away from a test: a presence provider that reads its answers from a file
in the config dir, a sealing-key store in a file in the config dir instead of
the login keychain, and a clock the test moves (`relay debug clock`). It
exists so verify journeys and the e2e tier run with no person and no login
session. It is not a promise relay makes to a user: no release carries it, and
a release has no switch that skips a gate, reads a file for a presence answer
or moves its clock (attacker 5's promise holds). The promises that cover it:

- **Absent from release (assets A, B).** `build.sh` and a CI step on every pull
  request check that no release binary holds a seam symbol or the tag. A
  release that holds one is a relay bug against attacker 5.
- **No seam on the real config dir (assets A, B).** The seams act only on a
  config dir other than the default one. On the default dir the test build
  approves `project.grant` alone, refuses every other gated op, uses the login
  keychain and refuses to move the clock, so a test build swapped in for the
  real app never takes an answer from a file.
- **What the approver can approve.** In the test build the approver may
  approve any op in `presence.GatedOps`, not only `project.grant`. The outcome
  file chooses the answer per op. It is read only on a config dir other than
  the default one, and it is absent from release builds, so it widens nothing
  a release or the real app does.
- **Recorded (asset C).** Each approval is a `control_decision` row with
  `presence_approver`, written before the act; an unrecordable approval
  refuses the act. Each answer, keychain fault and clock change writes a
  `debug.*` event.

One residual: a session whose grants include the test config dir could write
the files in it. The harness keeps that dir under `/tmp`, outside every grant.

## Out of scope: not a relay bug

1. **Grants the operator chose knowingly**, e.g. granting a broad folder,
   then a session using it. Misleading defaults, and screens that lead the
   operator into a broader grant than they meant, stay in scope. Example of
   one that stays in scope: #1 (sessions could read credential directories
   such as `~/.ssh` by default).
2. **Root, or a compromised OS.** Relay trusts the kernel, the keychain and
   code signing.
3. **A session degrading only its own experience**: killing its own
   provider, filling its own workspace.
4. **Damage an agent does within its grant**, beyond limiting and recording
   it (attacker 3).

Other loopback ports are reachable by design, since sessions run and test
their own servers. Reaching one is not a finding unless it breaks a promise
above. Example: #105 (a second relay instance's listeners, closed as out of
scope).

## How findings are judged

- A finding names the attacker, the asset, and the promise broken. One that
  can't name all three is out of scope.
- Races relay's own launch path must win (microsecond windows, one try per
  launch) are theoretical until reproduced. Examples: #57, #110.
- A read-only-projects session that writes to a project folder, or reads a
  path no project, template grant or CLI need names, or a profile that
  outlives a project change, is a relay bug against asset D.
- A request in the chief-of-staff scope that reaches anything outside its
  four doors, a session start outside the project root or in a remote
  (mTLS) project, a message marked `chief-of-staff` that did not come through
  the scope, or a scoped send with no intent record, is a relay bug against
  asset A or C.
