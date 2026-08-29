# Sealed config

`settings.json` holds two different kinds of value: things relay hands out,
and things relay only checks. This document is the durable reasoning behind
moving the first kind off disk and into an AES-256-GCM envelope keyed from the
login keychain, and behind the choices that follow from it. See
[ADR-017](decisions/017-config-dir-is-not-a-boundary.md) for the decision and
[the implementation spec](decisions/017-implementation-spec.md) for the wire
format and acceptance criteria; this document is the *why* that a comment in
`settings_seal.go` or `sealed/sealed.go` is not the place for.

## What is sealed, and the rule that decides

`0600` on `settings.json` keeps other users out. It keeps nothing out that
runs as the file's owner — and the thing that now runs as the owner,
routinely, is an LLM agent the owner started themselves, holding a project
token, from their own shell. Encrypting the whole file would defend against
that caller, but it would also turn every operator surface that reads
`settings.json` today into a surface that needs a key relay might not be able
to produce. The rule that resolves the tension falls out of how relay already
authenticates: **authentication needs only the hash; only brokering needs the
plaintext.** `AuthenticateProject` finds a project by `token_hash`. A control-
plane credential is resolved by `hash`. A passkey is verified against its
public key coordinates, never a private key relay would have to hold. None of
that needs a plaintext in memory, let alone on disk.

So the line is: **what relay would have to hand to somebody is sealed; a
verifier relay only ever compares against is not.**

**Sealed** — every one of these is a bearer relay presents to some other
party, or a value it injects into a process it spawns:

- `projects[].token` — the plaintext project token relay injects into every
  child it spawns for that project.
- `admin_secret` — the plaintext bearer the bridge compares against for
  `ReconcileExternalMcps` / `ReloadExternalMcp` / `ReloadService`.
- `external_mcps[].oauth_state.{access_token,refresh_token,client_secret}` —
  bearers relay presents upstream, or that mint one indefinitely.
- `external_mcps[].env` and `services[].env` **values** — injected verbatim
  into the child relay spawns, which is the definition of the sealed set
  applied to an env var.
- `ca.key` (a separate file, `ca.key.sealed`) — signs every client
  certificate relay issues.

**Clear** — every one of these is something relay only ever compares against,
or structure an operator needs to read to understand what is configured:
`token_hash`, every credential's `hash`, `login_bootstrap.hash`, a passkey's
public key coordinates, an enrolment's fingerprint, every project name and
path, every MCP's command and argv, `allowed_mcp_ids`, `allowed_tools`,
`access`, the resource scope, `allow_cwd_auth`, env **keys** (as opposed to
their values), the audit and remote blocks.

## The trap the field list exists to avoid

A project token is 32 random bytes, hex-encoded: 64 hex characters. Its
`token_hash` is a SHA-256 digest, hex-encoded: also 64 hex characters. Look at
either one and there is nothing to distinguish it from the other — no prefix,
no length difference, nothing. A rule like "leave 64-hex values alone, they
look like hashes" seals nothing and leaves every project token sitting in the
clear next to a hash that was never the secret. A rule like "seal anything
named `*_token`" seals the field named `token` and misses `token_hash`
entirely, or worse, seals the verifier and leaves the plaintext untouched.

The only rule that survives contact with that pair is an **explicit field
list**, enumerated once (`forEachSecret` in `settings_secret.go`) and checked
by a reflection-based test that walks every `Secret`-typed field reachable
from `*Settings` and fails if `forEachSecret` misses one. Nothing about a
field's name, its length, or what it looks like decides whether it is sealed.
Only its presence on the list does.

The same trap recurs at load time, in the other direction: a project's
sealed `token`, once opened, must actually be the preimage of its own
`token_hash`. If it is not, something is wrong — a bad key, a tampered file,
a bug in the sealing layer — and relay has no business handing that value to
anything, since it will not authenticate anyway. This is why migration
asserts `sha256(Reveal(token)) == token_hash` for every project *before*
writing anything, and why the same check on every subsequent load closes a
project whose token fails it (`verifyProjectTokenHashes` in
`settings_seal.go`) rather than serving a value that might be wrong in a way
nobody could see by eye.

## Env values are sealed; env keys are not

`external_mcps[].env` and `services[].env` get the same treatment as every
other value relay hands out — because injecting an env var into a spawned
child is exactly that. But the **keys** stay in the clear, deliberately, and
uniformly: every value is sealed, including one as unremarkable as
`LOG_LEVEL=debug`, and no heuristic sealing only values whose key "looks
credential-ish" exists or should exist — a rule that reads names is a rule an
attacker gets to choose the name to defeat.

The reason the keys stay clear is the same reason the whole file is not
encrypted, applied at a smaller scale: an operator reading `settings.json`,
or hand-editing it, still sees *which* variables an MCP or a service
receives. Only the contents are opaque. A hand-edit can still add, rename, or
remove a variable by name; it just cannot read or write its value without the
running service.

## Why sealing happens before serialisation, not to the file afterwards

`FileSettingsStore.save` seals every `Secret` in memory, then marshals the
result, then hands the resulting bytes to `atomicWriteFile` — which stages
through a uniquely-named `settings.json.*.tmp` in the same directory before
renaming it into place. That ordering is the whole of the residue rule: by
the time any byte reaches the staging file, it has already been through
`sealAllSecrets`, so the staging file never holds plaintext at any point in
its life. A process killed between the temp file's creation and its rename
leaves a *sealed* file behind, not a plaintext one.

The alternative — writing the temp file as before and then encrypting the
final result, or sweeping/shredding the temp file afterward — was rejected
for the same reason `atomicWriteFile` itself is not touched by this design at
all: it is machinery bolted onto the wrong layer. A file that never contained
plaintext needs no shredding. `Secret.MarshalJSON` returning an error for a
value that has not been through `sealAllSecrets` is what makes this
enforceable rather than aspirational: every residue path in the program —
`json.Marshal(settings)`, a stray `slog` call carrying a `Settings`, a future
export route nobody has written yet — hits that error before it reaches disk,
turning a silent leak into a loud test failure the first time it is
exercised.

Every write re-seals every sealed field, unconditionally, from the plaintext
each `Secret` currently holds in memory — not because a fresh nonce is
otherwise unsafe (`Seal` generates a random 96-bit nonce on every call, so
reuse is unreachable by construction, not merely avoided by discipline), but
because the alternative is a branch that decides "reseal this one" versus
"carry the old envelope forward," and that branch is exactly where a
plaintext leak would live. There is no such branch to get wrong.

## Why the whole file is not encrypted

Three things a hand-editable, structurally-readable `settings.json` buys, and
what each one costs to keep:

- **`relay grant` works with the tray stopped.** It is the operator's *what
  did I actually grant?* surface — every project's MCPs, mode, tools, and the
  real scope values, including a scope reaching a filesystem root. It builds
  a `StoredToken` purely from the clear fields the permission-derivation
  logic already reads, and touches no plaintext at all. Encrypt the whole
  file and this command becomes a client of the thing it exists to audit —
  it would need the same key the thing it audits needs, at which point an
  operator locked out of one is locked out of both.
- **`relay audit` stays ground truth.** It reads the tool-call log, not
  settings, and is unaffected either way — but it is the surface `CLAUDE.md`
  names as authoritative for anything relay gates, and ground truth that
  needs a key to consult is a weaker kind of ground truth.
- **A hand-edit stays a real recovery path.** A human can open
  `settings.json`, see a project's MCPs and paths, and reason about what is
  wrong with it. An opaque blob turns "the file is corrupt" into "the file is
  corrupt, or the key is wrong, or the keychain is locked," with nothing on
  the outside to tell those apart.

What this costs, stated rather than hidden: **the structure leaks.** Project
names and paths, every MCP's command line and env *keys* (never their
values), enrolment client ids and their grants, service commands, the real
scope values. Anyone who can read the file learns the shape of the machine
and every place worth attacking next. That is a disclosure cost, and this
design is about escalation, not disclosure — the trade is taken on purpose,
and it is the first thing to revisit if disclosure ever turns out to matter
more than it looks like it does today.

## Why a missing key is never replaced

The keychain ACL (below) protects *reading* the key. It does nothing to stop
the key from being *deleted* — any process running as the machine's owner can
remove a login-keychain item it does not own the ACL for the same way it can
remove any file it owns. That is conceded: a deleted key is denial of
service, and denial of service is not what this design defends against.

What it must never become is **substitution**. If relay's answer to "the key
I expected is gone" were to mint a fresh one and carry on, an attacker could
delete relay's key, plant a key of their own under the same keychain item,
and let relay re-seal every project token, every OAuth bearer, and `ca.key`
under a key the attacker holds. That turns a denial-of-service move into a
full compromise of everything sealed, with relay's own code doing the
re-sealing.

Two things close that door, both already required by the key-id design:

1. A missing keychain item, or one whose id does not match what
   `settings.json` names in `sealed_key_id`, is a loud, named refusal — never
   a reason to call `Keyring.Create`. `resolveSealer` in `sealed_config.go`
   only ever creates a key on a genuine first run (no `sealed_key_id` at
   all); every other branch that finds a mismatch returns a degraded reason
   and creates nothing.
2. The key id itself is folded into every envelope's associated data
   (`aesSealer.bind`), ahead of the field path. An envelope's `key` field
   cannot be edited independently of its ciphertext — doing so changes what
   GCM authenticated at seal time, and the tag stops verifying even against a
   sealer that reports the same id but does not hold the identical key bytes.

The only way a fresh key is ever minted after first run is the break-glass
reset below, and that path is reachable only after the operator has just
typed their login password to authorize exactly that act.

## The keychain ACL, and why it is deliberately a deprecated API

The modern way to gate keychain access on a per-caller basis is
`kSecAttrAccessControl` with `kSecAccessControlUserPresence`. It is not
usable here: every `SecItemAdd` carrying it measures `-34018`
(`errSecMissingEntitlement`), with or without
`kSecUseDataProtectionKeychain`. Getting past that needs the
`keychain-access-groups` entitlement, which is restricted — signing with it
under every identity available on this machine produced SIGKILL at exec
(`Taskgated Invalid Signature`), because it needs a portal-issued
provisioning profile relay does not have and has no path to acquiring.

The mechanism actually in use is the legacy one:
`SecTrustedApplicationCreateFromPath` to name a trusted code identity,
`SecAccessCreate` to build an ACL naming it, and `kSecAttrAccess` on the
keychain item at `SecItemAdd` time. It has been deprecated since macOS 10.10.
It also works, measured on this machine's OS version, and there is no
supported replacement that a relay-shaped application can use today. This is
a **deliberate deprecated-API bet**, not an oversight and not a placeholder
for a future rewrite — if it is ever withdrawn, the fallback is the same
keychain item with no trusted-application list at all (below), not a
different API.

**The ACL binds to code identity, not to a path.**
`SecTrustedApplicationCreateFromPath` reads the code identity at the path it
is given *once, at seal time* — team identifier plus bundle identifier — and
the resulting ACL matches anything carrying that identity, anywhere on disk,
signed by the same identity, forever after. Concretely: the installed bundle
copied elsewhere still unlocks silently; a later build signed by the same
Developer ID identity still unlocks silently, with no re-registration needed;
a re-signed or altered copy gets a hard refusal
(`errSecInteractionNotAllowed`) where no session can prompt, or the
keychain's own consent dialog where one can.

That last property — a modified relay is a different identity, and a
different identity fails toward a prompt or a refusal, never toward a silent
unlock — is what makes this survive real operation rather than becoming a
maintenance burden. **The installed bundle here is Developer ID signed**, so
this is the steady state on this machine: every rebuild by the same
Developer ID identity keeps the same grant, with no re-prompting and no ACL
maintenance. A locally rebuilt, unsigned or ad-hoc-signed binary is a
*different* identity to the keychain — its designated requirement is a bare
hash of that exact binary rather than an identifier-plus-anchor — so it
re-prompts on the next unlock. That is a development-time inconvenience with
no security content of its own, and the temptation it invites is to loosen
the ACL to make the dev loop quieter; nothing in this design does that, and
nothing should.

**Note for this VM specifically: SIP is disabled here.** Any property that
ultimately rests on the OS enforcing code-signature checks is measurably
weaker on a machine where that enforcement itself can be bypassed. The ACL's
identity binding is real and was measured working as described, but a
SIP-disabled box is not the environment to treat that measurement as a
worst-case bound — re-measure on a production Mac before leaning on it there.

## The CLI carries relay's own code identity — this is structural, not incidental

`/Applications/Relay.app/Contents/MacOS/relay credential mint` runs the exact
same binary, with the exact same code identity, as the tray. That means the
keychain ACL cannot tell the CLI from the tray by identity alone — a CLI
process asking the keychain directly would unlock silently, for precisely
the caller this whole design exists to keep out.

The fix is not a check inside the CLI that refuses to ask. It is that the CLI
**cannot reach the keychain at all**: the keychain keyring is constructed at
exactly one call site in the whole program, on the tray's own startup path in
`runTrayApp`, and every CLI entry point holds only a store built with a nil
sealer — one that reveals nothing and refuses every write
(`errSealerRequired`). A `go/ast` call-graph test walks every CLI entry point
and fails, naming the entry point and the call chain, if any of them can
reach `sealed.NewKeychainKeyring`, `Sealer.Unseal`, or `Secret.Reveal`. This
is the same shape as ADR-015's "make the route unreachable, don't check and
refuse" argument, applied to a keyring instead of an HTTP mux.

## Why relay stopped writing `settings.json` from CLI processes

Every mutating CLI command — minting a credential, creating an enrolment,
registering an MCP, rotating a project token — now asks the running tray
over the bridge socket instead of writing the file itself. This is the same
brokering pattern ADR-007 already applies to project tokens (relay is the
sole broker; nobody else holds one), turned inward on relay's own
control-plane secrets. It falls directly out of the ACL argument above:
sealing only means something if the key has exactly one holder, and the tray
is that holder. A CLI that could still write the file would either need the
key itself (reintroducing door two: a second process that can unlock) or
would have to leave every sealed field untouched on every CLI-driven write,
which is not a real option once minting a credential or rotating a token
*is* writing a sealed field.

The cost is real and is stated as a cost, not hidden: every mutating command
now requires relay running, and says so by its own name rather than failing
with a generic bridge or settings error. The read half — `relay audit`,
`relay grant`, and every `list` subcommand — needs nothing sealed and keeps
working exactly as before, with the tray stopped.

## Three degraded states, and why relay starts anyway

A key-id mismatch, a missing key, a tampered envelope, and a token that fails
its `token_hash` check are four distinct conditions, and each is named
distinctly rather than folded into one generic "cannot read settings"
message — the whole point of field-level sealing is that "the file is
corrupt" and "the key is wrong" stay distinguishable from the outside.

In every one of those states, relay **starts**. This is a deliberate
departure from the existing rule that the tray exits over a genuinely
unreadable settings file, and the two situations are opposites rather than
variations on a theme: an unreadable file has *unknown* contents, and writing
over it would be catastrophic. A key-mismatched sealed store has *known*
contents relay can still read and show — every clear field, every project
name and path, the whole shape of the machine — it just cannot open a
specific, named set of values. Exiting would hand the operator a machine
whose only recovery surface is the process that refuses to run.

So the degraded state is: the read half works in full (`relay audit`,
`relay grant`, every `list`, the Settings window rendering sealed values as
unavailable), every write refuses without touching the file, and every
operation that needs a sealed value refuses by name — no plaintext token to
hand `ResolvePtyEnv`, no `admin_secret` to compare against, the remote
listener closed rather than left serving with nothing to authenticate
against.

## Break-glass, and why there is no offline recovery code

The one way out of a degraded sealed store is the tray's own **Reset Sealed
Store…** menu item. Behind a presence prompt naming exactly what is about to
be destroyed — every project and its token, every control-plane credential,
every enrolment and the CA that signed them, every passkey — confirming it
deletes `settings.json`, `ca.key.sealed`, `ca.crt`, and the keychain item
together, then re-initializes relay from nothing.

There is no CLI reset subcommand, no `--force-reset` flag, no environment
variable, and no offline recovery code of any kind. Every one of those would
be a second door into the sealed store, and a second door is exactly what
this whole design spends its effort closing on the first one. An operator
whose Mac is headless and whose key is gone has the same answer everyone
always had: delete `settings.json` at the shell and start over — which was
always available, costs nothing new, and was never a door relay built on
purpose.

## What this does not do

An agent that already holds a project token keeps every tool that grant
carries, for as long as the grant lasts, with no prompt — sealing secrets at
rest says nothing about a grant already issued, and was never meant to.
Revocation is gated; `rm settings.json` is not, and remains the documented
way to lock the control plane out entirely. Sealing closes the door where an
agent reads a plaintext off disk and turns it into a fresh credential; it
does not narrow, shorten, or supervise anything a credential already grants.
